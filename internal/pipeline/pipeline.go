// Package pipeline is the per-request chain between the front ends and the
// providers: auth → route → rate limits → budget reservation → cache
// lookup → provider → settlement (usage to dollars, token-limit and budget
// correction, metrics) → cache fill → log. Hedging slots in between in a
// later milestone without the front ends changing.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/hedge"
	"github.com/TheAsianFish/switchboard/internal/router"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Route sends a client-facing model name to a provider.
type Route struct {
	// Model is the name clients use.
	Model string
	// ProviderName is the configured provider name, for logs.
	ProviderName string
	// Upstream is the provider's own model name.
	Upstream string
	Provider core.Provider
	// Price turns the model's usage into dollars; Priced is false for a
	// model with no known price, whose spend is not counted.
	Price  cost.Price
	Priced bool
	// OutputEstimate is the output assumed for a request without
	// max_tokens when it is admitted.
	OutputEstimate int
}

// Pipeline implements core.Gateway for `switchboard serve`.
type Pipeline struct {
	keys   *auth.Keys
	routes map[string]Route
	names  []string
	policy Policy
	now    func() time.Time
}

// AutoModel is the model name that asks the router to choose.
const AutoModel = "auto"

// New builds a pipeline over a key table and routes, enforcing pol. With
// a router in pol, AutoModel is one of the gateway's models.
func New(keys *auth.Keys, routes []Route, pol Policy) *Pipeline {
	p := &Pipeline{keys: keys, routes: make(map[string]Route, len(routes)), policy: pol, now: time.Now}
	for _, r := range routes {
		p.routes[r.Model] = r
		p.names = append(p.names, r.Model)
	}
	if pol.Router != nil {
		p.names = append(p.names, AutoModel)
	}
	sort.Strings(p.names)
	return p
}

var _ core.Gateway = (*Pipeline)(nil)

// Authenticate implements core.Gateway.
func (p *Pipeline) Authenticate(ctx context.Context, apiKey string) (context.Context, error) {
	// The span ends here: the returned context carries the caller's span,
	// not this one, so the later stages are its siblings.
	_, span := telemetry.StartSpan(ctx, "auth")
	pr, err := p.keys.Lookup(apiKey)
	if err != nil {
		telemetry.EndSpan(span, err)
		return ctx, err
	}
	span.SetAttributes(attribute.String("switchboard.key", pr.Name))
	span.End()
	ctx = auth.WithPrincipal(ctx, pr)
	return telemetry.WithAttrs(ctx, "key", pr.Name), nil
}

// Models implements core.Gateway: the models the caller's key may use.
func (p *Pipeline) Models(ctx context.Context) []string {
	pr := auth.FromContext(ctx)
	out := make([]string, 0, len(p.names))
	for _, n := range p.names {
		if pr != nil && pr.Allows(n) {
			out = append(out, n)
		}
	}
	return out
}

// resolve checks the caller may use the requested model and returns the
// route and a copy of the request addressed to the upstream model. For an
// auto request the returned context's log lines say so, since every
// later line names the model the router chose.
func (p *Pipeline) resolve(ctx context.Context, req *core.Request) (context.Context, Route, *core.Request, *admission, error) {
	// The router and the semantic cache may both embed the last user
	// turn; with one memo per request they share the vector.
	ctx = embed.WithMemo(ctx)
	pr := auth.FromContext(ctx)
	rt, dec, rd, err := p.route(ctx, pr, req)
	if err != nil {
		p.observeRefused(ctx, pr, req, rt, rd, err)
		return ctx, Route{}, nil, nil, err
	}
	if req.Model != rt.Model {
		ctx = telemetry.WithAttrs(ctx, "requested_model", req.Model)
	}
	adm, err := p.admit(ctx, pr, rt, req)
	if err != nil {
		switch core.AsError(err).Kind {
		case core.KindRateLimited:
			p.policy.Metrics.Call(pr.Name, rt.Model, telemetry.OutcomeRateLimited)
		case core.KindBudgetExceeded:
			p.policy.Metrics.Call(pr.Name, rt.Model, telemetry.OutcomeBudgetExhausted)
		default:
			p.policy.Metrics.Call(pr.Name, rt.Model, telemetry.OutcomeError)
		}
		p.observeRefused(ctx, pr, req, rt, rd, err)
		return ctx, Route{}, nil, nil, err
	}
	adm.cache, adm.decision = dec, rd
	up := *req
	up.Model = rt.Upstream
	if up.MaxTokens == 0 && adm.charged() {
		// The admission assumed this much output; sending it as the cap
		// keeps the real cost within the reservation instead of leaving
		// it to the upstream's own default, or to no cap at all.
		up.MaxTokens = adm.estimate.OutputTokens
	}
	return ctx, rt, &up, adm, nil
}

// route finds the request's route, checks the key may use it and decides
// the cache mode, in the "route" span.
func (p *Pipeline) route(ctx context.Context, pr *auth.Principal, req *core.Request) (rt Route, dec cache.Decision, rd *router.Decision, err error) {
	_, span := telemetry.StartSpan(ctx, "route", attribute.String("switchboard.model", req.Model))
	defer func() {
		if err == nil {
			span.SetAttributes(attribute.String("switchboard.provider", rt.ProviderName), attribute.String("switchboard.upstream_model", rt.Upstream))
		}
		telemetry.EndSpan(span, err)
	}()
	if pr == nil {
		return Route{}, dec, nil, core.Errorf(core.KindAuthentication, "missing API key")
	}
	rt, ok := p.routes[req.Model]
	if !ok && (req.Model != AutoModel || p.policy.Router == nil) {
		return Route{}, dec, nil, core.Errorf(core.KindNotFound, "model %q is not configured on this gateway", req.Model)
	}
	if !pr.Allows(req.Model) {
		return Route{}, dec, nil, core.Errorf(core.KindPermission, "this API key may not use model %q", req.Model)
	}
	if req.Model == AutoModel && p.policy.Router != nil {
		if rt, rd, err = p.choose(ctx, pr, req); err != nil {
			return Route{}, dec, nil, err
		}
	}
	if p.policy.Cache != nil {
		// Decided before admission so that a malformed directive is
		// refused without charging the key's limits.
		d, err := cache.Decide(pr.Cache, cache.ReportFrom(ctx).Directive(), req, p.policy.CacheMaxTemperature)
		if err != nil {
			p.policy.Metrics.Call(pr.Name, rt.Model, telemetry.OutcomeError)
			return rt, dec, rd, err
		}
		dec = d
	}
	return rt, dec, rd, nil
}

// choose runs the router for a request that asked for AutoModel, in the
// "router" span. The router picks only among candidates the key may use
// on their own, so a key's model list caps what auto can spend; a key
// with no list may use them all. The router never fails a request over
// its own trouble: when the prompt cannot be embedded it sends the
// request to its fallback and the cause is logged.
func (p *Pipeline) choose(ctx context.Context, pr *auth.Principal, req *core.Request) (Route, *router.Decision, error) {
	_, span := telemetry.StartSpan(ctx, "router")
	d, err := p.policy.Router.Route(ctx, req, func(m string) bool { _, ok := p.routes[m]; return ok && pr.Allows(m) })
	if err != nil {
		err = core.Wrap(core.KindPermission, err, "this API key may not use any of the models auto chooses between")
		telemetry.EndSpan(span, err)
		return Route{}, nil, err
	}
	span.SetAttributes(
		attribute.String("switchboard.router.model", d.Model),
		attribute.String("switchboard.router.reason", d.Reason),
		attribute.Float64("switchboard.router.target", p.policy.Router.Target()),
	)
	for i, c := range p.policy.Router.Weights().Candidates {
		if i < len(d.Probs) {
			span.SetAttributes(attribute.Float64("switchboard.router.p."+c.Model, d.Probs[i]))
		}
	}
	span.End()
	p.policy.Metrics.Routed(d.Model, d.Reason)
	if d.Err != nil {
		telemetry.Logger(ctx).Warn("router could not predict; using its fallback",
			slog.String("model", d.Model), slog.String("error", d.Err.Error()))
	}
	return p.routes[d.Model], &d, nil
}

// Complete implements core.Gateway.
func (p *Pipeline) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	ctx, rt, up, adm, err := p.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	ctx, lk := p.lookup(ctx, adm, req, up)
	if lk.Response != nil {
		resp := lk.Response
		resp.Model = rt.Model
		p.serveHit(ctx, adm, req, lk, resp, false)
		return resp, nil
	}
	ctx, _ = hedge.WithReport(ctx)
	start := p.now()
	pctx, span := startProvider(ctx, rt, false)
	resp, err := rt.Provider.Complete(pctx, up)
	elapsed := p.now().Sub(start)
	if err != nil {
		err = upstreamError(rt, err)
		u, outcome := abandonedUsage(ctx, adm)
		endProvider(span, 0, core.Usage{}, outcome, err)
		u, spent := p.settle(ctx, adm, u, 0, false, outcome)
		logCall(ctx, rt, false, elapsed, 0, core.Usage{}, err)
		p.observeCall(ctx, adm, req, lk, false, elapsed, 0, u, spent, outcomeErr(outcome, err))
		return nil, err
	}
	// The route's name, not the request's: for auto, the client learns
	// which model answered.
	resp.Model = rt.Model
	endProvider(span, 0, resp.Usage, telemetry.OutcomeOK, nil)
	u, spent := p.settle(ctx, adm, resp.Usage, len(resp.Text), false, telemetry.OutcomeOK)
	logCall(ctx, rt, false, elapsed, elapsed, resp.Usage, nil)
	p.observeCall(ctx, adm, req, lk, false, elapsed, 0, u, spent, nil)
	if lk.Fillable() {
		p.policy.Cache.Fill(ctx, lk, resp)
	}
	return resp, nil
}

// lookup consults the response cache and records what it found for the
// response headers and the log. The key is the client's request with the
// output cap that was really sent upstream: a reply the gateway capped at
// a key's max_output_tokens must not answer a request that had no cap.
func (p *Pipeline) lookup(ctx context.Context, adm *admission, req, up *core.Request) (context.Context, *cache.Lookup) {
	sctx, span := telemetry.StartSpan(ctx, "cache.lookup")
	var lk *cache.Lookup
	defer func() {
		span.SetAttributes(attribute.String("switchboard.cache.status", string(lk.Status)))
		if lk.Status == cache.StatusSemantic {
			span.SetAttributes(attribute.Float64("switchboard.cache.similarity", lk.Similarity))
		}
		span.End()
	}()
	switch {
	case p.policy.Cache == nil:
		lk = cache.Skip("no cache")
		return ctx, lk
	case adm.cache.Mode == cache.ModeOff:
		lk = cache.Skip(adm.cache.Reason)
	default:
		keyed := *req
		keyed.MaxTokens = up.MaxTokens
		// An auto request is cached as a request to the model that
		// answered it, so it shares entries with direct requests there.
		keyed.Model = adm.route.Model
		route := cache.Upstream{Provider: adm.route.ProviderName, Model: adm.route.Upstream}
		lk = p.policy.Cache.Lookup(sctx, adm.pr.Tenant, route, adm.cache.Mode, &keyed)
	}
	cache.ReportFrom(ctx).Set(lk)
	return telemetry.WithAttrs(ctx, "cache", string(lk.Status)), lk
}

// serveHit settles a request answered from the cache. It costs nothing:
// the token-limit charge and the budget reservation are returned in full.
// The request still counted against the request limit, which bounds how
// often a key may ask, whatever answers.
func (p *Pipeline) serveHit(ctx context.Context, adm *admission, req *core.Request, lk *cache.Lookup, resp *core.Response, stream bool) {
	p.settle(ctx, adm, core.Usage{}, 0, stream, telemetry.OutcomeCacheHit)
	var saved cost.Nanos
	if adm.route.Priced {
		saved = adm.route.Price.Of(resp.Usage)
		p.policy.Metrics.CacheSaved(adm.pr.Name, adm.route.Model, saved.USD())
	}
	total := p.now().Sub(adm.start)
	logCall(ctx, adm.route, stream, total, 0, core.Usage{}, nil)
	if p.policy.OnCall != nil {
		c := callOf(adm, req, stream, lk)
		c.Outcome, c.Duration, c.Saved = telemetry.OutcomeCacheHit, total, saved
		p.observe(ctx, c)
	}
}

// observeCall reports an admitted call that reached the provider.
func (p *Pipeline) observeCall(ctx context.Context, adm *admission, req *core.Request, lk *cache.Lookup, stream bool, total, ttft time.Duration, u core.Usage, spent cost.Nanos, err error) {
	if p.policy.OnCall == nil {
		return
	}
	c := callOf(adm, req, stream, lk)
	c.Outcome, c.ErrorKind = outcomeOf(err)
	c.Duration, c.TTFT, c.Usage, c.Spent = total, ttft, u, spent
	p.observe(ctx, c)
}

// outcomeErr is the error observeCall classifies: a call the client
// abandoned is reported as abandoned whatever error it ended with.
func outcomeErr(outcome string, err error) error {
	if outcome == telemetry.OutcomeAbandoned {
		return errStreamAbandoned
	}
	return err
}

// Stream implements core.Gateway.
func (p *Pipeline) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	ctx, rt, up, adm, err := p.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	ctx, lk := p.lookup(ctx, adm, req, up)
	if lk.Response != nil {
		// Replayed as canonical events, so the front end renders it in
		// the client's own wire format whichever format filled it.
		resp := lk.Response
		resp.Model = rt.Model
		p.serveHit(ctx, adm, req, lk, resp, true)
		return &core.SliceStream{Events: core.ResponseEvents(resp, cache.Chunks(resp.Text))}, nil
	}
	ctx, _ = hedge.WithReport(ctx)
	start := p.now()
	// The provider span runs until the stream ends, in finish; the
	// provider (and any retry or hedge wrapper around it) gets it as its
	// parent.
	pctx, span := startProvider(ctx, rt, true)
	s, err := rt.Provider.Stream(pctx, up)
	if err != nil {
		err = upstreamError(rt, err)
		u, outcome := abandonedUsage(ctx, adm)
		endProvider(span, 0, core.Usage{}, outcome, err)
		u, spent := p.settle(ctx, adm, u, 0, true, outcome)
		total := p.now().Sub(start)
		logCall(ctx, rt, true, total, 0, core.Usage{}, err)
		p.observeCall(ctx, adm, req, lk, true, total, 0, u, spent, outcomeErr(outcome, err))
		return nil, err
	}
	return &stream{inner: s, ctx: ctx, span: span, p: p, adm: adm, req: req, route: rt, model: rt.Model, start: start, now: p.now, lookup: lk}, nil
}

// abandonedUsage is what a call that failed before any output is charged.
// An upstream failure is charged nothing. When the client went away
// mid-call instead, the upstream may have read the whole prompt and billed
// for it, and nothing here can tell, so the prompt estimate is charged: an
// over-count rather than a cost the ledger never sees. When the resilience
// wrapper counted the abandoned attempts as cancelled, settle charges each
// of them, so nothing is added here.
func abandonedUsage(ctx context.Context, adm *admission) (core.Usage, string) {
	if ctx.Err() == nil {
		return core.Usage{}, telemetry.OutcomeError
	}
	var u core.Usage
	if hedge.ReportFrom(ctx).Snapshot().Cancelled == 0 {
		u.InputTokens = adm.estimate.InputTokens
	}
	return u, telemetry.OutcomeAbandoned
}

// upstreamError keeps a provider's refusal of the gateway's own credentials
// from looking like a problem with the client's key. An upstream 401 or 403
// reaching the client as 401 would send them debugging a key that works,
// and the upstream's message can quote part of the gateway's key; the
// detail stays in the cause for the log.
func upstreamError(rt Route, err error) error {
	e := core.AsError(err)
	if e.Kind != core.KindAuthentication && e.Kind != core.KindPermission {
		return err
	}
	return core.Wrap(core.KindUpstream, err, fmt.Sprintf("provider %q rejected the gateway's credentials", rt.ProviderName))
}

// stream rewrites the model name back to the client's, and settles and
// logs the call once, when it ends for whatever reason.
type stream struct {
	inner core.Stream
	ctx   context.Context
	// span is the provider span, ended with the stream.
	span  trace.Span
	p     *Pipeline
	adm   *admission
	req   *core.Request
	route Route
	model string
	start time.Time
	now   func() time.Time

	// lookup is the cache lookup; on a miss the stream's text is
	// collected so a complete reply can be stored.
	lookup *cache.Lookup

	// mu guards ttft, usage, chars, text and stop: Close may run on
	// another goroutine than Next, for example to abort a blocked read.
	mu     sync.Mutex
	ttft   time.Duration
	usage  core.Usage
	chars  int
	text   strings.Builder
	stop   core.StopReason
	logged sync.Once
}

func (s *stream) Next() (core.Event, error) {
	ev, err := s.inner.Next()
	if err != nil {
		switch {
		case errors.Is(err, io.EOF):
			// After a stop event this is a no-op; before one, the upstream
			// cut the stream short.
			s.finish(errNoStop)
		case s.ctx.Err() != nil:
			// The client went away and its cancelled context failed the
			// upstream read: that is an abandoned stream, not an upstream
			// failure.
			s.finish(errStreamAbandoned)
		default:
			err = upstreamError(s.route, err)
			s.finish(err)
		}
		return ev, err
	}
	s.mu.Lock()
	switch ev.Kind {
	case core.EventStart:
		ev.Model = s.model
		s.usage = ev.Usage
	case core.EventToolCallStart, core.EventToolCallDelta:
		// A reply that is only tool calls still has a first token.
		if s.ttft == 0 {
			s.ttft = s.now().Sub(s.start)
		}
	case core.EventDelta:
		if s.ttft == 0 {
			s.ttft = s.now().Sub(s.start)
		}
		s.chars += len(ev.Text)
		if s.lookup.Fillable() {
			s.text.WriteString(ev.Text)
		}
	case core.EventStop:
		s.usage = ev.Usage
		s.stop = ev.StopReason
	}
	s.mu.Unlock()
	if ev.Kind == core.EventStop {
		s.finish(nil)
	}
	return ev, nil
}

func (s *stream) Close() error {
	// Close upstream first: settling takes store round trips, and until
	// the upstream is closed the provider keeps generating.
	err := s.inner.Close()
	s.finish(errStreamAbandoned)
	return err
}

// errStreamAbandoned marks a stream closed before its stop event, which is
// what a client disconnect looks like from here.
var errStreamAbandoned = errors.New("stream closed before it finished")

// errNoStop marks a stream the upstream ended without a stop event.
var errNoStop = core.Errorf(core.KindUpstream, "stream ended without a stop event")

func (s *stream) finish(err error) {
	s.logged.Do(func() {
		s.mu.Lock()
		ttft, usage, chars, text, stop := s.ttft, s.usage, s.chars, s.text.String(), s.stop
		s.mu.Unlock()
		outcome := telemetry.OutcomeOK
		switch {
		case errors.Is(err, errStreamAbandoned):
			outcome = telemetry.OutcomeAbandoned
		case err != nil:
			outcome = telemetry.OutcomeError
		}
		spanErr := err
		if outcome == telemetry.OutcomeAbandoned {
			spanErr = nil // the client left; the upstream did not fail
		}
		endProvider(s.span, ttft, usage, outcome, spanErr)
		billed, spent := s.p.settle(s.ctx, s.adm, usage, chars, true, outcome)
		total := s.now().Sub(s.start)
		logCall(s.ctx, s.route, true, total, ttft, usage, err)
		s.p.observeCall(s.ctx, s.adm, s.req, s.lookup, true, total, ttft, billed, spent, err)
		if err == nil && s.lookup.Fillable() {
			// Only a stream that reached its stop event is a whole reply.
			u, _ := s.adm.fillUsage(usage, chars)
			s.p.policy.Cache.Fill(s.ctx, s.lookup, &core.Response{Text: text, StopReason: stop, Usage: u})
		}
	})
}

// startProvider starts the span around the upstream call. Its context is
// the one to hand the provider, so spans the provider starts (retries,
// hedged attempts) are its children.
func startProvider(ctx context.Context, rt Route, stream bool) (context.Context, trace.Span) {
	return telemetry.StartSpan(ctx, "provider",
		attribute.String("switchboard.provider", rt.ProviderName),
		attribute.String("switchboard.upstream_model", rt.Upstream),
		attribute.Bool("switchboard.stream", stream),
	)
}

// endProvider records what the call produced and ends its span.
func endProvider(span trace.Span, ttft time.Duration, u core.Usage, outcome string, err error) {
	if span.IsRecording() {
		span.SetAttributes(
			attribute.String("switchboard.outcome", outcome),
			attribute.Int("switchboard.input_tokens", u.InputTokens),
			attribute.Int("switchboard.output_tokens", u.OutputTokens),
		)
		if ttft > 0 {
			span.SetAttributes(attribute.Float64("switchboard.ttft_ms", ms(ttft)))
		}
	}
	telemetry.EndSpan(span, err)
}

// logCall writes the one line per call that says what it cost and how long
// it took. It never logs prompts or replies.
func logCall(ctx context.Context, rt Route, stream bool, total, ttft time.Duration, u core.Usage, err error) {
	attrs := []any{
		slog.String("model", rt.Model),
		slog.String("provider", rt.ProviderName),
		slog.String("upstream_model", rt.Upstream),
		slog.Bool("stream", stream),
		slog.Float64("duration_ms", ms(total)),
	}
	if ttft > 0 {
		attrs = append(attrs, slog.Float64("ttft_ms", ms(ttft)))
	}
	attrs = append(attrs,
		slog.Int("input_tokens", u.InputTokens),
		slog.Int("output_tokens", u.OutputTokens),
		slog.Int("cache_read_tokens", u.CacheReadTokens),
		slog.Int("cache_write_tokens", u.CacheWriteTokens),
	)
	if r := hedge.ReportFrom(ctx).Snapshot(); r.Attempts > 1 {
		attrs = append(attrs, slog.Int("attempts", r.Attempts), slog.Int("retries", r.Retries),
			slog.Bool("hedged", r.Hedged), slog.Bool("hedge_won", r.HedgeWon), slog.Int("cancelled_attempts", r.Cancelled))
	}
	log := telemetry.Logger(ctx)
	switch {
	case err == nil:
		log.Info("llm call", attrs...)
	case errors.Is(err, errStreamAbandoned):
		log.Info("llm call", append(attrs, slog.String("outcome", "abandoned"))...)
	default:
		e := core.AsError(err)
		log.Warn("llm call", append(attrs, slog.String("outcome", "error"), slog.String("error_kind", e.Kind.String()), slog.String("error", err.Error()))...)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

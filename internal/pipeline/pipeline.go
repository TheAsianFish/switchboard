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

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
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

// New builds a pipeline over a key table and routes, enforcing pol.
func New(keys *auth.Keys, routes []Route, pol Policy) *Pipeline {
	p := &Pipeline{keys: keys, routes: make(map[string]Route, len(routes)), policy: pol, now: time.Now}
	for _, r := range routes {
		p.routes[r.Model] = r
		p.names = append(p.names, r.Model)
	}
	sort.Strings(p.names)
	return p
}

var _ core.Gateway = (*Pipeline)(nil)

// Authenticate implements core.Gateway.
func (p *Pipeline) Authenticate(ctx context.Context, apiKey string) (context.Context, error) {
	pr, err := p.keys.Lookup(apiKey)
	if err != nil {
		return ctx, err
	}
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
// route and a copy of the request addressed to the upstream model.
func (p *Pipeline) resolve(ctx context.Context, req *core.Request) (Route, *core.Request, *admission, error) {
	pr := auth.FromContext(ctx)
	if pr == nil {
		return Route{}, nil, nil, core.Errorf(core.KindAuthentication, "missing API key")
	}
	rt, ok := p.routes[req.Model]
	if !ok {
		return Route{}, nil, nil, core.Errorf(core.KindNotFound, "model %q is not configured on this gateway", req.Model)
	}
	if !pr.Allows(req.Model) {
		return Route{}, nil, nil, core.Errorf(core.KindPermission, "this API key may not use model %q", req.Model)
	}
	var dec cache.Decision
	if p.policy.Cache != nil {
		// Decided before admission so that a malformed directive is
		// refused without charging the key's limits.
		d, err := cache.Decide(pr.Cache, cache.ReportFrom(ctx).Directive(), req, p.policy.CacheMaxTemperature)
		if err != nil {
			p.policy.Metrics.Call(pr.Name, rt.Model, telemetry.OutcomeError)
			return Route{}, nil, nil, err
		}
		dec = d
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
		return Route{}, nil, nil, err
	}
	adm.cache = dec
	up := *req
	up.Model = rt.Upstream
	if up.MaxTokens == 0 && adm.charged() {
		// The admission assumed this much output; sending it as the cap
		// keeps the real cost within the reservation instead of leaving
		// it to the upstream's own default, or to no cap at all.
		up.MaxTokens = adm.estimate.OutputTokens
	}
	return rt, &up, adm, nil
}

// Complete implements core.Gateway.
func (p *Pipeline) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	rt, up, adm, err := p.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	ctx, lk := p.lookup(ctx, adm, req, up)
	if lk.Response != nil {
		resp := lk.Response
		resp.Model = req.Model
		p.serveHit(ctx, adm, resp, false)
		return resp, nil
	}
	start := p.now()
	resp, err := rt.Provider.Complete(ctx, up)
	elapsed := p.now().Sub(start)
	if err != nil {
		err = upstreamError(rt, err)
		u, outcome := core.Usage{}, telemetry.OutcomeError
		if ctx.Err() != nil {
			// The client went away mid-call. The upstream may have read
			// the whole prompt and billed for it, and nothing here can
			// tell, so the prompt estimate is charged: an over-count
			// rather than a cost the ledger never sees.
			u.InputTokens, outcome = adm.estimate.InputTokens, telemetry.OutcomeAbandoned
		}
		p.settle(ctx, adm, u, 0, false, outcome)
		logCall(ctx, rt, false, elapsed, 0, core.Usage{}, err)
		return nil, err
	}
	resp.Model = req.Model
	p.settle(ctx, adm, resp.Usage, len(resp.Text), false, telemetry.OutcomeOK)
	logCall(ctx, rt, false, elapsed, elapsed, resp.Usage, nil)
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
	var lk *cache.Lookup
	switch {
	case p.policy.Cache == nil:
		return ctx, cache.Skip("no cache")
	case adm.cache.Mode == cache.ModeOff:
		lk = cache.Skip(adm.cache.Reason)
	default:
		keyed := *req
		keyed.MaxTokens = up.MaxTokens
		lk = p.policy.Cache.Lookup(ctx, adm.pr.Tenant, adm.cache.Mode, &keyed)
	}
	cache.ReportFrom(ctx).Set(lk)
	return telemetry.WithAttrs(ctx, "cache", string(lk.Status)), lk
}

// serveHit settles a request answered from the cache. It costs nothing:
// the token-limit charge and the budget reservation are returned in full.
// The request still counted against the request limit, which bounds how
// often a key may ask, whatever answers.
func (p *Pipeline) serveHit(ctx context.Context, adm *admission, resp *core.Response, stream bool) {
	p.settle(ctx, adm, core.Usage{}, 0, stream, telemetry.OutcomeCacheHit)
	if adm.route.Priced {
		p.policy.Metrics.CacheSaved(adm.pr.Name, adm.route.Model, adm.route.Price.Of(resp.Usage).USD())
	}
	logCall(ctx, adm.route, stream, p.now().Sub(adm.start), 0, core.Usage{}, nil)
}

// Stream implements core.Gateway.
func (p *Pipeline) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	rt, up, adm, err := p.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	ctx, lk := p.lookup(ctx, adm, req, up)
	if lk.Response != nil {
		// Replayed as canonical events, so the front end renders it in
		// the client's own wire format whichever format filled it.
		resp := lk.Response
		resp.Model = req.Model
		p.serveHit(ctx, adm, resp, true)
		return &core.SliceStream{Events: core.ResponseEvents(resp, cache.Chunks(resp.Text))}, nil
	}
	start := p.now()
	s, err := rt.Provider.Stream(ctx, up)
	if err != nil {
		err = upstreamError(rt, err)
		p.settle(ctx, adm, core.Usage{}, 0, true, telemetry.OutcomeError)
		logCall(ctx, rt, true, p.now().Sub(start), 0, core.Usage{}, err)
		return nil, err
	}
	return &stream{inner: s, ctx: ctx, p: p, adm: adm, route: rt, model: req.Model, start: start, now: p.now, lookup: lk}, nil
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
	p     *Pipeline
	adm   *admission
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
		s.p.settle(s.ctx, s.adm, usage, chars, true, outcome)
		logCall(s.ctx, s.route, true, s.now().Sub(s.start), ttft, usage, err)
		if err == nil && s.lookup.Fillable() {
			// Only a stream that reached its stop event is a whole reply.
			u, _ := s.adm.fillUsage(usage, chars)
			s.p.policy.Cache.Fill(s.ctx, s.lookup, &core.Response{Text: text, StopReason: stop, Usage: u})
		}
	})
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

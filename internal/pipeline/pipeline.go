// Package pipeline is the per-request chain between the front ends and the
// providers. In M1 the chain is auth → route → provider → log; limits,
// budgets, caching, hedging and settlement slot in between in later
// milestones without the front ends changing.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/core"
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
}

// Pipeline implements core.Gateway for `switchboard serve`.
type Pipeline struct {
	keys   *auth.Keys
	routes map[string]Route
	names  []string
	now    func() time.Time
}

// New builds a pipeline over a key table and routes.
func New(keys *auth.Keys, routes []Route) *Pipeline {
	p := &Pipeline{keys: keys, routes: make(map[string]Route, len(routes)), now: time.Now}
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
func (p *Pipeline) resolve(ctx context.Context, req *core.Request) (Route, *core.Request, error) {
	pr := auth.FromContext(ctx)
	if pr == nil {
		return Route{}, nil, core.Errorf(core.KindAuthentication, "missing API key")
	}
	rt, ok := p.routes[req.Model]
	if !ok {
		return Route{}, nil, core.Errorf(core.KindNotFound, "model %q is not configured on this gateway", req.Model)
	}
	if !pr.Allows(req.Model) {
		return Route{}, nil, core.Errorf(core.KindPermission, "this API key may not use model %q", req.Model)
	}
	up := *req
	up.Model = rt.Upstream
	return rt, &up, nil
}

// Complete implements core.Gateway.
func (p *Pipeline) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	rt, up, err := p.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	start := p.now()
	resp, err := rt.Provider.Complete(ctx, up)
	elapsed := p.now().Sub(start)
	if err != nil {
		err = upstreamError(rt, err)
		logCall(ctx, rt, false, elapsed, 0, core.Usage{}, err)
		return nil, err
	}
	resp.Model = req.Model
	logCall(ctx, rt, false, elapsed, elapsed, resp.Usage, nil)
	return resp, nil
}

// Stream implements core.Gateway.
func (p *Pipeline) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	rt, up, err := p.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	start := p.now()
	s, err := rt.Provider.Stream(ctx, up)
	if err != nil {
		err = upstreamError(rt, err)
		logCall(ctx, rt, true, p.now().Sub(start), 0, core.Usage{}, err)
		return nil, err
	}
	return &stream{inner: s, ctx: ctx, route: rt, model: req.Model, start: start, now: p.now}, nil
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

// stream rewrites the model name back to the client's and logs the call
// once, when it ends for whatever reason.
type stream struct {
	inner core.Stream
	ctx   context.Context
	route Route
	model string
	start time.Time
	now   func() time.Time

	// mu guards ttft and usage: Close may run on another goroutine than
	// Next, for example to abort a blocked read.
	mu     sync.Mutex
	ttft   time.Duration
	usage  core.Usage
	logged sync.Once
}

func (s *stream) Next() (core.Event, error) {
	ev, err := s.inner.Next()
	if err != nil {
		switch {
		case errors.Is(err, io.EOF):
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
	case core.EventStop:
		s.usage = ev.Usage
	}
	s.mu.Unlock()
	if ev.Kind == core.EventStop {
		s.finish(nil)
	}
	return ev, nil
}

func (s *stream) Close() error {
	s.finish(errStreamAbandoned)
	return s.inner.Close()
}

// errStreamAbandoned marks a stream closed before its stop event, which is
// what a client disconnect looks like from here.
var errStreamAbandoned = errors.New("stream closed before it finished")

func (s *stream) finish(err error) {
	s.logged.Do(func() {
		s.mu.Lock()
		ttft, usage := s.ttft, s.usage
		s.mu.Unlock()
		logCall(s.ctx, s.route, true, s.now().Sub(s.start), ttft, usage, err)
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

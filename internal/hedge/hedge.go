// Package hedge wraps a provider with the gateway's resilience: retries
// with backoff on retryable failures, hedged requests bounded by
// time-to-first-token with a per-tenant hedge budget, and a per-provider
// circuit breaker.
//
// One request is a series of races. A race sends an attempt and, if no
// first token has arrived after the hedge delay (by default the rolling p95
// of recent first-token times for that model), sends a second one; the
// first attempt to produce a token wins and the other is cancelled. A race
// that fails before any token reached the client is retried after a
// jittered backoff. Once a token has been handed to the client nothing is
// retried: the client has seen part of one reply and cannot be given
// another.
package hedge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/core"
)

// Config is a provider's resilience policy.
type Config struct {
	// Retries is the number of times a failed request is tried again; 0
	// disables retries.
	Retries int
	// BackoffBase and BackoffMax bound the wait before retry n, drawn
	// uniformly from [0, min(BackoffMax, BackoffBase·2^(n-1))] ("full
	// jitter", so replicas that failed together do not retry together).
	BackoffBase, BackoffMax time.Duration
	// MaxRetryAfter is the longest Retry-After the gateway waits out
	// itself; a longer one is returned to the client to wait instead.
	MaxRetryAfter time.Duration
	Hedge         HedgeConfig
	Breaker       BreakerConfig
}

// HedgeConfig sets when a second attempt is sent.
type HedgeConfig struct {
	Enabled bool
	// Delay is a fixed hedge delay; zero means the rolling Quantile of
	// the model's recent first-token times.
	Delay time.Duration
	// Quantile, Window and MinSamples define the rolling delay: the
	// Quantile of the last Window samples, used once there are at least
	// MinSamples of them. Until then no request is hedged.
	Quantile   float64
	Window     int
	MinSamples int
	// Budget is the share of a tenant's requests that may be hedged, and
	// Burst the hedges a tenant may save up.
	Budget float64
	Burst  float64
}

// DefaultConfig retries twice and opens the breaker after five failures in
// a row; hedging is off until configured.
var DefaultConfig = Config{
	Retries:       2,
	BackoffBase:   100 * time.Millisecond,
	BackoffMax:    2 * time.Second,
	MaxRetryAfter: 10 * time.Second,
	Hedge: HedgeConfig{
		Quantile:   0.95,
		Window:     1000,
		MinSamples: 100,
		Budget:     0.1,
		Burst:      10,
	},
	Breaker: BreakerConfig{Failures: 5, Cooldown: 10 * time.Second},
}

// Observer receives the layer's counts; *telemetry.Metrics implements it.
type Observer interface {
	// Retry counts a retry, by the kind of error that caused it.
	Retry(provider, kind string)
	// Hedge counts hedging events: sent, won, lost or refused (no budget).
	Hedge(provider, event string)
	// BreakerState reports a breaker transition.
	BreakerState(provider string, state int)
}

// Provider is a core.Provider with retries, hedging and a circuit breaker
// in front of another.
type Provider struct {
	name    string
	inner   core.Provider
	cfg     Config
	breaker *Breaker
	budgets *Budgets
	obs     Observer
	tracer  trace.Tracer
	now     func() time.Time

	mu      sync.Mutex
	windows map[string]*Window
	rnd     *rand.Rand
}

// Option configures a Provider.
type Option func(*Provider)

// WithObserver sends the layer's counts to obs.
func WithObserver(obs Observer) Option { return func(p *Provider) { p.obs = obs } }

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(p *Provider) { p.now = now } }

// WithSeed makes the backoff jitter deterministic, for tests.
func WithSeed(seed uint64) Option {
	return func(p *Provider) { p.rnd = rand.New(rand.NewPCG(seed, seed)) }
}

// New wraps inner, the provider configured as name.
func New(name string, inner core.Provider, cfg Config, opts ...Option) *Provider {
	p := &Provider{
		name:    name,
		inner:   inner,
		cfg:     cfg,
		budgets: NewBudgets(cfg.Hedge.Budget, cfg.Hedge.Burst),
		tracer:  otel.Tracer("github.com/TheAsianFish/switchboard/internal/hedge"),
		now:     time.Now,
		windows: map[string]*Window{},
		rnd:     rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
	for _, o := range opts {
		o(p)
	}
	p.breaker = NewBreaker(cfg.Breaker, p.now, func(s State) {
		if p.obs != nil {
			p.obs.BreakerState(p.name, int(s))
		}
	})
	if p.obs != nil {
		p.obs.BreakerState(name, int(Closed))
	}
	return p
}

var _ core.Provider = (*Provider)(nil)

// Health is a provider's state as the admin listener reports it.
type Health struct {
	Provider string `json:"provider"`
	Breaker  string `json:"breaker"`
	// RetryAfterMS is how long an open breaker keeps refusing.
	RetryAfterMS int64 `json:"retry_after_ms,omitempty"`
	Hedging      bool  `json:"hedging"`
	// HedgeDelayMS is each model's current hedge delay, keyed by upstream
	// model and mode ("stream" or "complete"), once it has enough samples.
	HedgeDelayMS map[string]float64 `json:"hedge_delay_ms,omitempty"`
}

// Health reports the breaker's state and the current hedge delays.
func (p *Provider) Health() Health {
	h := Health{Provider: p.name, Breaker: p.breaker.State().String(), Hedging: p.cfg.Hedge.Enabled}
	if ra := p.breaker.RetryAfter(); ra > 0 {
		h.RetryAfterMS = ra.Milliseconds()
	}
	p.mu.Lock()
	keys := make(map[string]*Window, len(p.windows))
	for k, w := range p.windows {
		keys[k] = w
	}
	p.mu.Unlock()
	for k, w := range keys {
		if d, n := w.Quantile(); n >= p.cfg.Hedge.MinSamples {
			if h.HedgeDelayMS == nil {
				h.HedgeDelayMS = map[string]float64{}
			}
			h.HedgeDelayMS[k] = float64(d.Microseconds()) / 1000
		}
	}
	return h
}

// Complete implements core.Provider. A non-streamed reply has no first
// token, so its hedge delay is a quantile of whole-reply latency.
func (p *Provider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	a, err := p.run(ctx, req, false)
	if err != nil {
		return nil, err
	}
	a.cancel()
	endSpan(a.span, "won", nil)
	return a.resp, nil
}

// Stream implements core.Provider. The returned stream starts with the
// winning attempt's buffered events.
func (p *Provider) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	a, err := p.run(ctx, req, true)
	if err != nil {
		return nil, err
	}
	return &stream{buf: a.events, inner: a.stream, cancel: a.cancel, span: a.span}, nil
}

// attempt is one request sent to the provider.
type attempt struct {
	n      int
	hedge  bool
	start  time.Time
	cancel context.CancelFunc
	span   trace.Span

	// Set by the attempt's goroutine before it is delivered.
	events    []core.Event
	stream    core.Stream
	resp      *core.Response
	err       error
	first     time.Duration
	cancelled bool
}

// run retries races until one produces a first token or the error is not
// worth retrying.
func (p *Provider) run(ctx context.Context, req *core.Request, streaming bool) (*attempt, error) {
	rep := ReportFrom(ctx)
	tenant := ""
	if pr := auth.FromContext(ctx); pr != nil {
		tenant = pr.Tenant
	}
	if p.cfg.Hedge.Enabled {
		p.budgets.Earn(tenant)
	}
	var lastErr error
	for try := 0; ; try++ {
		if try > 0 {
			wait, ok := p.backoff(ctx, try, lastErr)
			if !ok {
				return nil, lastErr
			}
			if err := sleep(ctx, wait); err != nil {
				return nil, lastErr
			}
			if p.obs != nil {
				p.obs.Retry(p.name, core.AsError(lastErr).Kind.String())
			}
			rep.update(func(r *Report) { r.retries++ })
		}
		a, err := p.race(ctx, req, streaming, tenant, rep)
		if err == nil {
			return a, nil
		}
		lastErr = err
		if ctx.Err() != nil || try >= p.cfg.Retries || !Retryable(err) {
			return nil, err
		}
	}
}

// Retryable reports whether a failed attempt is worth repeating: the
// provider was overloaded, rate limited, timed out or failed on its side.
// A refused request (bad input, credentials, unknown model, budget) fails
// the same way every time.
func Retryable(err error) bool {
	switch core.AsError(err).Kind {
	case core.KindRateLimited, core.KindOverloaded, core.KindTimeout, core.KindUpstream:
		return !errors.Is(err, ErrOpen)
	default:
		return false
	}
}

// countsAgainst reports whether a failure is the provider's: it moves the
// breaker towards open. A rate limit is the provider protecting itself
// from this gateway's traffic, not a sign it is down, so it is retried but
// not counted.
func countsAgainst(err error) bool {
	switch core.AsError(err).Kind {
	case core.KindOverloaded, core.KindTimeout, core.KindUpstream:
		return true
	default:
		return false
	}
}

// ErrOpen is the cause of the error returned while a provider's breaker is
// open.
var ErrOpen = errors.New("circuit breaker open")

// backoff returns the wait before retry try, or false if the request
// should not be retried: the provider asked for a longer wait than the
// gateway holds a request for, or the wait would outlast the deadline.
func (p *Provider) backoff(ctx context.Context, try int, err error) (time.Duration, bool) {
	ceiling := p.cfg.BackoffBase << (try - 1)
	if ceiling <= 0 || ceiling > p.cfg.BackoffMax {
		ceiling = p.cfg.BackoffMax
	}
	p.mu.Lock()
	wait := time.Duration(p.rnd.Int64N(int64(ceiling) + 1))
	p.mu.Unlock()
	if ra := core.AsError(err).RetryAfter; ra > 0 {
		if ra > p.cfg.MaxRetryAfter {
			return 0, false
		}
		wait = max(wait, ra)
	}
	if dl, ok := ctx.Deadline(); ok && p.now().Add(wait).After(dl) {
		return 0, false
	}
	return wait, true
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// window returns the latency window for an upstream model and mode.
func (p *Provider) window(model string, streaming bool) *Window {
	key := model + "/complete"
	if streaming {
		key = model + "/stream"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.windows[key]
	if !ok {
		w = NewWindow(p.cfg.Hedge.Window, p.cfg.Hedge.Quantile)
		p.windows[key] = w
	}
	return w
}

// hedgeDelay is how long the first attempt gets before a hedge is sent.
func (p *Provider) hedgeDelay(w *Window) (time.Duration, bool) {
	if !p.cfg.Hedge.Enabled {
		return 0, false
	}
	if p.cfg.Hedge.Delay > 0 {
		return p.cfg.Hedge.Delay, true
	}
	d, n := w.Quantile()
	if n < p.cfg.Hedge.MinSamples || d <= 0 {
		return 0, false
	}
	return d, true
}

// race sends one attempt, hedges it after the delay if the budget allows,
// and returns the first attempt to produce a token. Losers are cancelled
// and their streams closed in the background.
func (p *Provider) race(ctx context.Context, req *core.Request, streaming bool, tenant string, rep *Report) (*attempt, error) {
	if !p.breaker.Allow() {
		e := core.Wrap(core.KindOverloaded, ErrOpen, fmt.Sprintf("provider %q is failing; requests to it are paused", p.name))
		e.RetryAfter = p.breaker.RetryAfter()
		return nil, e
	}
	w := p.window(req.Model, streaming)
	results := make(chan *attempt, 2)
	var running []*attempt
	launch := func(hedge bool) *attempt {
		var n int
		rep.update(func(r *Report) {
			r.attempts++
			n = r.attempts
			if hedge {
				r.hedged = true
			}
		})
		actx, span := p.tracer.Start(ctx, "provider.attempt", trace.WithAttributes(
			attribute.String("switchboard.provider", p.name),
			attribute.Int("switchboard.attempt", n),
			attribute.Bool("switchboard.hedge", hedge),
		))
		actx, cancel := context.WithCancel(actx)
		a := &attempt{n: n, hedge: hedge, start: p.now(), cancel: cancel, span: span}
		running = append(running, a)
		go p.send(actx, a, req, streaming, results)
		return a
	}
	primary := launch(false)

	var timer <-chan time.Time
	if d, ok := p.hedgeDelay(w); ok {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	var firstErr error
	for {
		select {
		case <-timer:
			timer = nil
			if p.breaker.State() == Closed && p.budgets.Spend(tenant) {
				launch(true)
				p.observeHedge("sent")
			} else {
				p.observeHedge("refused")
			}
		case a := <-results:
			running = remove(running, a)
			if a.err == nil {
				p.breaker.Success()
				p.win(a, primary, w, running, rep, results)
				return a, nil
			}
			endSpan(a.span, "error", a.err)
			if !a.cancelled && countsAgainst(a.err) {
				p.breaker.Failure()
			}
			if firstErr == nil {
				firstErr = a.err
			}
			if len(running) == 0 {
				return nil, firstErr
			}
		case <-ctx.Done():
			p.abandon(running, results)
			return nil, core.Wrap(core.KindTimeout, ctx.Err(), "request cancelled")
		}
	}
}

// win records the winner's first-token time and cancels the rest.
//
// Only the first attempt's time goes into the window. A hedge's time is
// seen only when the hedge wins, which selects fast ones. When the first
// attempt loses, the time it had waited when it was cancelled goes in
// instead: its real time is at least that, so the window keeps the slow
// tail it would otherwise lose, and the delay does not drift down as
// hedging succeeds.
func (p *Provider) win(a, primary *attempt, w *Window, losers []*attempt, rep *Report, results <-chan *attempt) {
	if a == primary {
		w.Add(a.first)
	} else {
		w.Add(p.now().Sub(primary.start))
	}
	if a.hedge {
		rep.update(func(r *Report) { r.hedgeWon = true })
		p.observeHedge("won")
	} else if len(losers) > 0 {
		p.observeHedge("lost")
	}
	if len(losers) > 0 {
		rep.update(func(r *Report) { r.cancelled += len(losers) })
		a.span.SetAttributes(attribute.Bool("switchboard.winner", true))
	}
	for _, l := range losers {
		l.cancel()
	}
	// Each attempt's goroutine delivers exactly once; the losers' are
	// collected here so their streams are closed and spans ended.
	n := len(losers)
	go func() {
		for range n {
			closeAttempt(<-results, "lost")
		}
	}()
}

// abandon cancels every running attempt after the caller went away.
func (p *Provider) abandon(running []*attempt, results <-chan *attempt) {
	for _, a := range running {
		a.cancel()
	}
	go func() {
		for range running {
			a := <-results
			closeAttempt(a, "abandoned")
		}
	}()
}

func remove(as []*attempt, a *attempt) []*attempt {
	out := as[:0]
	for _, x := range as {
		if x != a {
			out = append(out, x)
		}
	}
	return out
}

func (p *Provider) observeHedge(event string) {
	if p.obs != nil {
		p.obs.Hedge(p.name, event)
	}
}

// send runs one attempt until it has a first token (or a whole reply),
// fails, or is cancelled, and delivers it exactly once.
func (p *Provider) send(ctx context.Context, a *attempt, req *core.Request, streaming bool, out chan<- *attempt) {
	defer func() {
		a.cancelled = ctx.Err() != nil
		out <- a
	}()
	if !streaming {
		a.resp, a.err = p.inner.Complete(ctx, req)
		a.first = p.now().Sub(a.start)
		return
	}
	s, err := p.inner.Stream(ctx, req)
	if err != nil {
		a.err = err
		return
	}
	for {
		ev, err := s.Next()
		if err != nil {
			_ = s.Close()
			if errors.Is(err, io.EOF) {
				err = core.Errorf(core.KindUpstream, "upstream stream ended before its first token")
			}
			a.err = err
			return
		}
		a.events = append(a.events, ev)
		if ev.Kind != core.EventStart {
			a.first = p.now().Sub(a.start)
			a.stream = s
			return
		}
	}
}

// closeAttempt releases what a losing or abandoned attempt opened.
func closeAttempt(a *attempt, outcome string) {
	if a.stream != nil {
		_ = a.stream.Close()
	}
	a.cancel()
	endSpan(a.span, outcome, nil)
}

func endSpan(span trace.Span, outcome string, err error) {
	span.SetAttributes(attribute.String("switchboard.outcome", outcome))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, core.AsError(err).Kind.String())
	}
	span.End()
}

// stream is the winning attempt's stream with its buffered first events
// in front.
type stream struct {
	buf    []core.Event
	inner  core.Stream
	cancel context.CancelFunc
	span   trace.Span
	once   sync.Once
}

func (s *stream) Next() (core.Event, error) {
	if len(s.buf) > 0 {
		ev := s.buf[0]
		s.buf = s.buf[1:]
		return ev, nil
	}
	ev, err := s.inner.Next()
	if err != nil {
		outcome := "won"
		if !errors.Is(err, io.EOF) {
			outcome = "error"
		} else {
			err = io.EOF
		}
		s.end(outcome, err)
	}
	return ev, err
}

func (s *stream) Close() error {
	err := s.inner.Close()
	s.end("won", nil)
	return err
}

func (s *stream) end(outcome string, err error) {
	s.once.Do(func() {
		s.cancel()
		if errors.Is(err, io.EOF) {
			err = nil
		}
		endSpan(s.span, outcome, err)
	})
}

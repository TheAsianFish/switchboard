package hedge

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// script is a provider whose n-th call (from 0) behaves as plan(n) says:
// wait, then fail with err or stream a short reply.
type script struct {
	plan   func(n int) (time.Duration, error)
	calls  atomic.Int32
	closed atomic.Int32
	// cancelled counts calls whose context was cancelled while they
	// waited.
	cancelled atomic.Int32
}

func (s *script) wait(ctx context.Context) (int, error) {
	n := int(s.calls.Add(1)) - 1
	d, err := s.plan(n)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		s.cancelled.Add(1)
		return n, core.Wrap(core.KindTimeout, ctx.Err(), "request cancelled")
	case <-t.C:
	}
	return n, err
}

func (s *script) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	n, err := s.wait(ctx)
	if err != nil {
		return nil, err
	}
	return &core.Response{ID: string(rune('a' + n)), Model: req.Model, Text: "hi", StopReason: core.StopEndTurn}, nil
}

func (s *script) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	return &scriptStream{s: s, ctx: ctx, model: req.Model}, nil
}

type scriptStream struct {
	s     *script
	ctx   context.Context
	model string
	n     int
	i     int
}

func (st *scriptStream) Next() (core.Event, error) {
	st.i++
	switch st.i {
	case 1:
		return core.Event{Kind: core.EventStart, Model: st.model, Usage: core.Usage{InputTokens: 3}}, nil
	case 2:
		n, err := st.s.wait(st.ctx)
		if err != nil {
			return core.Event{}, err
		}
		st.n = n
		return core.Event{Kind: core.EventDelta, Text: string(rune('a' + n))}, nil
	case 3:
		return core.Event{Kind: core.EventStop, StopReason: core.StopEndTurn, Usage: core.Usage{InputTokens: 3, OutputTokens: 1}}, nil
	default:
		return core.Event{}, io.EOF
	}
}

func (st *scriptStream) Close() error {
	st.s.closed.Add(1)
	return nil
}

func quick() Config {
	c := DefaultConfig
	c.BackoffBase, c.BackoffMax = time.Millisecond, 2*time.Millisecond
	return c
}

func overloaded() error { return core.Errorf(core.KindOverloaded, "busy") }

type counts struct {
	mu      sync.Mutex
	retries map[string]int
	hedges  map[string]int
	states  []int
}

func newCounts() *counts { return &counts{retries: map[string]int{}, hedges: map[string]int{}} }

func (c *counts) Retry(_, kind string) { c.mu.Lock(); c.retries[kind]++; c.mu.Unlock() }
func (c *counts) Hedge(_, ev string)   { c.mu.Lock(); c.hedges[ev]++; c.mu.Unlock() }
func (c *counts) BreakerState(_ string, s int) {
	c.mu.Lock()
	c.states = append(c.states, s)
	c.mu.Unlock()
}

func (c *counts) hedge(ev string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hedges[ev]
}

func TestRetries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		plan     func(n int) (time.Duration, error)
		cfg      func(*Config)
		wantErr  core.ErrorKind
		attempts int
	}{
		{name: "overloaded then ok", plan: func(n int) (time.Duration, error) {
			if n == 0 {
				return 0, overloaded()
			}
			return 0, nil
		}, attempts: 2},
		{name: "gives up after retries", plan: func(int) (time.Duration, error) { return 0, overloaded() },
			wantErr: core.KindOverloaded, attempts: 3},
		{name: "invalid request is not retried", plan: func(int) (time.Duration, error) {
			return 0, core.Errorf(core.KindInvalidRequest, "bad")
		}, wantErr: core.KindInvalidRequest, attempts: 1},
		{name: "retry-after beyond the cap goes back to the client", plan: func(int) (time.Duration, error) {
			e := core.Errorf(core.KindRateLimited, "slow down")
			e.RetryAfter = time.Minute
			return 0, e
		}, wantErr: core.KindRateLimited, attempts: 1},
		{name: "retries disabled", plan: func(int) (time.Duration, error) { return 0, overloaded() },
			cfg: func(c *Config) { c.Retries = 0 }, wantErr: core.KindOverloaded, attempts: 1},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				sc := &script{plan: tc.plan}
				cfg := quick()
				if tc.cfg != nil {
					tc.cfg(&cfg)
				}
				obs := newCounts()
				p := New("up", sc, cfg, WithObserver(obs), WithSeed(1))
				ctx, rep := WithReport(context.Background())
				var err error
				if streaming {
					var s core.Stream
					if s, err = p.Stream(ctx, &core.Request{Model: "m", Stream: true}); err == nil {
						_, err = core.Collect(s)
					}
				} else {
					_, err = p.Complete(ctx, &core.Request{Model: "m"})
				}
				if tc.wantErr == 0 && err != nil {
					t.Fatalf("err = %v", err)
				}
				if tc.wantErr != 0 && core.AsError(err).Kind != tc.wantErr {
					t.Fatalf("err = %v, want kind %v", err, tc.wantErr)
				}
				snap := rep.Snapshot()
				if snap.Attempts != tc.attempts || int(sc.calls.Load()) != tc.attempts {
					t.Fatalf("attempts = %d (provider saw %d), want %d", snap.Attempts, sc.calls.Load(), tc.attempts)
				}
				if snap.Retries != tc.attempts-1 {
					t.Fatalf("retries = %d, want %d", snap.Retries, tc.attempts-1)
				}
			})
		}
	}
}

func TestBackoffRespectsRetryAfterAndDeadline(t *testing.T) {
	t.Parallel()
	p := New("up", &script{}, quick(), WithSeed(3))
	e := core.Errorf(core.KindRateLimited, "x")
	e.RetryAfter = 300 * time.Millisecond
	if d, ok := p.backoff(context.Background(), 1, e); !ok || d < 300*time.Millisecond {
		t.Fatalf("backoff = %v, %v; want at least the Retry-After", d, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, ok := p.backoff(ctx, 1, e); ok {
		t.Fatal("a wait past the deadline was allowed")
	}
	for try := 1; try < 20; try++ {
		d, ok := p.backoff(context.Background(), try, overloaded())
		if !ok || d < 0 || d > quick().BackoffMax {
			t.Fatalf("try %d: backoff %v outside [0, max]", try, d)
		}
	}
}

func hedging(delay time.Duration) Config {
	c := quick()
	c.Hedge.Enabled, c.Hedge.Delay = true, delay
	return c
}

func TestHedgeWinsAndCancelsTheSlowAttempt(t *testing.T) {
	t.Parallel()
	for _, streaming := range []bool{false, true} {
		sc := &script{plan: func(n int) (time.Duration, error) {
			if n == 0 {
				return 5 * time.Second, nil
			}
			return 5 * time.Millisecond, nil
		}}
		obs := newCounts()
		p := New("up", sc, hedging(20*time.Millisecond), WithObserver(obs))
		ctx, rep := WithReport(context.Background())
		start := time.Now()
		var text string
		if streaming {
			s, err := p.Stream(ctx, &core.Request{Model: "m", Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := core.Collect(s)
			if err != nil {
				t.Fatal(err)
			}
			text = resp.Text
		} else {
			resp, err := p.Complete(ctx, &core.Request{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			text = resp.ID
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("took %v: the hedge did not win", took)
		}
		if text != "b" {
			t.Fatalf("reply from attempt %q, want the hedge's (b)", text)
		}
		snap := rep.Snapshot()
		if !snap.Hedged || !snap.HedgeWon || snap.Attempts != 2 || snap.Cancelled != 1 {
			t.Fatalf("report = %+v", snap)
		}
		waitFor(t, func() bool { return sc.cancelled.Load() == 1 }, "the slow attempt was not cancelled")
		if obs.hedge("sent") != 1 || obs.hedge("won") != 1 {
			t.Fatalf("hedge counts = %v", obs.hedges)
		}
	}
}

func TestNoHedgeWhenTheFirstAttemptIsFast(t *testing.T) {
	t.Parallel()
	sc := &script{plan: func(int) (time.Duration, error) { return 0, nil }}
	p := New("up", sc, hedging(200*time.Millisecond))
	ctx, rep := WithReport(context.Background())
	s, err := p.Stream(ctx, &core.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Collect(s); err != nil {
		t.Fatal(err)
	}
	if snap := rep.Snapshot(); snap.Hedged || snap.Attempts != 1 {
		t.Fatalf("report = %+v", snap)
	}
}

func TestHedgeBudgetIsPerTenant(t *testing.T) {
	t.Parallel()
	b := NewBudgets(0.1, 1)
	hedges := 0
	for range 100 {
		b.Earn("a")
		if b.Spend("a") {
			hedges++
		}
	}
	// At most the one saved up plus a tenth of 100 requests; the first
	// request's share is lost to the cap while the burst is full.
	if hedges < 10 || hedges > 11 {
		t.Fatalf("hedges = %d, want 10 or 11", hedges)
	}
	if !b.Spend("b") {
		t.Fatal("tenant b had no burst of its own")
	}
	if b.Spend("b") {
		t.Fatal("tenant b hedged twice without requests")
	}
}

func TestHedgeRefusedWithoutBudget(t *testing.T) {
	t.Parallel()
	sc := &script{plan: func(n int) (time.Duration, error) { return 60 * time.Millisecond, nil }}
	cfg := hedging(5 * time.Millisecond)
	cfg.Hedge.Budget, cfg.Hedge.Burst = 0, 1
	obs := newCounts()
	p := New("up", sc, cfg, WithObserver(obs))
	for i := range 2 {
		ctx, rep := WithReport(context.Background())
		if _, err := p.Complete(ctx, &core.Request{Model: "m"}); err != nil {
			t.Fatal(err)
		}
		if got, want := rep.Snapshot().Hedged, i == 0; got != want {
			t.Fatalf("request %d hedged = %v, want %v", i, got, want)
		}
	}
	if obs.hedge("refused") != 1 {
		t.Fatalf("hedge counts = %v", obs.hedges)
	}
}

func TestFailedHedgeLeavesTheFirstAttemptRunning(t *testing.T) {
	t.Parallel()
	sc := &script{plan: func(n int) (time.Duration, error) {
		if n == 0 {
			return 50 * time.Millisecond, nil
		}
		return 0, overloaded()
	}}
	p := New("up", sc, hedging(5*time.Millisecond))
	ctx, rep := WithReport(context.Background())
	resp, err := p.Complete(ctx, &core.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "a" || rep.Snapshot().HedgeWon {
		t.Fatalf("resp %q, report %+v", resp.ID, rep.Snapshot())
	}
}

func TestAdaptiveDelayWaitsForSamples(t *testing.T) {
	t.Parallel()
	cfg := quick()
	cfg.Hedge.Enabled, cfg.Hedge.MinSamples, cfg.Hedge.Window = true, 10, 20
	p := New("up", &script{plan: func(int) (time.Duration, error) { return 0, nil }}, cfg)
	w := p.window("m", true)
	if _, ok := p.hedgeDelay(w); ok {
		t.Fatal("hedged with no samples")
	}
	for i := range 20 {
		w.Add(time.Duration(i+1) * time.Millisecond)
	}
	d, ok := p.hedgeDelay(w)
	if !ok || d != 19*time.Millisecond {
		t.Fatalf("delay = %v, %v; want the p95 of 1..20 ms (19 ms)", d, ok)
	}
}

func TestWindowKeepsTheLosersWait(t *testing.T) {
	t.Parallel()
	sc := &script{plan: func(n int) (time.Duration, error) {
		if n == 0 {
			return time.Second, nil
		}
		return 0, nil
	}}
	p := New("up", sc, hedging(30*time.Millisecond))
	if _, err := p.Complete(context.Background(), &core.Request{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	d, n := p.window("m", false).Quantile()
	if n != 1 || d < 30*time.Millisecond {
		t.Fatalf("window = %v over %d samples, want the first attempt's wait (≥30 ms)", d, n)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestBreaker(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Unix(1000, 0)}
	var fail atomic.Bool
	fail.Store(true)
	sc := &script{plan: func(int) (time.Duration, error) {
		if fail.Load() {
			return 0, overloaded()
		}
		return 0, nil
	}}
	cfg := quick()
	cfg.Retries = 0
	cfg.Breaker = BreakerConfig{Failures: 3, Cooldown: 10 * time.Second}
	obs := newCounts()
	p := New("up", sc, cfg, WithClock(clk.now), WithObserver(obs))
	call := func() error {
		_, err := p.Complete(context.Background(), &core.Request{Model: "m"})
		return err
	}
	for range 3 {
		if err := call(); errors.Is(err, ErrOpen) {
			t.Fatal("open before the threshold")
		}
	}
	if p.Health().Breaker != "open" {
		t.Fatalf("health = %+v", p.Health())
	}
	err := call()
	if !errors.Is(err, ErrOpen) || core.AsError(err).Kind != core.KindOverloaded || core.AsError(err).RetryAfter != 10*time.Second {
		t.Fatalf("err = %v (retry after %v), want a fast refusal", err, core.AsError(err).RetryAfter)
	}
	if sc.calls.Load() != 3 {
		t.Fatalf("an open breaker let a call through (%d calls)", sc.calls.Load())
	}
	clk.add(10 * time.Second)
	// The probe fails: open again at once.
	if err := call(); errors.Is(err, ErrOpen) {
		t.Fatal("no probe after the cooldown")
	}
	if err := call(); !errors.Is(err, ErrOpen) {
		t.Fatalf("a failed probe left the breaker %s", p.Health().Breaker)
	}
	clk.add(10 * time.Second)
	fail.Store(false)
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if p.Health().Breaker != "closed" {
		t.Fatalf("a good probe left the breaker %s", p.Health().Breaker)
	}
	want := []int{int(Closed), int(Open), int(HalfOpen), int(Open), int(HalfOpen), int(Closed)}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.states) != len(want) {
		t.Fatalf("transitions = %v, want %v", obs.states, want)
	}
	for i := range want {
		if obs.states[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", obs.states, want)
		}
	}
}

func TestBreakerIgnoresClientErrors(t *testing.T) {
	t.Parallel()
	sc := &script{plan: func(int) (time.Duration, error) { return 0, core.Errorf(core.KindInvalidRequest, "bad") }}
	cfg := quick()
	cfg.Breaker.Failures = 2
	p := New("up", sc, cfg)
	for range 5 {
		_, _ = p.Complete(context.Background(), &core.Request{Model: "m"})
	}
	if s := p.Health().Breaker; s != "closed" {
		t.Fatalf("breaker %s after client errors", s)
	}
}

func TestClientCancelStopsEveryAttempt(t *testing.T) {
	t.Parallel()
	sc := &script{plan: func(int) (time.Duration, error) { return 5 * time.Second, nil }}
	p := New("up", sc, hedging(5*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := p.Stream(ctx, &core.Request{Model: "m", Stream: true})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	waitFor(t, func() bool { return sc.cancelled.Load() == 2 && sc.closed.Load() == 2 }, "attempts not cancelled and closed")
	if s := p.Health().Breaker; s != "closed" {
		t.Fatalf("a cancelled request moved the breaker to %s", s)
	}
}

func TestStreamCloseReleasesTheWinner(t *testing.T) {
	t.Parallel()
	sc := &script{plan: func(int) (time.Duration, error) { return 0, nil }}
	p := New("up", sc, quick())
	s, err := p.Stream(context.Background(), &core.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.Next()
	if err != nil || ev.Kind != core.EventStart {
		t.Fatalf("first event %v, %v", ev, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if sc.closed.Load() != 1 {
		t.Fatal("upstream stream not closed")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClientCancelCountsEveryRunningAttempt(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		sc := &script{plan: func(int) (time.Duration, error) { return 5 * time.Second, nil }}
		p := New("up", sc, hedging(5*time.Millisecond))
		ctx, rep := WithReport(context.Background())
		ctx, cancel := context.WithCancel(ctx)
		go func() {
			time.Sleep(40 * time.Millisecond)
			cancel()
		}()
		req := &core.Request{Model: "m", Stream: stream}
		var err error
		if stream {
			_, err = p.Stream(ctx, req)
		} else {
			_, err = p.Complete(ctx, req)
		}
		if err == nil {
			t.Fatal("a cancelled request succeeded")
		}
		// Both attempts reached the provider and may have been billed for
		// their prompts, so both are reported for settle to charge.
		if s := rep.Snapshot(); s.Attempts != 2 || s.Cancelled != 2 {
			t.Fatalf("stream=%v: report = %+v, want 2 attempts, 2 cancelled", stream, s)
		}
	}
}

func TestCancelledProbeGivesBackItsSlot(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Unix(1000, 0)}
	var hang atomic.Bool
	sc := &script{plan: func(int) (time.Duration, error) {
		if hang.Load() {
			return 5 * time.Second, nil
		}
		return 0, overloaded()
	}}
	cfg := quick()
	cfg.Retries = 0
	cfg.Breaker = BreakerConfig{Failures: 1, Cooldown: 10 * time.Second}
	p := New("up", sc, cfg, WithClock(clk.now))
	if _, err := p.Complete(context.Background(), &core.Request{Model: "m"}); err == nil || errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want the provider's failure", err)
	}
	clk.add(10 * time.Second)
	// The probe's client goes away before the provider answers.
	hang.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Complete(ctx, &core.Request{Model: "m"}); err == nil || errors.Is(err, ErrOpen) {
		t.Fatalf("probe err = %v, want a cancellation", err)
	}
	if s := p.Health().Breaker; s != "half_open" {
		t.Fatalf("a cancelled probe moved the breaker to %s", s)
	}
	// The next request probes at once instead of waiting out a cooldown.
	hang.Store(false)
	sc.plan = func(int) (time.Duration, error) { return 0, nil }
	if _, err := p.Complete(context.Background(), &core.Request{Model: "m"}); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if s := p.Health().Breaker; s != "closed" {
		t.Fatalf("breaker %s after a good probe", s)
	}
}

func TestHalfOpenRefusalCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Unix(1000, 0)}
	b := NewBreaker(BreakerConfig{Failures: 1, Cooldown: 10 * time.Second}, clk.now, nil)
	b.Failure()
	clk.add(10 * time.Second)
	if !b.Allow() {
		t.Fatal("no probe after the cooldown")
	}
	clk.add(3 * time.Second)
	if b.Allow() {
		t.Fatal("a second probe while the first is running")
	}
	if got := b.RetryAfter(); got != 7*time.Second {
		t.Fatalf("retry after = %v, want 7s", got)
	}
}

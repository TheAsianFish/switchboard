package hedge

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// stallFirst is a provider whose first `stalls` calls never send a first
// token: they hang until their context ends. inStream makes the hang
// happen inside Stream itself (a provider that accepts the connection and
// never sends headers) rather than in the first Next. Later calls answer
// at once with a start, one delta, then a second delta after tail, then a
// stop; the tail checks the context, so a timer that outlives the first
// token would cut the stream.
type stallFirst struct {
	stalls   int32
	inStream bool
	tail     time.Duration
	calls    atomic.Int32
}

func (p *stallFirst) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	if p.calls.Add(1) <= p.stalls {
		<-ctx.Done()
		return nil, core.Wrap(core.KindTimeout, ctx.Err(), "request cancelled")
	}
	return &core.Response{Model: req.Model, Text: "hi", StopReason: core.StopEndTurn}, nil
}

func (p *stallFirst) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	stalled := p.calls.Add(1) <= p.stalls
	if stalled && p.inStream {
		<-ctx.Done()
		return nil, core.Wrap(core.KindTimeout, ctx.Err(), "request cancelled")
	}
	return &stallStream{ctx: ctx, stalled: stalled, tail: p.tail}, nil
}

type stallStream struct {
	ctx     context.Context
	stalled bool
	tail    time.Duration
	i       int
}

func (s *stallStream) Next() (core.Event, error) {
	s.i++
	switch s.i {
	case 1:
		return core.Event{Kind: core.EventStart}, nil
	case 2:
		if s.stalled {
			<-s.ctx.Done()
			return core.Event{}, core.Wrap(core.KindTimeout, s.ctx.Err(), "request cancelled")
		}
		return core.Event{Kind: core.EventDelta, Text: "a"}, nil
	case 3:
		t := time.NewTimer(s.tail)
		defer t.Stop()
		select {
		case <-s.ctx.Done():
			return core.Event{}, core.Wrap(core.KindTimeout, s.ctx.Err(), "request cancelled")
		case <-t.C:
		}
		return core.Event{Kind: core.EventDelta, Text: "b"}, nil
	case 4:
		return core.Event{Kind: core.EventStop, StopReason: core.StopEndTurn}, nil
	default:
		return core.Event{}, io.EOF
	}
}

func (s *stallStream) Close() error { return nil }

func readAll(t *testing.T, s core.Stream) string {
	t.Helper()
	defer func() { _ = s.Close() }()
	var text string
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return text
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		text += ev.Text
	}
}

func fttConfig(timeout time.Duration, retries int) Config {
	c := quick()
	c.Retries = retries
	c.FirstTokenTimeout = timeout
	return c
}

func TestFirstTokenTimeoutFailsAStalledAttempt(t *testing.T) {
	t.Parallel()
	for _, inStream := range []bool{false, true} {
		name := "stall in first Next"
		if inStream {
			name = "stall in Stream"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			obs := newCounts()
			p := New("up", &stallFirst{stalls: 1, inStream: inStream}, fttConfig(20*time.Millisecond, 0), WithObserver(obs))
			start := time.Now()
			_, err := p.Stream(context.Background(), &core.Request{Model: "m", Stream: true})
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("failed after %v; the timeout is 20ms", elapsed)
			}
			var e *core.Error
			if !errors.As(err, &e) || e.Kind != core.KindTimeout || !errors.Is(err, ErrFirstToken) {
				t.Fatalf("err = %v, want a timeout caused by ErrFirstToken", err)
			}
			if obs.timeouts() != 1 {
				t.Errorf("first-token timeouts = %d, want 1", obs.timeouts())
			}
		})
	}
}

func TestFirstTokenTimeoutIsRetriedAndCountedByTheBreaker(t *testing.T) {
	t.Parallel()
	obs := newCounts()
	up := &stallFirst{stalls: 1}
	p := New("up", up, fttConfig(20*time.Millisecond, 1), WithObserver(obs))
	ctx, rep := WithReport(context.Background())
	s, err := p.Stream(ctx, &core.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := readAll(t, s); got != "ab" {
		t.Errorf("text = %q, want the retry's reply", got)
	}
	if snap := rep.Snapshot(); snap.Attempts != 2 || snap.Retries != 1 {
		t.Errorf("report = %+v, want two attempts and one retry", snap)
	}
	if obs.retries["timeout"] != 1 || obs.timeouts() != 1 {
		t.Errorf("retries %v, first-token timeouts %d", obs.retries, obs.timeouts())
	}

	// Five stalled attempts in a row open the breaker, which then
	// refuses at once with a Retry-After.
	up = &stallFirst{stalls: 1 << 20}
	p = New("up", up, fttConfig(10*time.Millisecond, 0))
	for i := range DefaultConfig.Breaker.Failures {
		if _, err := p.Stream(context.Background(), &core.Request{Model: "m", Stream: true}); !errors.Is(err, ErrFirstToken) {
			t.Fatalf("attempt %d: err = %v, want a first-token timeout", i, err)
		}
	}
	if st := p.Health().Breaker; st != "open" {
		t.Fatalf("breaker %s after %d first-token timeouts, want open", st, DefaultConfig.Breaker.Failures)
	}
	start := time.Now()
	_, err = p.Stream(context.Background(), &core.Request{Model: "m", Stream: true})
	var e *core.Error
	if !errors.Is(err, ErrOpen) || !errors.As(err, &e) || e.RetryAfter <= 0 {
		t.Fatalf("err = %v, want a refusal with Retry-After", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Errorf("refusal took %v", elapsed)
	}
	if n := up.calls.Load(); n != int32(DefaultConfig.Breaker.Failures) {
		t.Errorf("upstream calls = %d; the refusal must not reach the provider", n)
	}
}

func TestFirstTokenTimeoutStopsAtTheFirstToken(t *testing.T) {
	t.Parallel()
	obs := newCounts()
	// The second delta comes 4× the timeout after the first: a timer still
	// running after the first token would cut the stream.
	p := New("up", &stallFirst{tail: 80 * time.Millisecond}, fttConfig(20*time.Millisecond, 0), WithObserver(obs))
	s, err := p.Stream(context.Background(), &core.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := readAll(t, s); got != "ab" {
		t.Errorf("text = %q, want the whole reply", got)
	}
	if obs.timeouts() != 0 {
		t.Errorf("first-token timeouts = %d, want 0", obs.timeouts())
	}
}

func TestFirstTokenTimeoutLeavesCompleteAlone(t *testing.T) {
	t.Parallel()
	obs := newCounts()
	up := &script{plan: func(int) (time.Duration, error) { return 60 * time.Millisecond, nil }}
	p := New("up", up, fttConfig(10*time.Millisecond, 0), WithObserver(obs))
	if _, err := p.Complete(context.Background(), &core.Request{Model: "m"}); err != nil {
		t.Fatalf("complete: %v; a non-streamed reply has no first token to time", err)
	}
	if obs.timeouts() != 0 {
		t.Errorf("first-token timeouts = %d, want 0", obs.timeouts())
	}
}

func TestFirstTokenTimeoutOffByDefault(t *testing.T) {
	t.Parallel()
	if DefaultConfig.FirstTokenTimeout != 0 {
		t.Fatalf("default first-token timeout = %v, want off", DefaultConfig.FirstTokenTimeout)
	}
	obs := newCounts()
	up := &script{plan: func(int) (time.Duration, error) { return 60 * time.Millisecond, nil }}
	p := New("up", up, quick(), WithObserver(obs))
	s, err := p.Stream(context.Background(), &core.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_ = readAll(t, s)
	if obs.timeouts() != 0 {
		t.Errorf("first-token timeouts = %d with the setting off", obs.timeouts())
	}
}

func TestCallerCancelIsNotAFirstTokenTimeout(t *testing.T) {
	t.Parallel()
	obs := newCounts()
	cfg := fttConfig(time.Hour, 0)
	cfg.Breaker.Failures = 1
	p := New("up", &stallFirst{stalls: 1}, cfg, WithObserver(obs))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Stream(ctx, &core.Request{Model: "m", Stream: true})
	if err == nil || errors.Is(err, ErrFirstToken) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	if obs.timeouts() != 0 || p.Health().Breaker != "closed" {
		t.Errorf("first-token timeouts %d, breaker %s: a caller leaving is not the provider's failure", obs.timeouts(), p.Health().Breaker)
	}
}

func TestHedgeWinnerCancelsTheStalledAttemptBeforeItsTimer(t *testing.T) {
	t.Parallel()
	obs := newCounts()
	cfg := fttConfig(200*time.Millisecond, 0)
	cfg.Hedge = HedgeConfig{Enabled: true, Delay: 10 * time.Millisecond, Budget: 1, Burst: 10}
	p := New("up", &stallFirst{stalls: 1}, cfg, WithObserver(obs))
	s, err := p.Stream(context.Background(), &core.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_ = readAll(t, s)
	time.Sleep(250 * time.Millisecond) // past the stalled attempt's timer
	if obs.hedge("won") != 1 || obs.timeouts() != 0 {
		t.Errorf("hedges won %d, first-token timeouts %d: the loser was cancelled, not timed out", obs.hedge("won"), obs.timeouts())
	}
	if st := p.Health().Breaker; st != "closed" {
		t.Errorf("breaker %s", st)
	}
}

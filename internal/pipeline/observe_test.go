package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// calls collects what OnCall reports.
type calls struct {
	mu  sync.Mutex
	got []Call
}

func (c *calls) observe(call Call) {
	c.mu.Lock()
	c.got = append(c.got, call)
	c.mu.Unlock()
}

func (c *calls) take(t *testing.T) Call {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.got) != 1 {
		t.Fatalf("OnCall reported %d calls, want exactly 1: %+v", len(c.got), c.got)
	}
	out := c.got[0]
	c.got = nil
	return out
}

func zeroTemp(model, prompt string) *core.Request {
	t0 := 0.0
	return &core.Request{Model: model, Temperature: &t0, Messages: []core.Message{{Role: core.RoleUser, Text: prompt}}}
}

// Every request is reported once, with what the router chose, what the
// cache found, what it cost and the request id the log line carries.
func TestOnCallReportsEveryEnding(t *testing.T) {
	t.Parallel()
	r := newAutoRig(t)
	var cs calls
	r.p.policy.OnCall = cs.observe
	const prompt = "what is 1234 * 5678 - 91?" // the router sends numbers to big

	ctx := telemetry.WithRequest(context.Background(), slog.New(slog.DiscardHandler), "req-1")
	r.callCtx(t, ctx, "sk-a", zeroTemp("auto", prompt))
	c := cs.take(t)
	if c.RequestID != "req-1" || c.Key != "a" || c.RequestedModel != "auto" || c.Model != "big" || c.Provider != "fake" {
		t.Fatalf("auto call = %+v", c)
	}
	if c.Outcome != telemetry.OutcomeOK || c.Cache != cache.StatusMiss || c.Router == nil || c.Router.Model != "big" {
		t.Fatalf("auto call outcome/cache/router = %s %s %+v", c.Outcome, c.Cache, c.Router)
	}
	if c.Spent <= 0 || c.Usage.OutputTokens != 5 || c.Saved != 0 || c.Attempts != 0 {
		t.Fatalf("auto call spend = %+v", c)
	}
	spent := c.Spent

	r.callCtx(t, context.Background(), "sk-a", zeroTemp("auto", prompt))
	c = cs.take(t)
	if c.Outcome != telemetry.OutcomeCacheHit || c.Cache != cache.StatusExact || c.Spent != 0 || c.Saved != spent {
		t.Fatalf("cache hit = %+v, want saved %v", c, spent)
	}

	// Refused before any route: an unknown model.
	actx, err := r.p.Authenticate(context.Background(), "sk-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.Complete(actx, zeroTemp("nope", "hi")); err == nil {
		t.Fatal("unknown model accepted")
	}
	c = cs.take(t)
	if c.Outcome != telemetry.OutcomeError || c.ErrorKind != core.KindNotFound.String() || c.Model != "" || c.RequestedModel != "nope" {
		t.Fatalf("refused call = %+v", c)
	}

	// A stream, to its end and abandoned.
	s, err := r.p.Stream(actx, zeroTemp("small", "say something"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	c = cs.take(t)
	if !c.Stream || c.Outcome != telemetry.OutcomeOK || c.Model != "small" || c.Router != nil || c.Usage.OutputTokens != 5 {
		t.Fatalf("stream = %+v", c)
	}
	s, err = r.p.Stream(actx, zeroTemp("small", "say something else"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if c = cs.take(t); c.Outcome != telemetry.OutcomeAbandoned {
		t.Fatalf("abandoned stream = %+v", c)
	}
	r.cache.Wait()
}

func TestOnCallReportsRefusals(t *testing.T) {
	t.Parallel()
	key := config.Key{Name: "k", Key: "sk-k", Limits: config.Limits{Requests: &config.Rate{Rate: 1, Per: time.Minute, Burst: 1}}}
	p, _, _, _ := single(t, key, fake.New(fake.Config{Tokens: 3}))
	var cs calls
	p.policy.OnCall = cs.observe
	ctx, err := p.Authenticate(context.Background(), "sk-k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(ctx, zeroTemp("m", "one")); err != nil {
		t.Fatal(err)
	}
	if c := cs.take(t); c.Outcome != telemetry.OutcomeOK {
		t.Fatalf("first call = %+v", c)
	}
	if _, err := p.Complete(ctx, zeroTemp("m", "two")); core.AsError(err).Kind != core.KindRateLimited {
		t.Fatalf("second call err = %v, want rate limited", err)
	}
	if c := cs.take(t); c.Outcome != telemetry.OutcomeRateLimited || c.Model != "m" || c.ErrorKind != "" || c.Admitted {
		t.Fatalf("refused call = %+v", c)
	}
}

func drain(t *testing.T, s core.Stream) {
	t.Helper()
	defer func() { _ = s.Close() }()
	for {
		ev, err := s.Next()
		if err != nil {
			t.Fatalf("stream ended with %v before its stop event", err)
		}
		if ev.Kind == core.EventStop {
			return
		}
	}
}

// callCtx is autoRig.call on a given context.
func (r *autoRig) callCtx(t *testing.T, ctx context.Context, key string, req *core.Request) {
	t.Helper()
	ctx, _ = cache.WithRequest(ctx, "")
	ctx, err := r.p.Authenticate(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.cache.Wait()
	if _, err := r.p.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
}

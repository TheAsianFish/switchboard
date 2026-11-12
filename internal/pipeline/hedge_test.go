package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/hedge"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// slowFirst delays the first request it gets far past the hedge delay and
// answers every later one at once.
type slowFirst struct {
	*fake.Provider
	calls atomic.Int32
}

func (s *slowFirst) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	if s.calls.Add(1) == 1 {
		select {
		case <-ctx.Done():
			return nil, core.Wrap(core.KindTimeout, ctx.Err(), "cancelled")
		case <-time.After(5 * time.Second):
		}
	}
	return s.Provider.Stream(ctx, req)
}

// A hedged request is billed for the reply that reached the client and,
// at the prompt estimate, for the attempt that was cancelled: both were
// sent upstream.
func TestHedgedRequestIsChargedForTheCancelledAttempt(t *testing.T) {
	t.Parallel()
	cfg := hedge.DefaultConfig
	cfg.Hedge.Enabled, cfg.Hedge.Delay = true, 10*time.Millisecond
	up := &slowFirst{Provider: fake.New(fake.Config{Tokens: 5})}
	prov := hedge.New("x", up, cfg)
	p, _, _, met := single(t, config.Key{Name: "k", Key: "sk"}, prov)
	ctx, err := p.Authenticate(context.Background(), "sk")
	if err != nil {
		t.Fatal(err)
	}
	req := &core.Request{Model: "m", Stream: true, Messages: []core.Message{{Role: core.RoleUser, Text: "three little words"}}}
	start := time.Now()
	s, err := p.Stream(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := core.Collect(s)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the hedge did not answer")
	}
	if up.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.calls.Load())
	}
	// The reply's 3 input tokens, plus the cancelled attempt at the
	// prompt estimate.
	want := float64(resp.Usage.InputTokens + cost.EstimateInputTokens(req))
	if got := sum(t, []*telemetry.Metrics{met}, "switchboard_tokens_total", map[string]string{"type": "input"}); got != want {
		t.Fatalf("input tokens counted = %v, want %v", got, want)
	}
}

// hangs never answers before its caller goes away, and closes sent when
// its second call has arrived.
type hangs struct {
	*fake.Provider
	calls atomic.Int32
	sent  chan struct{}
}

func (h *hangs) wait(ctx context.Context) error {
	if h.calls.Add(1) == 2 {
		close(h.sent)
	}
	<-ctx.Done()
	return core.Wrap(core.KindTimeout, ctx.Err(), "cancelled")
}

func (h *hangs) Stream(ctx context.Context, _ *core.Request) (core.Stream, error) {
	return nil, h.wait(ctx)
}

func (h *hangs) Complete(ctx context.Context, _ *core.Request) (*core.Response, error) {
	return nil, h.wait(ctx)
}

// A client that leaves while a request and its hedge are both waiting is
// charged the prompt estimate for each attempt, streamed or not: both were
// sent upstream. The client leaves once the hedge has reached the
// provider, however long admission took.
func TestAbandonedHedgedRequestIsChargedForEveryAttempt(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		cfg := hedge.DefaultConfig
		cfg.Hedge.Enabled, cfg.Hedge.Delay = true, 5*time.Millisecond
		up := &hangs{Provider: fake.New(fake.Config{}), sent: make(chan struct{})}
		prov := hedge.New("x", up, cfg)
		p, _, _, met := single(t, config.Key{Name: "k", Key: "sk"}, prov)
		ctx, err := p.Authenticate(context.Background(), "sk")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-up.sent:
			case <-time.After(10 * time.Second):
			}
			cancel()
		}()
		req := &core.Request{Model: "m", Stream: stream, Messages: []core.Message{{Role: core.RoleUser, Text: "three little words"}}}
		if stream {
			_, err = p.Stream(ctx, req)
		} else {
			_, err = p.Complete(ctx, req)
		}
		cancel()
		if err == nil {
			t.Fatal("an abandoned request succeeded")
		}
		if n := up.calls.Load(); n != 2 {
			t.Fatalf("stream=%v: upstream calls = %d, want 2", stream, n)
		}
		want := float64(2 * cost.EstimateInputTokens(req))
		if got := sum(t, []*telemetry.Metrics{met}, "switchboard_tokens_total", map[string]string{"type": "input"}); got != want {
			t.Fatalf("stream=%v: input tokens counted = %v, want %v", stream, got, want)
		}
	}
}

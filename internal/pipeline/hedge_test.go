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

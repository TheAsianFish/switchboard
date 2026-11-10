package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/router"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// testWeights routes prompts without numbers to "small" and prompts with
// numbers to "big": small's predicted chance falls with the "numbers"
// feature, big's is always high. With dims > 0 they also take a (zero
// weighted) embedding of that many dimensions from the fake embedder.
func testWeights(t *testing.T, dims int) *router.Weights {
	t.Helper()
	d := len(router.CheapFeatureNames) + dims
	sc := router.Scaler{Mean: make([]float64, d), Std: make([]float64, d)}
	for i := range sc.Std {
		sc.Std[i] = 1
	}
	small := router.Logistic{Weights: make([]float64, d), Bias: 3}
	small.Weights[slices.Index(router.CheapFeatureNames, "numbers")] = -10
	big := router.Logistic{Weights: make([]float64, d), Bias: 5}
	w := &router.Weights{
		Version: router.WeightsVersion, Features: slices.Clone(router.CheapFeatureNames), Scaler: sc,
		CostUnit: router.CostGFLOPs, Fallback: "big", QualityTarget: 0.5,
		Candidates: []router.Candidate{
			{Model: "small", Cost: 1, Accuracy: 0.5, Logistic: small},
			{Model: "big", Cost: 10, Accuracy: 0.9, Logistic: big},
		},
	}
	if dims > 0 {
		w.Embedding, w.Dimensions = embed.FakeModel, dims
	}
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	return w
}

func newAutoPipeline(t *testing.T, e embed.Embedder, dims int) (*Pipeline, *fake.Provider, *telemetry.Metrics) {
	t.Helper()
	r, err := router.New(testWeights(t, dims), e, router.Options{Target: -1})
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Config{Tokens: 5})
	m := telemetry.NewMetrics(false)
	keys := auth.NewKeys([]config.Key{
		{Name: "all", Key: "sk-all"},
		{Name: "auto-only", Key: "sk-auto", Models: []string{AutoModel}},
		{Name: "auto-big", Key: "sk-auto-big", Models: []string{AutoModel, "big"}},
		{Name: "small", Key: "sk-small", Models: []string{"small"}},
	})
	return New(keys, []Route{
		{Model: "small", ProviderName: "fake", Upstream: "fake-small", Provider: f},
		{Model: "big", ProviderName: "fake", Upstream: "fake-big", Provider: f},
	}, Policy{Router: r, Metrics: m}), f, m
}

func TestAutoRoutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, key, prompt, want string
		stream                  bool
	}{
		{"easy prompt to the cheap model", "sk-all", "say hello", "small", false},
		{"numbers to the dear model", "sk-all", "what is 1234 * 5678 - 91?", "big", false},
		{"streamed", "sk-all", "what is 1234 * 5678 - 91?", "big", true},
		{"a key's model list caps the candidates", "sk-auto-big", "say hello", "big", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, f, m := newAutoPipeline(t, nil, 0)
			ctx, err := p.Authenticate(context.Background(), tt.key)
			if err != nil {
				t.Fatal(err)
			}
			req := &core.Request{Model: AutoModel, Stream: tt.stream, Messages: []core.Message{{Role: core.RoleUser, Text: tt.prompt}}}
			var model string
			if tt.stream {
				s, err := p.Stream(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := core.Collect(s)
				if err != nil {
					t.Fatal(err)
				}
				model = resp.Model
			} else {
				resp, err := p.Complete(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				model = resp.Model
			}
			if model != tt.want {
				t.Errorf("response model %q, want %q: the client learns which model answered", model, tt.want)
			}
			if got := f.LastRequest().Model; got != "fake-"+tt.want {
				t.Errorf("provider saw %q, want fake-%s", got, tt.want)
			}
			if req.Model != AutoModel {
				t.Errorf("caller's request was modified: %q", req.Model)
			}
			if n := sum(t, []*telemetry.Metrics{m}, "switchboard_router_decisions_total", map[string]string{"model": tt.want}); n != 1 {
				t.Errorf("router decisions for %s = %v, want 1", tt.want, n)
			}
		})
	}
}

func TestAutoPermissionsAndModels(t *testing.T) {
	t.Parallel()
	p, _, _ := newAutoPipeline(t, nil, 0)
	ctx, _ := p.Authenticate(context.Background(), "sk-small")
	_, err := p.Complete(ctx, &core.Request{Model: AutoModel, Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}})
	if core.AsError(err).Kind != core.KindPermission {
		t.Errorf("a key without auto got %v, want a permission error", err)
	}
	if got := p.Models(ctx); slices.Contains(got, AutoModel) {
		t.Errorf("models for a key without auto: %v", got)
	}
	ctx, _ = p.Authenticate(context.Background(), "sk-auto")
	_, err = p.Complete(ctx, &core.Request{Model: AutoModel, Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}})
	if core.AsError(err).Kind != core.KindPermission {
		t.Errorf("a key allowed auto but none of its candidates got %v, want a permission error", err)
	}
	ctx, _ = p.Authenticate(context.Background(), "sk-all")
	if got := p.Models(ctx); !slices.Contains(got, AutoModel) {
		t.Errorf("models %v: want auto listed", got)
	}

	plain, _ := newPipeline(t)
	ctx, _ = plain.Authenticate(context.Background(), "sk-all")
	_, err = plain.Complete(ctx, &core.Request{Model: AutoModel, Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}})
	if core.AsError(err).Kind != core.KindNotFound {
		t.Errorf("auto without a router: %v, want not found", err)
	}
}

func TestAutoFallsBackWhenTheEmbedderFails(t *testing.T) {
	t.Parallel()
	p, f, m := newAutoPipeline(t, &embed.Fake{Dims: 8, Err: errors.New("ollama is down")}, 8)
	ctx, _ := p.Authenticate(context.Background(), "sk-all")
	resp, err := p.Complete(ctx, &core.Request{Model: AutoModel, Messages: []core.Message{{Role: core.RoleUser, Text: "say hello"}}})
	if err != nil {
		t.Fatalf("an embedder failure failed the request: %v", err)
	}
	if resp.Model != "big" || !strings.HasSuffix(f.LastRequest().Model, "big") {
		t.Errorf("served by %q, want the fallback big", resp.Model)
	}
	if n := sum(t, []*telemetry.Metrics{m}, "switchboard_router_decisions_total", map[string]string{"model": "big", "reason": router.ReasonNoEmbedding}); n != 1 {
		t.Errorf("no_embedding decisions = %v, want 1", n)
	}

	ok, _, _ := newAutoPipeline(t, &embed.Fake{Dims: 8}, 8)
	ctx, _ = ok.Authenticate(context.Background(), "sk-all")
	resp, err = ok.Complete(ctx, &core.Request{Model: AutoModel, Messages: []core.Message{{Role: core.RoleUser, Text: "say hello"}}})
	if err != nil || resp.Model != "small" {
		t.Errorf("with a working embedder: %v, %v; want small", resp, err)
	}
}

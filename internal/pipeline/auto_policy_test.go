package pipeline

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/router"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// autoRig is an auto pipeline with priced routes, budgets and a cache.
// "small" and "big" have different prices and output estimates, so a
// request admitted, priced or cached as the wrong model shows. Keys "a"
// and "b" are in different tenants; both have a budget.
type autoRig struct {
	p      *Pipeline
	fake   *fake.Provider
	cache  *cache.Cache
	ledger *budget.Memory
}

var bigPrice = cost.Price{Base: cost.Rates{Input: 15000, Output: 75000}}

func newAutoRig(t *testing.T) *autoRig {
	t.Helper()
	r, err := router.New(testWeights(t, 0), nil, router.Options{Target: -1})
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Config{Tokens: 5})
	c := cache.New(cache.Options{
		KV:        cache.NewMemoryKV(100, time.Now),
		NewIndex:  func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 100, time.Now), nil },
		Embedder:  &embed.Fake{Dims: 16},
		Threshold: 0.9,
		TTL:       time.Hour,
	})
	led := budget.NewMemory(time.Minute, time.Now)
	keys := auth.NewKeys([]config.Key{
		{Name: "a", Key: "sk-a", Tenant: "team-a", Cache: "exact", Budget: budgetCfg},
		{Name: "b", Key: "sk-b", Tenant: "team-b", Cache: "exact", Budget: budgetCfg},
	})
	p := New(keys, []Route{
		{Model: "small", ProviderName: "fake", Upstream: "fake-small", Provider: f, Price: price, Priced: true, OutputEstimate: 16},
		{Model: "big", ProviderName: "fake", Upstream: "fake-big", Provider: f, Price: bigPrice, Priced: true, OutputEstimate: 64},
	}, Policy{
		Router: r, Ledger: led, Cache: c, Metrics: telemetry.NewMetrics(false),
		Limiter: ratelimit.NewMemoryFixedWindow(time.Now),
	})
	return &autoRig{p: p, fake: f, cache: c, ledger: led}
}

func (r *autoRig) call(t *testing.T, key string, req *core.Request) (*core.Response, string) {
	t.Helper()
	ctx, rep := cache.WithRequest(context.Background(), "")
	ctx, err := r.p.Authenticate(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.cache.Wait()
	resp, err := r.p.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	rep.WriteHeaders(h)
	return resp, h.Get(cache.StatusHeader)
}

func (r *autoRig) spent(t *testing.T, key string) int64 {
	t.Helper()
	pr, _ := auth.NewKeys([]config.Key{{Name: key, Key: "x", Budget: budgetCfg}}).Lookup("x")
	b, err := r.ledger.Balance(context.Background(), key, *pr.Budget)
	if err != nil {
		t.Fatal(err)
	}
	return int64(b.Spent)
}

// An auto request is charged, capped and priced as the model the router
// chose: the same request sent straight to that model costs the same,
// and differs from the other candidate's price.
func TestAutoIsAdmittedAsTheChosenModel(t *testing.T) {
	t.Parallel()
	const prompt = "what is 1234 * 5678 - 91?" // the router sends numbers to big
	spend := func(model string) (int64, *core.Request) {
		r := newAutoRig(t)
		t0 := 0.0
		r.call(t, "sk-a", &core.Request{Model: model, Temperature: &t0, Messages: []core.Message{{Role: core.RoleUser, Text: prompt}}})
		return r.spent(t, "a"), r.fake.LastRequest()
	}
	autoSpent, autoUp := spend(AutoModel)
	bigSpent, _ := spend("big")
	smallSpent, _ := spend("small")
	if autoSpent == 0 || autoSpent != bigSpent {
		t.Errorf("auto spent %d, want big's %d (small's is %d)", autoSpent, bigSpent, smallSpent)
	}
	if bigSpent == smallSpent {
		t.Fatalf("prices do not tell the models apart: %d", bigSpent)
	}
	if autoUp.Model != "fake-big" || autoUp.MaxTokens != 64 {
		t.Errorf("upstream got model %q max_tokens %d, want fake-big capped at big's estimate 64", autoUp.Model, autoUp.MaxTokens)
	}
}

// An auto reply is cached under the chosen model, inside the tenant: a
// direct request to that model hits it and so does a repeated auto
// request, both answered as the chosen model; another tenant misses.
func TestAutoCachesUnderTheChosenModel(t *testing.T) {
	t.Parallel()
	r := newAutoRig(t)
	t0 := 0.0
	req := func(model string) *core.Request {
		return &core.Request{Model: model, Temperature: &t0, Messages: []core.Message{{Role: core.RoleUser, Text: "what is 1234 * 5678 - 91?"}}}
	}
	steps := []struct {
		key, model, wantStatus string
	}{
		{"sk-a", AutoModel, string(cache.StatusMiss)},
		{"sk-a", "big", string(cache.StatusExact)},
		{"sk-a", AutoModel, string(cache.StatusExact)},
		{"sk-a", "small", string(cache.StatusMiss)},
		{"sk-b", AutoModel, string(cache.StatusMiss)},
	}
	for i, s := range steps {
		calls := r.fake.Calls()
		resp, status := r.call(t, s.key, req(s.model))
		if status != s.wantStatus {
			t.Errorf("step %d (%s, %s): cache %q, want %q", i, s.key, s.model, status, s.wantStatus)
		}
		if hit := r.fake.Calls() == calls; hit != (s.wantStatus == string(cache.StatusExact)) {
			t.Errorf("step %d: upstream called = %v on status %q", i, !hit, status)
		}
		want := s.model
		if want == AutoModel {
			want = "big"
		}
		if resp.Model != want {
			t.Errorf("step %d (%s): reply model %q, want %q", i, s.model, resp.Model, want)
		}
	}
}

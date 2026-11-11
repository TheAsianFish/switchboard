package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/router"
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

// countingEmbedder counts the embedding calls that reach the fake.
type countingEmbedder struct {
	embed.Fake
	n atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.n.Add(1)
	return c.Fake.Embed(ctx, texts)
}

// An auto request with the semantic cache on embeds its prompt once: the
// router and the cache use the same model on the same text, and share
// the vector through the request's memo.
func TestAutoAndSemanticCacheShareOneEmbedding(t *testing.T) {
	t.Parallel()
	const dims = 16
	inner := &countingEmbedder{Fake: embed.Fake{Dims: dims}}
	r, err := router.New(testWeights(t, dims), embed.Memoized(inner), router.Options{Target: -1})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cache.Options{
		KV:        cache.NewMemoryKV(100, time.Now),
		NewIndex:  func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 100, time.Now), nil },
		Embedder:  embed.Memoized(inner),
		Threshold: 0.99,
		TTL:       time.Hour,
	})
	f := fake.New(fake.Config{Tokens: 3})
	p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk-k", Cache: "semantic"}}), []Route{
		{Model: "small", ProviderName: "fake", Upstream: "fake-small", Provider: f},
		{Model: "big", ProviderName: "fake", Upstream: "fake-big", Provider: f},
	}, Policy{Router: r, Cache: c, Metrics: telemetry.NewMetrics(false)})
	var cs calls
	p.policy.OnCall = cs.observe

	ctx, err := p.Authenticate(context.Background(), "sk-k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(ctx, zeroTemp(AutoModel, "name a colour")); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	if got := cs.take(t); got.Cache != cache.StatusMiss || got.Router == nil {
		t.Fatalf("call = %+v, want a semantic miss routed by the router", got)
	}
	if n := inner.n.Load(); n != 1 {
		t.Fatalf("embedding calls = %d, want 1 shared by the router and the cache", n)
	}
	// The next request is a new memo: it embeds again.
	if _, err := p.Complete(ctx, zeroTemp(AutoModel, "name a fruit")); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	if n := inner.n.Load(); n != 2 {
		t.Fatalf("embedding calls = %d after a second request, want 2", n)
	}
}

// stepClock is a test clock that the embedder and the provider advance.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// slowEmbedder advances the clock on every call, as an embedding would.
type slowEmbedder struct {
	embed.Fake
	clk *stepClock
	d   time.Duration
}

func (e *slowEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.clk.Add(e.d)
	return e.Fake.Embed(ctx, texts)
}

// slowProvider advances the clock before answering, before its first token
// when streaming.
type slowProvider struct {
	core.Provider
	clk *stepClock
	d   time.Duration
}

func (p *slowProvider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	p.clk.Add(p.d)
	return p.Provider.Complete(ctx, req)
}

func (p *slowProvider) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	p.clk.Add(p.d)
	return p.Provider.Stream(ctx, req)
}

// A miss is timed from admission, like a hit and the duration metric: the
// semantic lookup's embedding counts, not just the upstream call.
func TestOnCallTimesAMissFromAdmission(t *testing.T) {
	t.Parallel()
	const embedTime, upstream = 40 * time.Millisecond, 10 * time.Millisecond
	for _, stream := range []bool{false, true} {
		clk := &stepClock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
		c := cache.New(cache.Options{
			KV:        cache.NewMemoryKV(100, clk.Now),
			NewIndex:  func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 100, clk.Now), nil },
			Embedder:  &slowEmbedder{Fake: embed.Fake{Dims: 16}, clk: clk, d: embedTime},
			Threshold: 0.9,
			TTL:       time.Hour,
		})
		keys := auth.NewKeys([]config.Key{{Name: "s", Key: "sk-s", Tenant: "t", Cache: "semantic"}})
		prov := &slowProvider{Provider: fake.New(fake.Config{Tokens: 3}), clk: clk, d: upstream}
		var cs calls
		p := New(keys, []Route{{Model: "m", ProviderName: "fake", Upstream: "fake-m", Provider: prov, Price: price, Priced: true, OutputEstimate: 8}},
			Policy{Cache: c, Metrics: telemetry.NewMetrics(false), OnCall: cs.observe})
		p.now = clk.Now

		ctx, _ := cache.WithRequest(context.Background(), "")
		ctx, err := p.Authenticate(ctx, "sk-s")
		if err != nil {
			t.Fatal(err)
		}
		req := zeroTemp("m", "a prompt nobody has sent before")
		req.Stream = stream
		if stream {
			s, err := p.Stream(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := core.Collect(s); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
		} else if _, err := p.Complete(ctx, req); err != nil {
			t.Fatal(err)
		}
		c.Wait()
		got := cs.take(t)
		if got.Cache != cache.StatusMiss {
			t.Fatalf("stream=%v: cache = %q, want a miss", stream, got.Cache)
		}
		if want := embedTime + upstream; got.Duration != want {
			t.Errorf("stream=%v: duration = %v, want %v (embedding + upstream)", stream, got.Duration, want)
		}
		if stream && got.TTFT != embedTime+upstream {
			t.Errorf("stream=%v: ttft = %v, want %v", stream, got.TTFT, embedTime+upstream)
		}
	}
}

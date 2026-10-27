package pipeline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// countingLimiter records the net units charged per key.
type countingLimiter struct {
	ratelimit.Limiter
	mu  sync.Mutex
	net map[string]int64
}

func (c *countingLimiter) Allow(ctx context.Context, key string, l ratelimit.Limit, n int64) (ratelimit.Decision, error) {
	d, err := c.Limiter.Allow(ctx, key, l, n)
	if err == nil && d.Allowed {
		c.mu.Lock()
		c.net[key] += n
		c.mu.Unlock()
	}
	return d, err
}

func (c *countingLimiter) Adjust(ctx context.Context, key string, l ratelimit.Limit, delta int64) error {
	c.mu.Lock()
	c.net[key] += delta
	c.mu.Unlock()
	return c.Limiter.Adjust(ctx, key, l, delta)
}

func (c *countingLimiter) charged(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.net[key]
}

type cacheRig struct {
	p       *Pipeline
	fake    *fake.Provider
	cache   *cache.Cache
	limiter *countingLimiter
	ledger  *budget.Memory
	metrics *telemetry.Metrics
}

var (
	budgetCfg = &config.KeyBudget{USD: "1", Period: "total"}
	tokenCfg  = &config.Rate{Rate: 1_000_000, Per: time.Minute, Burst: 1_000_000}
)

// newCacheRig builds a pipeline with an in-memory cache, limiter and
// ledger. Keys: "a1" and "a2" share tenant "team-a"; "b" is its own
// tenant; "exact" may only use the exact layer; "off" has no cache. a1
// has a budget, so its requests go upstream capped at the route's output
// estimate; a2 and the others send no cap.
func newCacheRig(t *testing.T) *cacheRig {
	t.Helper()
	keys := []config.Key{
		{Name: "a1", Key: "sk-a1", Tenant: "team-a", Cache: "semantic", Budget: budgetCfg, Limits: config.Limits{Tokens: tokenCfg}},
		{Name: "a2", Key: "sk-a2", Tenant: "team-a", Cache: "semantic"},
		{Name: "a3", Key: "sk-a3", Tenant: "team-a", Cache: "semantic"},
		{Name: "b", Key: "sk-b", Cache: "semantic"},
		{Name: "exact", Key: "sk-exact", Tenant: "team-a", Cache: "exact"},
		{Name: "off", Key: "sk-off", Tenant: "team-a", Cache: "off"},
	}
	f := fake.New(fake.Config{Tokens: 6})
	c := cache.New(cache.Options{
		KV:           cache.NewMemoryKV(100, time.Now),
		NewIndex:     func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 100, time.Now), nil },
		Embedder:     &embed.Fake{Dims: 64},
		Threshold:    0.9,
		MatchNumbers: true,
		TTL:          time.Hour,
	})
	lim := &countingLimiter{Limiter: ratelimit.NewMemoryFixedWindow(time.Now), net: map[string]int64{}}
	led := budget.NewMemory(time.Minute, time.Now)
	met := telemetry.NewMetrics(false)
	p := New(auth.NewKeys(keys), []Route{
		{Model: "m", ProviderName: "fake", Upstream: "fake-m", Provider: f, Price: price, Priced: true, OutputEstimate: 64},
		{Model: "m2", ProviderName: "fake", Upstream: "fake-m2", Provider: f, Price: price, Priced: true, OutputEstimate: 64},
	}, Policy{Limiter: lim, Ledger: led, Metrics: met, Cache: c})
	return &cacheRig{p: p, fake: f, cache: c, limiter: lim, ledger: led, metrics: met}
}

func deterministic(model, system, prompt string) *core.Request {
	t := 0.0
	return &core.Request{Model: model, System: system, Temperature: &t, Messages: []core.Message{{Role: core.RoleUser, Text: prompt}}}
}

// call sends req as key, streaming or not, and returns the reply text,
// the cache status the response headers would carry, and the error.
func (r *cacheRig) call(t *testing.T, key, directive string, req *core.Request, stream bool) (string, http.Header, error) {
	t.Helper()
	ctx, rep := cache.WithRequest(context.Background(), directive)
	ctx, err := r.p.Authenticate(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.cache.Wait()
	h := http.Header{}
	if !stream {
		resp, err := r.p.Complete(ctx, req)
		rep.WriteHeaders(h)
		if err != nil {
			return "", h, err
		}
		if resp.Model != req.Model {
			t.Errorf("model = %q, want the client's %q", resp.Model, req.Model)
		}
		return resp.Text, h, nil
	}
	s, err := r.p.Stream(ctx, req)
	rep.WriteHeaders(h)
	if err != nil {
		return "", h, err
	}
	resp, err := core.Collect(s)
	if err != nil {
		return "", h, err
	}
	if resp.Model != req.Model {
		t.Errorf("stream model = %q, want the client's %q", resp.Model, req.Model)
	}
	return resp.Text, h, nil
}

func TestCacheThroughPipeline(t *testing.T) {
	t.Parallel()
	q := "What is the capital of France?"
	reworded := "France: what is the capital of?"
	tests := []struct {
		name string
		// The first call fills, the second is the one checked.
		firstKey, secondKey       string
		first, second             *core.Request
		firstStream, secondStream bool
		directive                 string
		want                      cache.Status
	}{
		{"exact hit", "sk-a1", "sk-a1", deterministic("m", "", q), deterministic("m", "", q), false, false, "", cache.StatusExact},
		{"stream fills, complete hits", "sk-a1", "sk-a1", deterministic("m", "", q), deterministic("m", "", q), true, false, "", cache.StatusExact},
		{"complete fills, stream replays", "sk-a1", "sk-a1", deterministic("m", "", q), deterministic("m", "", q), false, true, "", cache.StatusExact},
		{"semantic hit", "sk-a1", "sk-a1", deterministic("m", "", q), deterministic("m", "", reworded), false, true, "", cache.StatusSemantic},
		{"keys of one tenant share", "sk-a2", "sk-a3", deterministic("m", "", q), deterministic("m", "", reworded), false, false, "", cache.StatusSemantic},
		{"a reply the gateway capped does not answer an uncapped request", "sk-a1", "sk-a2", deterministic("m", "", q), deterministic("m", "", q), false, false, "", cache.StatusMiss},
		{"other tenant misses exactly", "sk-a2", "sk-b", deterministic("m", "", q), deterministic("m", "", q), false, false, "", cache.StatusMiss},
		{"other tenant misses semantically", "sk-a2", "sk-b", deterministic("m", "", q), deterministic("m", "", reworded), false, false, "", cache.StatusMiss},
		{"other system prompt misses", "sk-a1", "sk-a1", deterministic("m", "Be brief.", q), deterministic("m", "Be verbose.", reworded), false, false, "", cache.StatusMiss},
		{"other model misses", "sk-a1", "sk-a1", deterministic("m", "", q), deterministic("m2", "", q), false, false, "", cache.StatusMiss},
		{"exact-only key gets no semantic hit", "sk-a2", "sk-exact", deterministic("m", "", q), deterministic("m", "", reworded), false, false, "", cache.StatusMiss},
		{"exact-only key still hits exactly", "sk-a2", "sk-exact", deterministic("m", "", q), deterministic("m", "", q), false, false, "", cache.StatusExact},
		{"key with the cache off skips", "sk-a2", "sk-off", deterministic("m", "", q), deterministic("m", "", q), false, false, "", cache.StatusSkip},
		{"directive lowers to exact", "sk-a1", "sk-a1", deterministic("m", "", q), deterministic("m", "", reworded), false, false, "exact", cache.StatusMiss},
		{"directive turns it off", "sk-a1", "sk-a1", deterministic("m", "", q), deterministic("m", "", q), false, false, "off", cache.StatusSkip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newCacheRig(t)
			firstText, h, err := r.call(t, tt.firstKey, "", tt.first, tt.firstStream)
			if err != nil {
				t.Fatal(err)
			}
			if got := h.Get(cache.StatusHeader); got != string(cache.StatusMiss) {
				t.Fatalf("first call status header = %q, want miss", got)
			}
			calls := r.fake.Calls()
			text, h, err := r.call(t, tt.secondKey, tt.directive, tt.second, tt.secondStream)
			if err != nil {
				t.Fatal(err)
			}
			if got := h.Get(cache.StatusHeader); got != string(tt.want) {
				t.Fatalf("status header = %q, want %q", got, tt.want)
			}
			hit := tt.want == cache.StatusExact || tt.want == cache.StatusSemantic
			if upstream := r.fake.Calls() - calls; (upstream == 0) != hit {
				t.Errorf("upstream calls on the second request = %d, hit = %v", upstream, hit)
			}
			if hit && text != firstText {
				t.Errorf("cached text = %q, want the first reply %q", text, firstText)
			}
			if (h.Get(cache.SimilarityHeader) != "") != (tt.want == cache.StatusSemantic) {
				t.Errorf("similarity header = %q on %s", h.Get(cache.SimilarityHeader), tt.want)
			}
		})
	}
}

// A hit costs nothing: its token-limit charge and budget reservation are
// returned in full, and the request is counted as a cache hit.
func TestCacheHitIsFree(t *testing.T) {
	t.Parallel()
	r := newCacheRig(t)
	req := deterministic("m", "", "What is the capital of France?")
	if _, _, err := r.call(t, "sk-a1", "", req, false); err != nil {
		t.Fatal(err)
	}
	pr, _ := auth.NewKeys([]config.Key{{Name: "a1", Key: "x", Budget: budgetCfg}}).Lookup("x")
	before, err := r.ledger.Balance(context.Background(), "a1", *pr.Budget)
	if err != nil {
		t.Fatal(err)
	}
	tokens := r.limiter.charged(tokensKey(pr))
	if before.Spent == 0 || tokens == 0 {
		t.Fatalf("the miss should cost something: spent %v, tokens %d", before.Spent, tokens)
	}
	for _, stream := range []bool{false, true} {
		if _, h, err := r.call(t, "sk-a1", "", req, stream); err != nil || h.Get(cache.StatusHeader) != string(cache.StatusExact) {
			t.Fatalf("stream=%v: %v %v", stream, h, err)
		}
	}
	after, _ := r.ledger.Balance(context.Background(), "a1", *pr.Budget)
	if after.Spent != before.Spent || after.Reserved != 0 {
		t.Errorf("budget after hits: spent %v reserved %v, want spent %v reserved 0", after.Spent, after.Reserved, before.Spent)
	}
	if got := r.limiter.charged(tokensKey(pr)); got != tokens {
		t.Errorf("token charge after hits = %d, want %d", got, tokens)
	}
	if got := sum(t, []*telemetry.Metrics{r.metrics}, "switchboard_requests_total", map[string]string{"key": "a1", "outcome": telemetry.OutcomeCacheHit}); got != 2 {
		t.Errorf("cache_hit requests = %v, want 2", got)
	}
	if got := sum(t, []*telemetry.Metrics{r.metrics}, "switchboard_cache_saved_dollars_total", map[string]string{"key": "a1"}); got <= 0 {
		t.Errorf("saved dollars = %v, want > 0", got)
	}
}

func TestCacheabilityInPipeline(t *testing.T) {
	t.Parallel()
	t.Run("sampling temperature is not cached", func(t *testing.T) {
		t.Parallel()
		r := newCacheRig(t)
		req := deterministic("m", "", "hello")
		req.Temperature = nil
		for range 2 {
			if _, h, err := r.call(t, "sk-a1", "", req, false); err != nil || h.Get(cache.StatusHeader) != string(cache.StatusSkip) {
				t.Fatalf("%v %v", h, err)
			}
		}
		if r.fake.Calls() != 2 {
			t.Errorf("upstream calls = %d, want 2", r.fake.Calls())
		}
	})
	t.Run("the directive opts a sampled request in", func(t *testing.T) {
		t.Parallel()
		r := newCacheRig(t)
		req := deterministic("m", "", "hello")
		req.Temperature = nil
		_, _, _ = r.call(t, "sk-a1", "semantic", req, false)
		if _, h, _ := r.call(t, "sk-a1", "semantic", req, false); h.Get(cache.StatusHeader) != string(cache.StatusExact) {
			t.Errorf("status = %q", h.Get(cache.StatusHeader))
		}
	})
	t.Run("a bad directive is refused before admission", func(t *testing.T) {
		t.Parallel()
		r := newCacheRig(t)
		_, _, err := r.call(t, "sk-a1", "always", deterministic("m", "", "hello"), false)
		if err == nil || core.AsError(err).Kind != core.KindInvalidRequest {
			t.Fatalf("err = %v, want invalid request", err)
		}
		if r.limiter.charged("tok:a1") != 0 || r.fake.Calls() != 0 {
			t.Error("a refused directive charged the key or reached the provider")
		}
	})
	t.Run("an abandoned stream is not stored", func(t *testing.T) {
		t.Parallel()
		r := newCacheRig(t)
		req := deterministic("m", "", "hello")
		ctx, _ := r.p.Authenticate(context.Background(), "sk-a1")
		s, err := r.p.Stream(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		r.cache.Wait()
		if _, h, _ := r.call(t, "sk-a1", "", req, false); h.Get(cache.StatusHeader) != string(cache.StatusMiss) {
			t.Errorf("after an abandoned stream: %q, want miss", h.Get(cache.StatusHeader))
		}
	})
}

// The cache key covers where the model name is routed: after an operator
// points a model at another upstream model or provider, or while replicas
// of a rolling deploy disagree, one route's replies never answer another.
func TestCacheKeyCoversRoute(t *testing.T) {
	t.Parallel()
	for name, second := range map[string]Route{
		"upstream model": {Model: "m", ProviderName: "fake", Upstream: "big-model"},
		"provider":       {Model: "m", ProviderName: "other", Upstream: "small-model"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := cache.New(cache.Options{KV: cache.NewMemoryKV(10, time.Now), TTL: time.Hour})
			keys := auth.NewKeys([]config.Key{{Name: "k", Key: "sk", Cache: "exact"}})
			f := fake.New(fake.Config{Tokens: 3})
			first := New(keys, []Route{{Model: "m", ProviderName: "fake", Upstream: "small-model", Provider: f}}, Policy{Cache: c})
			second.Provider = f
			remapped := New(keys, []Route{second}, Policy{Cache: c})
			req := deterministic("m", "", "What is the capital of France?")
			for i, p := range []*Pipeline{first, first, remapped} {
				ctx, rep := cache.WithRequest(context.Background(), "")
				ctx, _ = p.Authenticate(ctx, "sk")
				if _, err := p.Complete(ctx, req); err != nil {
					t.Fatal(err)
				}
				c.Wait()
				h := http.Header{}
				rep.WriteHeaders(h)
				want := []cache.Status{cache.StatusMiss, cache.StatusExact, cache.StatusMiss}[i]
				if got := h.Get(cache.StatusHeader); got != string(want) {
					t.Fatalf("call %d: status %q, want %q", i+1, got, want)
				}
			}
		})
	}
}

// cutShort streams a start event and two deltas, then ends with err: io.EOF
// for an upstream that hangs up without a stop event, or a read error.
type cutShort struct{ err error }

func (c cutShort) Complete(context.Context, *core.Request) (*core.Response, error) {
	return nil, errors.New("not used")
}

func (c cutShort) Stream(context.Context, *core.Request) (core.Stream, error) {
	return &failingStream{SliceStream: core.SliceStream{Events: []core.Event{
		{Kind: core.EventStart, Usage: core.Usage{InputTokens: 3}},
		{Kind: core.EventDelta, Text: "Paris is "},
		{Kind: core.EventDelta, Text: "the"},
	}}, err: c.err}, nil
}

type failingStream struct {
	core.SliceStream
	err error
}

func (f *failingStream) Next() (core.Event, error) {
	ev, err := f.SliceStream.Next()
	if errors.Is(err, io.EOF) {
		return ev, f.err
	}
	return ev, err
}

// A stream that ends before its stop event is not a whole reply and is not
// stored, whether the upstream hung up or failed mid-stream.
func TestStreamCutShortIsNotStored(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"eof without stop": io.EOF,
		"mid-stream error": core.Errorf(core.KindOverloaded, "connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := cache.New(cache.Options{KV: cache.NewMemoryKV(10, time.Now), TTL: time.Hour})
			p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk", Cache: "exact"}}),
				[]Route{{Model: "m", ProviderName: "fake", Upstream: "u", Provider: cutShort{err: err}}}, Policy{Cache: c})
			req := deterministic("m", "", "What is the capital of France?")
			// The second identical request must miss and go upstream again.
			for i := range 2 {
				ctx, rep := cache.WithRequest(context.Background(), "")
				ctx, _ = p.Authenticate(ctx, "sk")
				s, serr := p.Stream(ctx, req)
				if serr != nil {
					t.Fatal(serr)
				}
				h := http.Header{}
				rep.WriteHeaders(h)
				if got := h.Get(cache.StatusHeader); got != string(cache.StatusMiss) {
					t.Fatalf("call %d: status %q, want miss", i+1, got)
				}
				if _, cerr := core.Collect(s); cerr == nil {
					t.Fatalf("call %d: the stream did not fail", i+1)
				}
				_ = s.Close()
				c.Wait()
			}
		})
	}
}

package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// target is the route every test request is looked up under.
var target = cache.Upstream{Provider: "p", Model: "upstream-m"}

func zero() *float64 { v := 0.0; return &v }

// req builds a deterministic request whose last user turn is prompt, after
// the given earlier turns.
func req(model, system, prompt string, earlier ...string) *core.Request {
	r := &core.Request{Model: model, System: system, Temperature: zero()}
	for i, t := range earlier {
		role := core.RoleUser
		if i%2 == 1 {
			role = core.RoleAssistant
		}
		r.Messages = append(r.Messages, core.Message{Role: role, Text: t})
	}
	r.Messages = append(r.Messages, core.Message{Role: core.RoleUser, Text: prompt})
	return r
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newCache builds a cache over the in-memory stores and the fake embedder,
// which ignores word order: a reordered prompt has similarity 1.
func newCache(t *testing.T, kv cache.KV, emb embed.Embedder, threshold float64) (*cache.Cache, *clock) {
	t.Helper()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	if kv == nil {
		kv = cache.NewMemoryKV(100, clk.now)
	}
	if emb == nil {
		emb = &embed.Fake{Dims: 64}
	}
	return cache.New(cache.Options{
		KV:           kv,
		NewIndex:     func(dims int) (cache.Index, error) { return cache.NewMemoryIndex(dims, 100, clk.now), nil },
		Embedder:     emb,
		Threshold:    threshold,
		MatchNumbers: true,
		TTL:          time.Hour,
		Now:          clk.now,
	}), clk
}

var answer = &core.Response{Text: "Paris is the capital.", StopReason: core.StopEndTurn, Usage: core.Usage{InputTokens: 7, OutputTokens: 5}}

// fill looks r up (expecting a miss) and stores answer as its reply.
func fill(t *testing.T, c *cache.Cache, tenant string, mode cache.Mode, r *core.Request) {
	t.Helper()
	l := c.Lookup(context.Background(), tenant, target, mode, r)
	if l.Status != cache.StatusMiss {
		t.Fatalf("fill: lookup status = %s (%s), want miss", l.Status, l.Reason)
	}
	c.Fill(context.Background(), l, answer)
	c.Wait()
}

func TestLookupLayers(t *testing.T) {
	t.Parallel()
	base := req("m", "Be brief.", "What is the capital of France?")
	tests := []struct {
		name string
		mode cache.Mode
		// query is looked up after base was filled for tenant "a".
		query      *core.Request
		tenant     string
		threshold  float64
		wantStatus cache.Status
	}{
		{"same request hits exactly", cache.ModeSemantic, base, "a", 0.9, cache.StatusExact},
		{"trailing whitespace and line endings are ignored", cache.ModeExact, req("m", "Be brief.\r\n", "\nWhat is the capital of France?  \r\n"), "a", 0.9, cache.StatusExact},
		{"leading spaces are significant", cache.ModeExact, req("m", "Be brief.", "  What is the capital of France?"), "a", 0.9, cache.StatusMiss},
		{"reworded prompt hits semantically", cache.ModeSemantic, req("m", "Be brief.", "France: what is the capital of?"), "a", 0.9, cache.StatusSemantic},
		{"reworded prompt misses in exact mode", cache.ModeExact, req("m", "Be brief.", "France: what is the capital of?"), "a", 0.9, cache.StatusMiss},
		{"inner whitespace is significant to the exact layer", cache.ModeExact, req("m", "Be brief.", "What is the  capital of France?"), "a", 0.9, cache.StatusMiss},
		{"below the threshold misses", cache.ModeSemantic, req("m", "Be brief.", "What is the capital of Spain?"), "a", 0.9, cache.StatusMiss},
		{"other tenant never hits exactly", cache.ModeSemantic, base, "b", 0.0001, cache.StatusMiss},
		{"other tenant never hits semantically", cache.ModeSemantic, req("m", "Be brief.", "France: what is the capital of?"), "b", 0.0001, cache.StatusMiss},
		{"other system prompt is another context", cache.ModeSemantic, req("m", "Be verbose.", "France: what is the capital of?"), "a", 0.0001, cache.StatusMiss},
		{"other earlier turns are another context", cache.ModeSemantic, req("m", "Be brief.", "France: what is the capital of?", "Hi", "Hello!"), "a", 0.0001, cache.StatusMiss},
		{"other model is another context", cache.ModeSemantic, req("m2", "Be brief.", "What is the capital of France?"), "a", 0.0001, cache.StatusMiss},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newCache(t, nil, nil, tt.threshold)
			fill(t, c, "a", cache.ModeSemantic, base)
			l := c.Lookup(context.Background(), tt.tenant, target, tt.mode, tt.query)
			if l.Status != tt.wantStatus {
				t.Fatalf("status = %s (%s), want %s", l.Status, l.Reason, tt.wantStatus)
			}
			hit := tt.wantStatus == cache.StatusExact || tt.wantStatus == cache.StatusSemantic
			if hit != (l.Response != nil) {
				t.Fatalf("response = %v on status %s", l.Response, l.Status)
			}
			if hit && (l.Response.Text != answer.Text || l.Response.Usage != answer.Usage || l.Response.StopReason != answer.StopReason) {
				t.Errorf("response = %+v, want %+v", l.Response, answer)
			}
			if tt.wantStatus == cache.StatusSemantic && l.Similarity < tt.threshold {
				t.Errorf("similarity %v below threshold %v", l.Similarity, tt.threshold)
			}
		})
	}
}

// The context changes with every sampling parameter: a reply generated
// under one max_tokens or stop list must not answer another.
func TestContextCoversParameters(t *testing.T) {
	t.Parallel()
	base := req("m", "", "Count to ten.")
	k0, ok := cache.KeysFor("a", target, base)
	if !ok {
		t.Fatal("base not cacheable")
	}
	half := 0.5
	variants := map[string]func(r *core.Request){
		"max_tokens":  func(r *core.Request) { r.MaxTokens = 5 },
		"temperature": func(r *core.Request) { r.Temperature = &half },
		"top_p":       func(r *core.Request) { r.TopP = &half },
		"stop":        func(r *core.Request) { r.Stop = []string{"five"} },
		"system":      func(r *core.Request) { r.System = "x" },
		"model":       func(r *core.Request) { r.Model = "m2" },
		"earlier": func(r *core.Request) {
			r.Messages = append([]core.Message{{Role: core.RoleUser, Text: "x"}, {Role: core.RoleAssistant, Text: "y"}}, r.Messages...)
		},
	}
	for name, mutate := range variants {
		r := *base
		r.Messages = append([]core.Message(nil), base.Messages...)
		mutate(&r)
		k, _ := cache.KeysFor("a", target, &r)
		if k.Context == k0.Context || k.Exact == k0.Exact {
			t.Errorf("%s: same keys as the base request", name)
		}
	}
	// Who said each earlier turn is part of the context, not only what.
	earlier := func(first, second core.Role) *core.Request {
		r := *base
		r.Messages = append([]core.Message{{Role: first, Text: "x"}, {Role: second, Text: "y"}}, base.Messages...)
		return &r
	}
	kUA, _ := cache.KeysFor("a", target, earlier(core.RoleUser, core.RoleAssistant))
	kAU, _ := cache.KeysFor("a", target, earlier(core.RoleAssistant, core.RoleUser))
	if kUA.Context == kAU.Context || kUA.Exact == kAU.Exact {
		t.Error("swapping the roles of earlier turns kept the keys")
	}
	// The route is part of the context: a model remapped to another
	// upstream model or provider starts afresh.
	for name, other := range map[string]cache.Upstream{
		"upstream model": {Provider: target.Provider, Model: "other"},
		"provider":       {Provider: "other", Model: target.Model},
	} {
		if k, _ := cache.KeysFor("a", other, base); k.Context == k0.Context || k.Exact == k0.Exact {
			t.Errorf("%s: same keys as the base request", name)
		}
	}
	// The stream flag is not part of the key: a streamed reply answers a
	// non-streaming request and back.
	streamed := *base
	streamed.Stream = true
	if k, _ := cache.KeysFor("a", target, &streamed); k != k0 {
		t.Error("stream flag changed the keys")
	}
	// Tenants differ in namespace and in every hash.
	if k, _ := cache.KeysFor("b", target, base); k.Namespace == k0.Namespace || k.Context == k0.Context || k.Exact == k0.Exact {
		t.Error("another tenant shares a key")
	}
}

func TestNumberGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		guard       bool
		stored, ask string
		want        cache.Status
	}{
		// The fake embedder sees "has 3 apples" and "has 4 apples" as
		// close; the guard is what tells them apart.
		{"changed number refused", true, "Tom has 3 apples and eats 1. How many are left?", "Tom has 4 apples and eats 1. How many are left?", cache.StatusMiss},
		{"changed number served without the guard", false, "Tom has 3 apples and eats 1. How many are left?", "Tom has 4 apples and eats 1. How many are left?", cache.StatusSemantic},
		{"spelled-out number matches digits", true, "Tom has 3 apples and eats one. How many are left?", "Tom has three apples and eats 1. How many are left?", cache.StatusSemantic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			c := cache.New(cache.Options{
				KV:           cache.NewMemoryKV(10, clk.now),
				NewIndex:     func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 10, clk.now), nil },
				Embedder:     &embed.Fake{Dims: 8},
				Threshold:    0.5,
				MatchNumbers: tt.guard,
				TTL:          time.Hour,
				Now:          clk.now,
			})
			fill(t, c, "a", cache.ModeSemantic, req("m", "", tt.stored))
			if got := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, req("m", "", tt.ask)).Status; got != tt.want {
				t.Errorf("status = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()
	c, clk := newCache(t, nil, nil, 0.9)
	base := req("m", "", "What is the capital of France?")
	fill(t, c, "a", cache.ModeSemantic, base)
	clk.add(59 * time.Minute)
	if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, base).Status; s != cache.StatusExact {
		t.Fatalf("before expiry: %s", s)
	}
	clk.add(2 * time.Minute)
	if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, req("m", "", "France: what is the capital of?")).Status; s != cache.StatusMiss {
		t.Fatalf("after expiry: %s, want miss", s)
	}
}

func TestFillSkipsIncompleteReplies(t *testing.T) {
	t.Parallel()
	for name, resp := range map[string]*core.Response{
		"content filter": {Text: "partial", StopReason: core.StopContentFilter},
		"empty":          {Text: "", StopReason: core.StopEndTurn},
	} {
		c, _ := newCache(t, nil, nil, 0.9)
		r := req("m", "", "hello there")
		l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, r)
		c.Fill(context.Background(), l, resp)
		c.Wait()
		if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, r).Status; s != cache.StatusMiss {
			t.Errorf("%s: stored (%s)", name, s)
		}
	}
}

// withTools returns base with one tool and a finished tool round trip
// before the last turn, then lets mutate change one thing.
func withTools(mutate func(r *core.Request)) *core.Request {
	r := req("m", "", "and tomorrow?")
	r.Tools = []core.Tool{{Name: "weather", Description: "Weather for a city", Parameters: json.RawMessage(`{"type":"object"}`)}}
	r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
	last := r.Messages[0]
	r.Messages = []core.Message{
		{Role: core.RoleUser, Text: "weather in Paris?"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "c1", Name: "weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}}},
		{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "c1", Content: "18 C"}}},
		{Role: core.RoleAssistant, Text: "18 C in Paris."},
		last,
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

// Two requests that differ only in tool content never share an entry.
func TestExactKeyCoversTools(t *testing.T) {
	t.Parallel()
	results := func(rs ...core.ToolResult) func(r *core.Request) {
		return func(r *core.Request) {
			r.Messages = append(r.Messages[:4], core.Message{Role: core.RoleUser, ToolResults: rs})
		}
	}
	base := withTools(nil)
	resultBase := withTools(results(core.ToolResult{CallID: "c2", Content: "rain"}))
	tests := []struct {
		name  string
		base  *core.Request
		query *core.Request
	}{
		{"no tools", base, withTools(func(r *core.Request) { r.Tools = nil })},
		{"tool name", base, withTools(func(r *core.Request) { r.Tools[0].Name = "forecast" })},
		{"tool description", base, withTools(func(r *core.Request) { r.Tools[0].Description = "Weather" })},
		{"tool schema", base, withTools(func(r *core.Request) {
			r.Tools[0].Parameters = json.RawMessage(`{"type":"object","required":["city"]}`)
		})},
		{"extra tool", base, withTools(func(r *core.Request) { r.Tools = append(r.Tools, core.Tool{Name: "time"}) })},
		{"tool choice mode", base, withTools(func(r *core.Request) { r.ToolChoice.Mode = core.ToolChoiceAny })},
		{"tool choice unset", base, withTools(func(r *core.Request) { r.ToolChoice = core.ToolChoice{} })},
		{"tool choice name", base, withTools(func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceTool, Name: "weather"} })},
		{"parallel calls", base, withTools(func(r *core.Request) { r.ToolChoice.DisableParallel = true })},
		{"call id", base, withTools(func(r *core.Request) { r.Messages[1].ToolCalls[0].ID = "c9" })},
		{"call name", base, withTools(func(r *core.Request) { r.Messages[1].ToolCalls[0].Name = "forecast" })},
		{"call arguments", base, withTools(func(r *core.Request) { r.Messages[1].ToolCalls[0].Arguments = json.RawMessage(`{"city":"Rome"}`) })},
		{"earlier result content", base, withTools(func(r *core.Request) { r.Messages[2].ToolResults[0].Content = "30 C" })},
		{"earlier result error flag", base, withTools(func(r *core.Request) { r.Messages[2].ToolResults[0].IsError = true })},
		{"last turn result content", resultBase, withTools(results(core.ToolResult{CallID: "c2", Content: "snow"}))},
		{"last turn result id", resultBase, withTools(results(core.ToolResult{CallID: "c3", Content: "rain"}))},
		{"last turn result error flag", resultBase, withTools(results(core.ToolResult{CallID: "c2", Content: "rain", IsError: true}))},
		{"last turn extra result", resultBase, withTools(results(core.ToolResult{CallID: "c2", Content: "rain"}, core.ToolResult{CallID: "c3"}))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newCache(t, nil, nil, 0.0001)
			fill(t, c, "a", cache.ModeExact, tt.base)
			if s := c.Lookup(context.Background(), "a", target, cache.ModeExact, tt.base).Status; s != cache.StatusExact {
				t.Fatalf("same request: %s, want exact hit", s)
			}
			if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, tt.query).Status; s != cache.StatusMiss {
				t.Errorf("changed request: %s, want miss", s)
			}
		})
	}
}

// The semantic layer does not serve requests with tool content, even when
// the caller asks for it: the reordered prompt below has similarity 1 to
// the stored one under the fake embedder, in the same context.
func TestSemanticSkipsToolContent(t *testing.T) {
	t.Parallel()
	c, _ := newCache(t, nil, nil, 0.0001)
	fill(t, c, "a", cache.ModeSemantic, withTools(nil))
	reworded := withTools(func(r *core.Request) { r.Messages[4].Text = "tomorrow and?" })
	if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, reworded).Status; s != cache.StatusMiss {
		t.Errorf("reworded tool request: %s, want miss", s)
	}
}

func TestFillSkipsToolCalls(t *testing.T) {
	t.Parallel()
	call := []core.ToolCall{{ID: "c", Name: "weather", Arguments: json.RawMessage(`{}`)}}
	tests := []struct {
		name  string
		req   *core.Request
		resp  *core.Response
		store bool
	}{
		{"tool calls", withTools(nil), &core.Response{Text: "Let me check.", ToolCalls: call, StopReason: core.StopToolUse}, false},
		{"tool_use without calls (streamed fill)", withTools(nil), &core.Response{Text: "Let me check.", StopReason: core.StopToolUse}, false},
		{"tool request cut off", withTools(nil), &core.Response{Text: "Let me", StopReason: core.StopMaxTokens}, false},
		{"tool request answered", withTools(nil), &core.Response{Text: "Rain tomorrow.", StopReason: core.StopEndTurn}, true},
		{"plain request cut off", req("m", "", "hi"), &core.Response{Text: "Hel", StopReason: core.StopMaxTokens}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newCache(t, nil, nil, 0.9)
			l := c.Lookup(context.Background(), "a", target, cache.ModeExact, tt.req)
			c.Fill(context.Background(), l, tt.resp)
			c.Wait()
			got := c.Lookup(context.Background(), "a", target, cache.ModeExact, tt.req)
			if (got.Status == cache.StatusExact) != tt.store {
				t.Errorf("status %s, stored = %v", got.Status, tt.store)
			}
		})
	}
}

func TestSkips(t *testing.T) {
	t.Parallel()
	c, _ := newCache(t, nil, nil, 0.9)
	if l := c.Lookup(context.Background(), "a", target, cache.ModeOff, req("m", "", "hi")); l.Status != cache.StatusSkip || l.Fillable() {
		t.Errorf("off: %+v", l)
	}
	prefill := req("m", "", "hi")
	prefill.Messages = append(prefill.Messages, core.Message{Role: core.RoleAssistant, Text: "Once upon"})
	if l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, prefill); l.Status != cache.StatusSkip {
		t.Errorf("assistant prefill: %+v", l)
	}
}

// failingKV fails every call while down is set.
type failingKV struct {
	cache.KV
	mu   sync.Mutex
	down bool
	gets int
}

func (f *failingKV) Get(ctx context.Context, k string) ([]byte, bool, error) {
	f.mu.Lock()
	f.gets++
	down := f.down
	f.mu.Unlock()
	if down {
		return nil, false, errors.New("connection refused")
	}
	return f.KV.Get(ctx, k)
}

func (f *failingKV) set(down bool) { f.mu.Lock(); f.down = down; f.mu.Unlock() }

// After a store error the cache is bypassed for the retry interval, so an
// outage costs one timeout per interval rather than one per request.
func TestStoreOutageBypasses(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	kv := &failingKV{KV: cache.NewMemoryKV(10, clk.now), down: true}
	c := cache.New(cache.Options{KV: kv, TTL: time.Hour, RetryInterval: time.Second, Now: clk.now})
	r := req("m", "", "hi")
	if l := c.Lookup(context.Background(), "a", target, cache.ModeExact, r); l.Status != cache.StatusError || l.Fillable() {
		t.Fatalf("first lookup: %+v", l)
	}
	for range 5 {
		if l := c.Lookup(context.Background(), "a", target, cache.ModeExact, r); l.Status != cache.StatusSkip {
			t.Fatalf("during bypass: %+v", l)
		}
	}
	if kv.gets != 1 {
		t.Errorf("store called %d times during the bypass, want 1", kv.gets)
	}
	kv.set(false)
	clk.add(time.Second)
	if l := c.Lookup(context.Background(), "a", target, cache.ModeExact, r); l.Status != cache.StatusMiss {
		t.Errorf("after the interval: %+v", l)
	}
}

// logBuffer collects log output written from several goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The first store call that succeeds after a failure logs, at warn like the
// failure, that the store is back; later ones do not.
func TestStoreRecoveryLogged(t *testing.T) {
	t.Parallel()
	const recovered = `level=WARN msg="cache store reachable again"`
	tests := []struct {
		name string
		// outage takes the store down, lets the bypass pass and brings it
		// back. It is given a lookup made before the outage.
		outage bool
		// fillFirst fills the lookup made before the outage once the store
		// is back, so the first call to succeed is a Set.
		fillFirst bool
		wantOp    string
	}{
		{name: "no outage logs nothing"},
		{name: "get after an outage", outage: true, wantOp: "get"},
		{name: "set after an outage", outage: true, fillFirst: true, wantOp: "set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logs := &logBuffer{}
			ctx := telemetry.WithRequest(context.Background(), slog.New(slog.NewTextHandler(logs, nil)), "req_test")
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			kv := &failingKV{KV: cache.NewMemoryKV(10, clk.now)}
			c := cache.New(cache.Options{KV: kv, TTL: time.Hour, RetryInterval: time.Second, Now: clk.now})
			before := c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", "before"))
			if before.Status != cache.StatusMiss {
				t.Fatalf("lookup before the outage: %+v", before)
			}
			if tt.outage {
				kv.set(true)
				if l := c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", "during")); l.Status != cache.StatusError {
					t.Fatalf("lookup in the outage: %+v", l)
				}
				kv.set(false)
				clk.add(time.Second)
			}
			if tt.fillFirst {
				c.Fill(ctx, before, answer)
				c.Wait()
			}
			for i := range 3 {
				l := c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", fmt.Sprintf("after %d", i)))
				if l.Status != cache.StatusMiss {
					t.Fatalf("lookup after the outage: %+v", l)
				}
				c.Fill(ctx, l, answer)
				c.Wait()
			}
			out := logs.String()
			want := 0
			if tt.wantOp != "" {
				want = 1
			}
			if n := strings.Count(out, recovered); n != want {
				t.Fatalf("%d recovery lines, want %d; logs:\n%s", n, want, out)
			}
			if want == 1 && !strings.Contains(out, recovered+" request_id=req_test op="+tt.wantOp) {
				t.Errorf("recovery line without op=%s; logs:\n%s", tt.wantOp, out)
			}
		})
	}
}

// slowKV holds the first Get until released, so a call can be in flight
// while another one fails.
type slowKV struct {
	*failingKV
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *slowKV) Get(ctx context.Context, k string) ([]byte, bool, error) {
	first := false
	s.once.Do(func() { first = true })
	if first {
		close(s.entered)
		<-s.release
		return s.KV.Get(ctx, k)
	}
	return s.failingKV.Get(ctx, k)
}

// A call that was in flight when the store failed, and succeeds afterwards,
// says nothing about the store now: it must not log a recovery in the middle
// of the outage. The first call that starts after the failure and succeeds
// does.
func TestStoreRecoveryIgnoresCallsFromBeforeTheFailure(t *testing.T) {
	t.Parallel()
	const recovered = `level=WARN msg="cache store reachable again"`
	logs := &logBuffer{}
	ctx := telemetry.WithRequest(context.Background(), slog.New(slog.NewTextHandler(logs, nil)), "req_test")
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	kv := &slowKV{failingKV: &failingKV{KV: cache.NewMemoryKV(10, clk.now)}, entered: make(chan struct{}), release: make(chan struct{})}
	c := cache.New(cache.Options{KV: kv, TTL: time.Hour, Timeout: time.Minute, RetryInterval: time.Second, Now: clk.now})

	done := make(chan *cache.Lookup)
	go func() { done <- c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", "in flight")) }()
	<-kv.entered
	kv.set(true)
	if l := c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", "fails")); l.Status != cache.StatusError {
		t.Fatalf("lookup in the outage: %+v", l)
	}
	close(kv.release)
	if l := <-done; l.Status != cache.StatusMiss {
		t.Fatalf("lookup in flight: %+v", l)
	}
	if n := strings.Count(logs.String(), recovered); n != 0 {
		t.Fatalf("%d recovery lines from a call that started before the failure, want 0; logs:\n%s", n, logs.String())
	}

	kv.set(false)
	clk.add(time.Second)
	if l := c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", "after")); l.Status != cache.StatusMiss {
		t.Fatalf("lookup after the outage: %+v", l)
	}
	if n := strings.Count(logs.String(), recovered); n != 1 {
		t.Fatalf("%d recovery lines after the outage, want 1; logs:\n%s", n, logs.String())
	}
}

// Without the embedder the exact layer keeps working.
func TestEmbedderDown(t *testing.T) {
	t.Parallel()
	emb := &embed.Fake{Dims: 8, Err: errors.New("ollama down")}
	c, _ := newCache(t, nil, emb, 0.9)
	base := req("m", "", "What is the capital of France?")
	fill(t, c, "a", cache.ModeSemantic, base)
	if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, base).Status; s != cache.StatusExact {
		t.Errorf("exact with the embedder down: %s", s)
	}
	if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, req("m", "", "France: what is the capital of?")).Status; s != cache.StatusMiss {
		t.Errorf("semantic with the embedder down: %s", s)
	}
}

// A semantic lookup embeds once; its fill reuses the vector.
func TestFillReusesEmbedding(t *testing.T) {
	t.Parallel()
	emb := &embed.Fake{Dims: 8}
	c, _ := newCache(t, nil, emb, 0.9)
	fill(t, c, "a", cache.ModeSemantic, req("m", "", "hello world"))
	if calls, _ := emb.Calls(); calls != 1 {
		t.Errorf("embed calls = %d, want 1", calls)
	}
}

func TestChunks(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 150)
	for _, text := range []string{
		"", "one", "Paris is the capital.", "  leading and trailing  ", "line one\nline two\n\n",
		long, "日本語のテキストは空白がありません" + long, "mixed 日本 text " + long + " end",
	} {
		chunks := cache.Chunks(text)
		if got := strings.Join(chunks, ""); got != text {
			t.Errorf("Chunks(%q) joined = %q", text, got)
		}
		for _, c := range chunks {
			if c == "" || len(c) > 64 || !utf8.ValidString(c) {
				t.Errorf("Chunks(%q): bad chunk %q", text, c)
			}
		}
	}
	if got := cache.Chunks("Paris is the capital."); len(got) != 4 || got[0] != "Paris " {
		t.Errorf("word chunks = %q", got)
	}
}

// ctxKV fails a call whose context is done, as a Redis client does.
type ctxKV struct{ cache.KV }

func (c ctxKV) Get(ctx context.Context, k string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return c.KV.Get(ctx, k)
}

// A client that went away fails its own store call; that must not turn
// the cache off for every other tenant.
func TestCancelledClientDoesNotBypass(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := cache.New(cache.Options{KV: ctxKV{cache.NewMemoryKV(10, clk.now)}, TTL: time.Hour, Now: clk.now})
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if l := c.Lookup(gone, "a", target, cache.ModeExact, req("m", "", "hi")); l.Status != cache.StatusError {
		t.Fatalf("cancelled lookup: %+v", l)
	}
	if l := c.Lookup(context.Background(), "b", target, cache.ModeExact, req("m", "", "hi")); l.Status != cache.StatusMiss {
		t.Errorf("next tenant's lookup: %+v, want a miss", l)
	}
}

// countingEmbedder counts calls and fails them with err.
type countingEmbedder struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (e *countingEmbedder) Model() string { return "counting" }
func (e *countingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return nil, e.err
}

// An embedder that is down is bypassed for the retry interval, like the
// store; a prompt that is merely too long to embed is not an outage.
func TestEmbedderBackoff(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err       error
		wantCalls int
	}{
		"down":     {errors.New("connection refused"), 1},
		"too long": {embed.ErrTooLong, 3},
	} {
		emb := &countingEmbedder{err: tc.err}
		c, _ := newCache(t, nil, emb, 0.9)
		for range 3 {
			if l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, req("m", "", "hello")); l.Status != cache.StatusMiss {
				t.Fatalf("%s: %+v", name, l)
			}
		}
		if emb.calls != tc.wantCalls {
			t.Errorf("%s: %d embedding calls in 3 lookups, want %d", name, emb.calls, tc.wantCalls)
		}
	}
}

// After Close, replies are no longer stored and Fill never races Wait.
func TestCloseStopsFills(t *testing.T) {
	t.Parallel()
	c, _ := newCache(t, nil, nil, 0.9)
	r := req("m", "", "hello there")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			c.Fill(context.Background(), c.Lookup(context.Background(), "a", target, cache.ModeSemantic, r), answer)
		})
	}
	c.Close()
	wg.Wait()
	c.Close()
	other := req("m", "", "a different question")
	c.Fill(context.Background(), c.Lookup(context.Background(), "a", target, cache.ModeSemantic, other), answer)
	c.Wait()
	if s := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, other).Status; s != cache.StatusMiss {
		t.Errorf("filled after Close: %s", s)
	}
}

// failingIndex fails every Search or every Add, as a Redis without the
// Query Engine does, and counts the calls.
type failingIndex struct {
	cache.Index
	failSearch, failAdd bool
	mu                  sync.Mutex
	searches, adds      int
}

func (f *failingIndex) Search(ctx context.Context, q cache.Query) ([]cache.Match, error) {
	f.mu.Lock()
	f.searches++
	f.mu.Unlock()
	if f.failSearch {
		return nil, errors.New("ERR unknown command 'FT.SEARCH'")
	}
	return f.Index.Search(ctx, q)
}

func (f *failingIndex) Add(ctx context.Context, it cache.Item) error {
	f.mu.Lock()
	f.adds++
	f.mu.Unlock()
	if f.failAdd {
		return errors.New("ERR unknown command 'HSET'")
	}
	return f.Index.Add(ctx, it)
}

// A failing vector index bypasses the semantic layer only: the exact
// layer, whose store answers, keeps serving every tenant.
func TestIndexFailureKeepsExactLayer(t *testing.T) {
	t.Parallel()
	base := req("m", "", "What is the capital of France?")
	reworded := req("m", "", "France: what is the capital of?")
	newFailing := func(idx *failingIndex) (*cache.Cache, *clock) {
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		return cache.New(cache.Options{
			KV: cache.NewMemoryKV(10, clk.now),
			NewIndex: func(d int) (cache.Index, error) {
				idx.Index = cache.NewMemoryIndex(d, 10, clk.now)
				return idx, nil
			},
			Embedder:      &embed.Fake{Dims: 8},
			Threshold:     0.9,
			TTL:           time.Hour,
			RetryInterval: time.Second,
			Now:           clk.now,
		}), clk
	}

	t.Run("search", func(t *testing.T) {
		t.Parallel()
		idx := &failingIndex{failSearch: true}
		c, clk := newFailing(idx)
		fill(t, c, "a", cache.ModeExact, base)
		l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, reworded)
		if l.Status != cache.StatusMiss || !l.Fillable() {
			t.Fatalf("lookup with a failing search: %+v, want a fillable miss", l)
		}
		if l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, base); l.Status != cache.StatusExact {
			t.Fatalf("exact lookup after a search failure: %+v, want hit-exact", l)
		}
		// The reply to the missed request is still stored exactly.
		c.Fill(context.Background(), l, answer)
		c.Wait()
		if l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, reworded); l.Status != cache.StatusExact {
			t.Fatalf("refill after a search failure: %+v, want hit-exact", l)
		}
		if idx.searches != 1 {
			t.Errorf("searches during the back-off = %d, want 1", idx.searches)
		}
		clk.add(time.Second)
		_ = c.Lookup(context.Background(), "b", target, cache.ModeSemantic, reworded)
		if idx.searches != 2 {
			t.Errorf("searches after the back-off = %d, want 2", idx.searches)
		}
	})

	t.Run("add", func(t *testing.T) {
		t.Parallel()
		idx := &failingIndex{failAdd: true}
		c, _ := newFailing(idx)
		fill(t, c, "a", cache.ModeSemantic, base)
		if idx.adds != 1 {
			t.Fatalf("adds = %d, want 1", idx.adds)
		}
		if l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, base); l.Status != cache.StatusExact {
			t.Fatalf("exact lookup after an add failure: %+v, want hit-exact", l)
		}
		if l := c.Lookup(context.Background(), "b", target, cache.ModeExact, base); l.Status != cache.StatusMiss {
			t.Fatalf("other tenant after an add failure: %+v, want miss", l)
		}
	})
}

// settingKV counts Sets, and fails Gets while down is set.
type settingKV struct {
	cache.KV
	mu   sync.Mutex
	down bool
	sets int
}

func (s *settingKV) Get(ctx context.Context, k string) ([]byte, bool, error) {
	s.mu.Lock()
	down := s.down
	s.mu.Unlock()
	if down {
		return nil, false, errors.New("connection refused")
	}
	return s.KV.Get(ctx, k)
}

func (s *settingKV) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	s.mu.Lock()
	s.sets++
	s.mu.Unlock()
	return s.KV.Set(ctx, k, v, ttl)
}

// Only a miss is filled: a skip, a hit and a failed lookup write nothing,
// so a hit is never overwritten by a reply to another request.
func TestFillOnlyMisses(t *testing.T) {
	t.Parallel()
	other := &core.Response{Text: "Lyon.", StopReason: core.StopEndTurn}
	base := req("m", "", "What is the capital of France?")
	tests := []struct {
		name   string
		want   cache.Status
		lookup func(c *cache.Cache, kv *settingKV) *cache.Lookup
	}{
		{"skip", cache.StatusSkip, func(*cache.Cache, *settingKV) *cache.Lookup { return cache.Skip("off") }},
		{"cache off", cache.StatusSkip, func(c *cache.Cache, _ *settingKV) *cache.Lookup {
			return c.Lookup(context.Background(), "a", target, cache.ModeOff, base)
		}},
		{"hit-exact", cache.StatusExact, func(c *cache.Cache, _ *settingKV) *cache.Lookup {
			return c.Lookup(context.Background(), "a", target, cache.ModeSemantic, base)
		}},
		{"hit-semantic", cache.StatusSemantic, func(c *cache.Cache, _ *settingKV) *cache.Lookup {
			return c.Lookup(context.Background(), "a", target, cache.ModeSemantic, req("m", "", "France: what is the capital of?"))
		}},
		{"error", cache.StatusError, func(c *cache.Cache, kv *settingKV) *cache.Lookup {
			kv.mu.Lock()
			kv.down = true
			kv.mu.Unlock()
			defer func() { kv.mu.Lock(); kv.down = false; kv.mu.Unlock() }()
			return c.Lookup(context.Background(), "a", target, cache.ModeSemantic, base)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			kv := &settingKV{KV: cache.NewMemoryKV(10, clk.now)}
			idx := &failingIndex{}
			c := cache.New(cache.Options{
				KV: kv,
				NewIndex: func(d int) (cache.Index, error) {
					idx.Index = cache.NewMemoryIndex(d, 10, clk.now)
					return idx, nil
				},
				Embedder:  &embed.Fake{Dims: 64},
				Threshold: 0.9,
				TTL:       time.Hour,
				Now:       clk.now,
			})
			fill(t, c, "a", cache.ModeSemantic, base)
			if kv.sets != 1 || idx.adds != 1 {
				t.Fatalf("after the first fill: %d sets, %d adds", kv.sets, idx.adds)
			}
			l := tt.lookup(c, kv)
			if l.Status != tt.want || l.Fillable() {
				t.Fatalf("lookup %+v, want an unfillable %s", l, tt.want)
			}
			c.Fill(context.Background(), l, other)
			c.Wait()
			if kv.sets != 1 || idx.adds != 1 {
				t.Errorf("Fill on a %s lookup wrote: %d sets, %d adds, want 1 and 1", l.Status, kv.sets, idx.adds)
			}
			clk.add(time.Minute) // past any back-off
			if l := c.Lookup(context.Background(), "a", target, cache.ModeExact, base); l.Response == nil || l.Response.Text != answer.Text {
				t.Errorf("stored reply after the Fill: %+v, want %q", l, answer.Text)
			}
		})
	}
}

// tableEmbedder returns a fixed vector per text.
type tableEmbedder map[string][]float32

func (tableEmbedder) Model() string { return "table" }
func (e tableEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e[t]
	}
	return out, nil
}

// The threshold is the lowest similarity served: a match exactly at it is
// a hit, one just above it a miss.
func TestThresholdIsInclusive(t *testing.T) {
	t.Parallel()
	// Cosine of (1, 0) and (3, 4) is 3/5, exactly 0.6 in float64.
	emb := tableEmbedder{"stored": {1, 0}, "asked": {3, 4}}
	for _, tt := range []struct {
		threshold float64
		want      cache.Status
	}{
		{0.6, cache.StatusSemantic},
		{math.Nextafter(0.6, 1), cache.StatusMiss},
	} {
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		c := cache.New(cache.Options{
			KV:        cache.NewMemoryKV(10, clk.now),
			NewIndex:  func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 10, clk.now), nil },
			Embedder:  emb,
			Threshold: tt.threshold,
			TTL:       time.Hour,
			Now:       clk.now,
		})
		fill(t, c, "a", cache.ModeSemantic, req("m", "", "stored"))
		l := c.Lookup(context.Background(), "a", target, cache.ModeSemantic, req("m", "", "asked"))
		if l.Status != tt.want {
			t.Errorf("threshold %v: status %s (similarity %v), want %s", tt.threshold, l.Status, l.Similarity, tt.want)
		}
		if tt.want == cache.StatusSemantic && l.Similarity != 0.6 {
			t.Errorf("similarity = %v, want exactly 0.6", l.Similarity)
		}
	}
}

// sleepyKV waits d before every call.
type sleepyKV struct {
	cache.KV
	d time.Duration
}

func (s sleepyKV) Get(ctx context.Context, k string) ([]byte, bool, error) {
	time.Sleep(s.d)
	return s.KV.Get(ctx, k)
}

func (s sleepyKV) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	time.Sleep(s.d)
	return s.KV.Set(ctx, k, v, ttl)
}

// A lookup and a fill that wait on the store are each logged as a slow
// store call of their own component, with the request's id.
func TestSlowStoreCallsLogged(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	ctx := telemetry.WithRequest(context.Background(), slog.New(slog.NewTextHandler(logs, nil)), "main-12")
	c := cache.New(cache.Options{KV: sleepyKV{KV: cache.NewMemoryKV(10, time.Now), d: 60 * time.Millisecond}, TTL: time.Hour})
	l := c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", "hi"))
	c.Fill(ctx, l, answer)
	c.Wait()
	out := logs.String()
	for _, want := range []string{
		`msg="store call slow" request_id=main-12 component=cache_lookup op=get wait_ms=`,
		`msg="store call slow" request_id=main-12 component=cache_fill op=set wait_ms=`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; logs:\n%s", want, out)
		}
	}
}

// probeKV fails while down; with hold set, the next Get signals entered
// and waits for release, so a probe can be held in flight while other
// lookups arrive (and any of them that calls the store is counted and
// answered at once).
type probeKV struct {
	cache.KV
	mu      sync.Mutex
	down    bool
	hold    bool
	gets    int
	entered chan struct{}
	release chan struct{}
}

func (p *probeKV) Get(ctx context.Context, k string) ([]byte, bool, error) {
	p.mu.Lock()
	p.gets++
	down, hold := p.down, p.hold
	p.hold = false
	p.mu.Unlock()
	if hold {
		p.entered <- struct{}{}
		<-p.release
	}
	if down {
		return nil, false, errors.New("dial tcp: i/o timeout")
	}
	return p.KV.Get(ctx, k)
}

func (p *probeKV) state(down, hold bool) { p.mu.Lock(); p.down, p.hold = down, hold; p.mu.Unlock() }
func (p *probeKV) count() int            { p.mu.Lock(); defer p.mu.Unlock(); return p.gets }

// While the store is down, one lookup per retry interval probes it: with
// the probe still in flight, every other lookup bypasses the cache without
// calling the store, and keeps bypassing after the probe fails until a
// further interval has passed. The first probe that succeeds turns the
// cache back on for everyone.
func TestOneLookupProbesAnUnreachableStore(t *testing.T) {
	t.Parallel()
	const others = 50
	for _, probeUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("probe finds the store up=%v", probeUp), func(t *testing.T) {
			t.Parallel()
			clk := &clock{t: time.Unix(1_700_000_000, 0)}
			kv := &probeKV{KV: cache.NewMemoryKV(100, clk.now), down: true, entered: make(chan struct{}), release: make(chan struct{})}
			c := cache.New(cache.Options{KV: kv, TTL: time.Hour, RetryInterval: time.Second, Now: clk.now})
			ctx := context.Background()
			lookup := func(i int) *cache.Lookup {
				return c.Lookup(ctx, "a", target, cache.ModeExact, req("m", "", fmt.Sprintf("p%d", i)))
			}
			if l := lookup(-1); l.Status != cache.StatusError {
				t.Fatalf("first lookup: %+v", l)
			}
			// Inside the interval nothing reaches the store.
			clk.add(999 * time.Millisecond)
			if l := lookup(-2); l.Status != cache.StatusSkip || kv.count() != 1 {
				t.Fatalf("lookup inside the interval: %+v, %d gets", l, kv.count())
			}
			// The interval is over: hold the probe in flight.
			clk.add(time.Millisecond)
			kv.state(!probeUp, true)
			probe := make(chan *cache.Lookup)
			go func() { probe <- lookup(0) }()
			<-kv.entered
			var wg sync.WaitGroup
			skipped := make(chan bool, others)
			for i := range others {
				wg.Add(1)
				go func() {
					defer wg.Done()
					skipped <- lookup(i+1).Status == cache.StatusSkip
				}()
			}
			wg.Wait()
			close(skipped)
			for s := range skipped {
				if !s {
					t.Fatal("a lookup reached the store while the probe was in flight")
				}
			}
			if n := kv.count(); n != 2 {
				t.Fatalf("%d gets with the probe in flight, want 2 (the first failure and the probe)", n)
			}
			close(kv.release)
			l := <-probe
			if !probeUp {
				if l.Status != cache.StatusError {
					t.Fatalf("failed probe: %+v", l)
				}
				// The failure restarts the interval: still one probe per
				// interval, however many lookups arrive.
				for i := range others {
					if l := lookup(100 + i); l.Status != cache.StatusSkip {
						t.Fatalf("lookup after a failed probe: %+v", l)
					}
				}
				clk.add(time.Second)
				kv.state(false, false)
			} else if l.Status != cache.StatusMiss {
				t.Fatalf("successful probe: %+v", l)
			}
			before := kv.count()
			for i := range 5 {
				if l := lookup(200 + i); l.Status != cache.StatusMiss {
					t.Fatalf("lookup %d after recovery: %+v", i, l)
				}
			}
			if n := kv.count() - before; n != 5 {
				t.Fatalf("%d of 5 lookups reached the store after recovery", n)
			}
		})
	}
}

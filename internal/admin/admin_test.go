package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/router"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// clock is a settable time source shared by a test's components.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func call(at time.Time, key, outcome string, d time.Duration) pipeline.Call {
	return pipeline.Call{Time: at, Key: key, RequestedModel: "m", Model: "m", Outcome: outcome, Duration: d, Admitted: outcome != telemetry.OutcomeRateLimited}
}

func TestRecorderRecentIsNewestFirstAndBounded(t *testing.T) {
	t.Parallel()
	clk := newClock()
	r := NewRecorder(RecorderOptions{Recent: 3, Now: clk.Now})
	for i := range 5 {
		c := call(clk.Now(), "k", telemetry.OutcomeOK, time.Millisecond)
		c.RequestID = fmt.Sprint(i)
		r.Observe(c)
	}
	_, recent := r.snapshot()
	var ids []string
	for _, c := range recent {
		ids = append(ids, c.RequestID)
	}
	if got := strings.Join(ids, ","); got != "4,3,2" {
		t.Fatalf("recent ids = %s, want 4,3,2", got)
	}

	r2 := NewRecorder(RecorderOptions{Recent: 3, Now: clk.Now})
	r2.Observe(call(clk.Now(), "k", telemetry.OutcomeOK, 0))
	if _, recent := r2.snapshot(); len(recent) != 1 {
		t.Fatalf("a part-filled ring returned %d calls, want 1", len(recent))
	}
}

func TestRecorderWindow(t *testing.T) {
	t.Parallel()
	clk := newClock()
	r := NewRecorder(RecorderOptions{Window: 10 * time.Second, Now: clk.Now})
	// Old calls that fall out of the window.
	for range 50 {
		r.Observe(call(clk.Now(), "k", telemetry.OutcomeError, time.Second))
	}
	clk.Add(11 * time.Second)
	// 100 calls in the window: durations 1..100 ms, 5 errors, 4 cache
	// lookups of which 1 hit, one refusal that is counted but neither an
	// error nor timed.
	for i := 1; i <= 100; i++ {
		c := call(clk.Now(), "k", telemetry.OutcomeOK, time.Duration(i)*time.Millisecond)
		switch {
		case i <= 5:
			c.Outcome = telemetry.OutcomeError
		case i == 6:
			c.Outcome, c.Admitted = telemetry.OutcomeRateLimited, false
		case i <= 9:
			c.Cache = cache.StatusMiss
		case i == 10:
			c.Cache, c.Outcome = cache.StatusExact, telemetry.OutcomeCacheHit
		case i == 11:
			c.Cache = cache.StatusSkip
		}
		r.Observe(c)
	}
	rep, _ := r.snapshot()
	w := rep.Window
	want := Window{Seconds: 10, Requests: 100, RPS: 10, Errors: 5, ErrorRate: 0.05, P50MS: 51, P99MS: 100, CacheHitRate: 0.25}
	if w != want {
		t.Fatalf("window = %+v\nwant     %+v", w, want)
	}
	if rep.Requests != 150 || rep.Outcomes[telemetry.OutcomeError] != 55 || rep.Outcomes[telemetry.OutcomeRateLimited] != 1 {
		t.Fatalf("totals: requests %d, outcomes %v", rep.Requests, rep.Outcomes)
	}
	if rep.Lookups["miss"] != 3 || rep.Lookups["hit-exact"] != 1 || rep.Lookups["skip"] != 1 {
		t.Fatalf("lookups = %v", rep.Lookups)
	}

	// Once everything is older than the window, the figures are empty.
	clk.Add(time.Minute)
	if rep, _ := r.snapshot(); rep.Window.Requests != 0 || rep.Window.P99MS != 0 {
		t.Fatalf("stale window = %+v", rep.Window)
	}
}

func TestQuantileNearestRank(t *testing.T) {
	t.Parallel()
	tests := []struct {
		xs   []float64
		q    float64
		want float64
	}{
		{nil, 0.5, 0},
		{[]float64{7}, 0.99, 7},
		{[]float64{1, 2, 3, 4}, 0.5, 2},
		{[]float64{1, 2, 3, 4}, 0.75, 3},
		{[]float64{1, 2, 3, 4}, 1, 4},
	}
	for _, tt := range tests {
		if got := quantile(tt.xs, tt.q); got != tt.want {
			t.Errorf("quantile(%v, %v) = %v, want %v", tt.xs, tt.q, got, tt.want)
		}
	}
}

func TestRecorderKeysSpendAndRouterDecisions(t *testing.T) {
	t.Parallel()
	clk := newClock()
	r := NewRecorder(RecorderOptions{Candidates: []string{"small", "big"}, Now: clk.Now})
	c := call(clk.Now(), "alice", telemetry.OutcomeOK, time.Millisecond)
	c.RequestedModel, c.Model = "auto", "big"
	c.Router = &router.Decision{Model: "big", Reason: "target", Probs: []float64{0.4, 0.9}}
	c.Spent = 3 * cost.Cent
	c.Usage = core.Usage{InputTokens: 10, OutputTokens: 20}
	r.Observe(c)
	hit := call(clk.Now(), "alice", telemetry.OutcomeCacheHit, 0)
	hit.Cache, hit.Saved = cache.StatusExact, cost.Cent
	r.Observe(hit)
	r.Observe(call(clk.Now(), "bob", telemetry.OutcomeOK, 0))

	rep, recent := r.snapshot()
	if got := rep.Keys["alice"]; got.Calls != 2 || got.SpentNanos != int64(3*cost.Cent) {
		t.Fatalf("alice totals = %+v", got)
	}
	if rep.SavedNanos != int64(cost.Cent) || rep.SpentUSD != 0.03 {
		t.Fatalf("saved %d, spent %v", rep.SavedNanos, rep.SpentUSD)
	}
	if len(rep.Decisions) != 1 || rep.Decisions[0] != (Decision{Model: "big", Reason: "target", Count: 1}) {
		t.Fatalf("decisions = %+v", rep.Decisions)
	}
	auto := recent[2]
	if auto.RouterReason != "target" || auto.RouterProbs["small"] != 0.4 || auto.RouterProbs["big"] != 0.9 {
		t.Fatalf("auto call = %+v", auto)
	}
	if auto.CostUSD != 0.03 || auto.InputTokens != 10 || auto.OutputTokens != 20 {
		t.Fatalf("auto call cost/usage = %+v", auto)
	}
}

func TestRecorderConcurrentObserve(t *testing.T) {
	t.Parallel()
	r := NewRecorder(RecorderOptions{})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for range 500 {
				r.Observe(call(time.Now(), fmt.Sprint("k", g), telemetry.OutcomeOK, time.Millisecond))
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			r.snapshot()
		}
	})
	wg.Wait()
	if rep, _ := r.snapshot(); rep.Requests != 4000 {
		t.Fatalf("requests = %d, want 4000", rep.Requests)
	}
}

// replica is one simulated gateway process sharing a Redis.
type replica struct {
	rec *Recorder
	srv *Server
}

func newReplica(t *testing.T, id string, rdb redis.Cmdable, clk *clock, o Options) replica {
	t.Helper()
	rec := NewRecorder(RecorderOptions{Now: clk.Now})
	o.ID, o.Version, o.Started, o.Recorder, o.Now, o.Log = id, "test", clk.Now(), rec, clk.Now, quiet()
	if rdb != nil {
		o.Fleet = NewFleet(rdb, "sb:", quiet())
	}
	return replica{rec: rec, srv: NewServer(o)}
}

func (r replica) publish(t *testing.T) {
	t.Helper()
	rep, recent := r.srv.self()
	if err := r.srv.o.Fleet.publish(context.Background(), rep, recent); err != nil {
		t.Fatal(err)
	}
}

func TestOverviewWithoutRedisShowsOnlyItself(t *testing.T) {
	t.Parallel()
	clk := newClock()
	r := newReplica(t, "solo", nil, clk, Options{})
	r.rec.Observe(call(clk.Now(), "k", telemetry.OutcomeOK, time.Millisecond))
	ov := r.srv.Overview(context.Background())
	if ov.Shared || ov.FleetError != "" || len(ov.Replicas) != 1 {
		t.Fatalf("overview = %+v", ov)
	}
	if rr := ov.Replicas[0]; !rr.Self || rr.Status != StatusUp || rr.Requests != 1 {
		t.Fatalf("replica = %+v", rr)
	}
	if len(ov.Recent) != 1 || ov.Recent[0].Replica != "solo" {
		t.Fatalf("recent = %+v", ov.Recent)
	}
}

func TestOverviewMergesTheFleet(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	clk := newClock()
	a := newReplica(t, "a", rdb, clk, Options{CacheEnabled: true})
	b := newReplica(t, "b", rdb, clk, Options{})
	c := newReplica(t, "c", rdb, clk, Options{})

	hit := call(clk.Now(), "k", telemetry.OutcomeCacheHit, time.Millisecond)
	hit.Cache, hit.RequestID = cache.StatusExact, "a1"
	a.rec.Observe(hit)
	clk.Add(time.Millisecond)
	miss := call(clk.Now(), "k", telemetry.OutcomeOK, time.Millisecond)
	miss.Cache, miss.RequestID, miss.Spent = cache.StatusMiss, "b1", cost.Cent
	b.rec.Observe(miss)
	clk.Add(time.Millisecond)
	cc := call(clk.Now(), "k", telemetry.OutcomeOK, time.Millisecond)
	cc.RequestID = "c1"
	c.rec.Observe(cc)
	b.publish(t)
	c.publish(t)

	// c stops reporting: stale after a few missed intervals, down later,
	// forgotten after ForgetAfter. b keeps reporting.
	steps := []struct {
		advance time.Duration
		cStatus string
	}{
		{time.Second, StatusUp},
		{StaleAfter, StatusStale},
		{DownAfter, StatusDown},
		{ForgetAfter, ""},
	}
	for _, st := range steps {
		clk.Add(st.advance)
		b.publish(t)
		ov := a.srv.Overview(context.Background())
		if !ov.Shared || ov.FleetError != "" {
			t.Fatalf("overview not shared: %+v", ov)
		}
		got := map[string]Replica{}
		for _, r := range ov.Replicas {
			got[r.ID] = r
		}
		if !got["a"].Self || got["a"].Status != StatusUp || got["b"].Status != StatusUp {
			t.Fatalf("after %v: a %+v, b %+v", st.advance, got["a"], got["b"])
		}
		if st.cStatus == "" {
			if _, ok := got["c"]; ok {
				t.Fatalf("c not forgotten after %v", ForgetAfter)
			}
			if n, _ := rdb.HLen(context.Background(), "sb:fleet:replicas").Result(); n != 1 {
				t.Fatalf("reports hash holds %d replicas, want only b", n)
			}
			continue
		}
		if got["c"].Status != st.cStatus {
			t.Fatalf("after %v: c is %s, want %s", st.advance, got["c"].Status, st.cStatus)
		}
	}

	ov := a.srv.Overview(context.Background())
	if ov.Cache.Lookups["hit-exact"] != 1 || ov.Cache.Lookups["miss"] != 1 || ov.Cache.HitRate != 0.5 {
		t.Fatalf("fleet cache = %+v", ov.Cache)
	}
	var ids []string
	for _, r := range ov.Recent {
		ids = append(ids, r.Replica+":"+r.RequestID)
	}
	if got := strings.Join(ids, ","); got != "b:b1,a:a1" {
		t.Fatalf("recent = %s, want b:b1,a:a1 (c's calls left with it)", got)
	}
}

func TestOverviewWhenRedisIsDown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	clk := newClock()
	a := newReplica(t, "a", rdb, clk, Options{Timeout: 200 * time.Millisecond})
	a.rec.Observe(call(clk.Now(), "k", telemetry.OutcomeOK, time.Millisecond))
	mr.Close()
	ov := a.srv.Overview(context.Background())
	if ov.FleetError == "" || len(ov.Replicas) != 1 || !ov.Replicas[0].Self || len(ov.Recent) != 1 {
		t.Fatalf("overview with Redis down = %+v", ov)
	}
}

func TestOverviewKeys(t *testing.T) {
	t.Parallel()
	clk := newClock()
	keys := auth.NewKeys([]config.Key{
		{Name: "free", Key: "sk-free", Models: []string{"b", "a"}, Cache: "exact",
			Limits: config.Limits{Requests: &config.Rate{Rate: 100, Per: time.Minute, Burst: 20}},
			Budget: &config.KeyBudget{USD: "1.50", Period: "daily"}},
		{Name: "team", Key: "sk-team", Tenant: "acme"},
	})
	ledger := budget.NewMemory(time.Minute, clk.Now)
	b := budget.Budget{Limit: 150 * cost.Cent, Period: budget.PeriodDaily}
	res, err := ledger.Reserve(context.Background(), "free", b, "r1", 50*cost.Cent)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Settle(context.Background(), "free", b, res, 20*cost.Cent); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(context.Background(), "free", b, "r2", 10*cost.Cent); err != nil {
		t.Fatal(err)
	}
	r := newReplica(t, "a", nil, clk, Options{Keys: keys.Principals(), Ledger: ledger})
	c := call(clk.Now(), "free", telemetry.OutcomeOK, 0)
	c.Spent = 20 * cost.Cent
	r.rec.Observe(c)

	ov := r.srv.Overview(context.Background())
	if len(ov.Keys) != 2 || ov.Keys[0].Name != "free" || ov.Keys[1].Name != "team" {
		t.Fatalf("keys = %+v", ov.Keys)
	}
	free, team := ov.Keys[0], ov.Keys[1]
	if strings.Join(free.Models, ",") != "a,b" || free.Cache != "exact" || free.Tenant != "free" {
		t.Fatalf("free = %+v", free)
	}
	if *free.Requests != (Limit{Rate: 100, PeriodSeconds: 60, Burst: 20}) || free.Tokens != nil {
		t.Fatalf("free limits = %+v %+v", free.Requests, free.Tokens)
	}
	if bb := free.Budget; bb == nil || bb.LimitUSD != 1.5 || bb.SpentUSD != 0.2 || bb.ReservedUSD != 0.1 || bb.Error != "" {
		t.Fatalf("free budget = %+v", free.Budget)
	}
	if free.Calls != 1 || free.SpentUSD != 0.2 {
		t.Fatalf("free totals = %d %v", free.Calls, free.SpentUSD)
	}
	if team.Models != nil || team.Tenant != "acme" || team.Budget != nil || team.Requests != nil {
		t.Fatalf("team = %+v", team)
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()
	clk := newClock()
	page := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>admin</title>")}}
	r := newReplica(t, "a", nil, clk, Options{UI: page, AllowedHosts: []string{"switchboard-1"}, TraceURL: "http://jaeger/trace/{trace_id}"})
	h := r.srv.Handler()
	tests := []struct {
		host, path string
		status     int
		body       string
	}{
		{"127.0.0.1:9090", "/api/overview", 200, `"self":"a"`},
		{"localhost:9090", "/api/overview", 200, `"trace_url":"http://jaeger/trace/{trace_id}"`},
		{"[::1]:9090", "/", 200, "<title>admin</title>"},
		{"switchboard-1:9090", "/", 200, "<title>admin</title>"},
		{"evil.example:9090", "/api/overview", 403, "host not allowed"},
		{"127.0.0.1.nip.io", "/", 403, "host not allowed"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		req.Host = tt.host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.body) {
			t.Errorf("GET %s with Host %s = %d %q, want %d containing %q", tt.path, tt.host, rec.Code, rec.Body.String(), tt.status, tt.body)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.Host = "127.0.0.1:9090"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var ov Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Recent == nil || ov.Keys == nil || ov.Router.Decisions == nil {
		t.Fatalf("empty lists must encode as [], got %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("overview is cacheable")
	}
}

func TestRunPublishesUntilCancelled(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	clk := newClock()
	r := newReplica(t, "a", rdb, clk, Options{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.srv.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !mr.Exists("sb:fleet:recent:a") {
		if time.Now().After(deadline) {
			t.Fatal("no report published")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if ttl := mr.TTL("sb:fleet:recent:a"); ttl != recentTTL {
		t.Fatalf("recent calls TTL = %v, want %v", ttl, recentTTL)
	}
}

func TestRecorderBoundsTheRequestedModel(t *testing.T) {
	t.Parallel()
	clk := newClock()
	tests := []struct {
		name, model, want string
	}{
		{"short names are kept", "claude-opus-5-5", "claude-opus-5-5"},
		{"exactly the cap is kept", strings.Repeat("a", maxModelLen), strings.Repeat("a", maxModelLen)},
		{"long names are cut", strings.Repeat("a", 1<<20), strings.Repeat("a", maxModelLen) + "…"},
		{"a cut never splits a rune", strings.Repeat("a", maxModelLen-1) + "é" + "tail", strings.Repeat("a", maxModelLen-1) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRecorder(RecorderOptions{Recent: 1, Now: clk.Now})
			c := call(clk.Now(), "k", telemetry.OutcomeError, 0)
			c.RequestedModel = tt.model
			r.Observe(c)
			_, recent := r.snapshot()
			if got := recent[0].RequestedModel; got != tt.want {
				t.Fatalf("requested model = %.40q (%d bytes), want %.40q (%d bytes)", got, len(got), tt.want, len(tt.want))
			}
		})
	}
}

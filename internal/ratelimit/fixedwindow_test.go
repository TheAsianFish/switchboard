package ratelimit_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/ratelimit/ratelimittest"
)

func newFixedWindow(rdb redis.Scripter, prefix string) ratelimit.Limiter {
	return ratelimit.NewFixedWindow(rdb, prefix)
}

func newMemoryFixedWindow(c *ratelimittest.Clock) ratelimit.Limiter {
	return ratelimit.NewMemoryFixedWindow(c.Now)
}

// fixedWindowHarnesses are the two fixed-window implementations, which
// must behave identically.
var fixedWindowHarnesses = []struct {
	name string
	new  func(t *testing.T) ratelimittest.Harness
}{
	{"fixed_window/miniredis", miniredisHarness(newFixedWindow)},
	{"fixed_window/memory", memoryHarness(newMemoryFixedWindow)},
}

func TestFixedWindowContract(t *testing.T) {
	t.Parallel()
	for _, h := range fixedWindowHarnesses {
		ratelimittest.RunContract(t, h.name, h.new)
	}
}

func TestFixedWindowDecisions(t *testing.T) {
	t.Parallel()
	l := ratelimit.Limit{Rate: 5, Period: time.Minute, Burst: 1}
	// Start a minute on the clock: windows are aligned to the epoch.
	minute := time.Date(2026, 3, 14, 15, 9, 0, 0, time.UTC)
	type step struct {
		at    time.Duration // since minute
		cost  int64         // 0 means Adjust by delta
		delta int64
		want  ratelimit.Decision
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"burst is ignored", []step{
			{at: 0, cost: 5, want: ratelimit.Decision{Allowed: true}},
		}},
		{"retry after is the window end", []step{
			{at: 10 * time.Second, cost: 4, want: ratelimit.Decision{Allowed: true, Remaining: 1}},
			{at: 10*time.Second + 250*time.Microsecond, cost: 2,
				want: ratelimit.Decision{Remaining: 1, RetryAfter: 50*time.Second - 250*time.Microsecond}},
			{at: 10 * time.Second, cost: 1, want: ratelimit.Decision{Allowed: true}},
		}},
		{"new window resets", []step{
			{at: 59 * time.Second, cost: 5, want: ratelimit.Decision{Allowed: true}},
			{at: 60 * time.Second, cost: 5, want: ratelimit.Decision{Allowed: true}},
		}},
		{"never above rate", []step{
			{at: 0, cost: 6, want: ratelimit.Decision{Remaining: 5, RetryAfter: ratelimit.Never}},
		}},
		{"charge stays in its window", []step{
			{at: 0, delta: 50},
			{at: 30 * time.Second, cost: 1, want: ratelimit.Decision{RetryAfter: 30 * time.Second}},
			{at: 60 * time.Second, cost: 5, want: ratelimit.Decision{Allowed: true}},
		}},
		{"refund clamps at zero", []step{
			{at: 0, cost: 2, want: ratelimit.Decision{Allowed: true, Remaining: 3}},
			{at: 0, delta: -10},
			{at: 0, cost: 5, want: ratelimit.Decision{Allowed: true}},
			{at: 0, cost: 1, want: ratelimit.Decision{RetryAfter: time.Minute}},
		}},
	}
	for _, h := range fixedWindowHarnesses {
		for _, tc := range cases {
			t.Run(h.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				hs := h.new(t)
				ctx := context.Background()
				for i, s := range tc.steps {
					hs.Set(minute.Add(s.at))
					if s.cost == 0 {
						if err := hs.Limiter.Adjust(ctx, "k", l, s.delta); err != nil {
							t.Fatalf("step %d: Adjust: %v", i, err)
						}
						continue
					}
					got, err := hs.Limiter.Allow(ctx, "k", l, s.cost)
					if err != nil {
						t.Fatalf("step %d: Allow: %v", i, err)
					}
					if got != s.want {
						t.Fatalf("step %d: Allow(%d) = %+v, want %+v", i, s.cost, got, s.want)
					}
				}
			})
		}
	}
}

// TestFixedWindowBoundaryFlaw documents the baseline's flaw that Q2
// measures: Rate units just before a window boundary and Rate more just
// after it are all admitted, twice the allowance within milliseconds,
// even with Burst equal to Rate.
func TestFixedWindowBoundaryFlaw(t *testing.T) {
	t.Parallel()
	l := ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 100}
	boundary := time.Date(2026, 3, 14, 15, 10, 0, 0, time.UTC)
	for _, h := range fixedWindowHarnesses {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			hs := h.new(t)
			ctx := context.Background()
			var admitted int64
			for _, at := range []time.Time{boundary.Add(-time.Millisecond), boundary.Add(time.Millisecond)} {
				hs.Set(at)
				for range l.Rate {
					d, err := hs.Limiter.Allow(ctx, "k", l, 1)
					if err != nil {
						t.Fatal(err)
					}
					if d.Allowed {
						admitted++
					}
				}
			}
			if b := l.Bound(2 * time.Millisecond); admitted != 2*l.Rate || admitted <= b {
				t.Fatalf("admitted %d in 2ms across the boundary (bound %d), want %d", admitted, b, 2*l.Rate)
			}
		})
	}
}

// TestFixedWindowFailsBound checks that RunBound's property catches the
// baseline: on random traces it finds a window over the bound, so the
// property is not vacuous and the experiment's premise holds.
func TestFixedWindowFailsBound(t *testing.T) {
	t.Parallel()
	limits := []ratelimit.Limit{
		{Rate: 100, Period: time.Minute, Burst: 20}, // the Q2 limit
		{Rate: 10, Period: time.Second, Burst: 10},  // Burst = Rate: only the boundary flaw remains
	}
	for _, h := range fixedWindowHarnesses {
		for _, l := range limits {
			t.Run(fmt.Sprintf("%s/rate%d_burst%d", h.name, l.Rate, l.Burst), func(t *testing.T) {
				t.Parallel()
				found := 0
				for seed := uint64(1); seed <= 8; seed++ {
					v, err := ratelimittest.FindBoundViolation(context.Background(), h.new(t), l, "k", seed, 200)
					if err != nil {
						t.Fatal(err)
					}
					if v != nil {
						found++
					}
				}
				if found == 0 {
					t.Fatalf("no bound violation found for %+v in 8 traces", l)
				}
			})
		}
	}
}

func TestFixedWindowKeysExpire(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	m.SetTime(ratelimittest.Start)
	fw := ratelimit.NewFixedWindow(newClient(t, m), "p:")
	l := ratelimit.Limit{Rate: 5, Period: time.Minute, Burst: 1}
	if _, err := fw.Allow(context.Background(), "user", l, 1); err != nil {
		t.Fatal(err)
	}
	keys := m.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want one counter", keys)
	}
	if ttl := m.TTL(keys[0]); ttl <= time.Minute || ttl > 2*time.Minute {
		t.Fatalf("counter TTL = %v, want about two periods", ttl)
	}
	const wantPrefix = "p:rl:fw:{user}:"
	if len(keys[0]) <= len(wantPrefix) || keys[0][:len(wantPrefix)] != wantPrefix {
		t.Fatalf("key %q does not start with %q", keys[0], wantPrefix)
	}
	m.FastForward(2 * time.Minute)
	if keys := m.Keys(); len(keys) != 0 {
		t.Fatalf("keys after two periods = %v, want none", keys)
	}
}

func TestMemoryFixedWindowDropsOldWindows(t *testing.T) {
	t.Parallel()
	c := ratelimittest.NewClock(ratelimittest.Start)
	m := ratelimit.NewMemoryFixedWindow(c.Now)
	l := ratelimit.Limit{Rate: 5, Period: time.Second, Burst: 1}
	ctx := context.Background()
	// Many keys, each used once a second apart: the map must not keep
	// every one of them.
	for i := range 20000 {
		c.Set(ratelimittest.Start.Add(time.Duration(i/1000) * time.Second))
		if _, err := m.Allow(ctx, fmt.Sprintf("k%d", i), l, 1); err != nil {
			t.Fatal(err)
		}
	}
	if n := ratelimit.MemoryWindows(m); n > 4096 {
		t.Fatalf("%d windows held for 1000 live keys", n)
	}
}

func TestFixedWindowConcurrentReplicas(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	m.SetTime(ratelimittest.Start)
	var replicas []ratelimit.Limiter
	for range 3 {
		replicas = append(replicas, ratelimit.NewFixedWindow(newClient(t, m), "test:"))
	}
	l := ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 20}
	if got := admitConcurrently(t, replicas, "shared", l, 300); got != l.Rate {
		t.Fatalf("admitted %d across three replicas, want %d", got, l.Rate)
	}
}

func TestAlgorithmByName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{ratelimit.AlgorithmFixedWindow, false},
		{ratelimit.AlgorithmGCRA, false},
		{"sliding_log", true},
		{"", true},
	} {
		_, err := ratelimit.New(tc.name, nil, "")
		_, merr := ratelimit.NewMemory(tc.name, nil)
		if (err != nil) != tc.wantErr || (merr != nil) != tc.wantErr {
			t.Errorf("New(%q) err = %v, NewMemory err = %v, want error %v", tc.name, err, merr, tc.wantErr)
		}
	}
}

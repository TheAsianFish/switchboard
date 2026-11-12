//go:build integration

package ratelimit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/redistest"
)

// recordingLimiter records the errors of the limiter it wraps.
type recordingLimiter struct {
	ratelimit.Limiter
	errs *redistest.Errors
}

func (r recordingLimiter) Allow(ctx context.Context, key string, l ratelimit.Limit, cost int64) (ratelimit.Decision, error) {
	dec, err := r.Limiter.Allow(ctx, key, l, cost)
	return dec, r.errs.Add(err)
}

func (r recordingLimiter) Adjust(ctx context.Context, key string, l ratelimit.Limit, delta int64) error {
	return r.errs.Add(r.Limiter.Adjust(ctx, key, l, delta))
}

// A Redis that answers LOADING (restarting, reading its dataset back) is
// an unreachable store to the limiter: decisions come from the local
// limiter at this replica's share, without an error, and once the load is
// over they come from the shared counters again, which survived it. Not
// parallel: the reload refuses every client of the server.
func TestIntegrationLoadingDegradesAndRecovers(t *testing.T) {
	srv := redistest.Loading(t)
	rdb := srv.Client()
	errs := &redistest.Errors{}
	var (
		mu     sync.Mutex
		states []bool
	)
	d := &ratelimit.Degrading{
		Primary:       recordingLimiter{ratelimit.NewFixedWindow(rdb, testPrefix(t, rdb)), errs},
		Local:         ratelimit.NewMemoryFixedWindow(nil),
		Replicas:      5,
		RetryInterval: 50 * time.Millisecond,
		Timeout:       500 * time.Millisecond,
		OnState: func(degraded bool) {
			mu.Lock()
			states = append(states, degraded)
			mu.Unlock()
		},
	}
	seen := func() []bool { mu.Lock(); defer mu.Unlock(); return append([]bool(nil), states...) }
	ctx := context.Background()
	// A day-long window, so the test does not straddle one; the local
	// share is 10 / 5 = 2.
	lim := ratelimit.Limit{Rate: 10, Period: 24 * time.Hour, Burst: 10}
	allow := func(phase string) ratelimit.Decision {
		t.Helper()
		dec, err := d.Allow(ctx, "k", lim, 1)
		if err != nil {
			t.Fatalf("Allow %s: %v", phase, err)
		}
		return dec
	}
	day := time.Now().UTC().YearDay()

	for i := range 3 {
		if dec := allow("before"); !dec.Allowed || dec.Remaining != int64(9-i) {
			t.Fatalf("Allow %d before the reload: %+v, want allowed with %d left", i, dec, 9-i)
		}
	}

	srv.StartLoading()
	for i := range 2 {
		if dec := allow("during"); !dec.Allowed || dec.Remaining != int64(1-i) {
			t.Fatalf("Allow %d during LOADING: %+v, want allowed by the local share with %d left", i, dec, 1-i)
		}
	}
	if !d.Degraded() {
		t.Fatal("not degraded during LOADING")
	}
	if got := seen(); len(got) != 1 || !got[0] {
		t.Fatalf("OnState calls = %v, want [true]", got)
	}
	// The local share is spent, though the shared limit has 7 left.
	if dec := allow("during"); dec.Allowed {
		t.Fatalf("third Allow during LOADING: %+v, want refused until the next probe", dec)
	}
	// A probe after the retry interval meets LOADING again.
	time.Sleep(60 * time.Millisecond)
	if dec := allow("probe"); dec.Allowed || !d.Degraded() {
		t.Fatalf("probe during LOADING: %+v, degraded %v; want refused by the local share", dec, d.Degraded())
	}
	srv.EndLoading()
	failures := errs.RequireLoading(t)
	if len(failures) < 2 {
		t.Fatalf("store errors during the reload = %v, want the first call's and the probe's", failures)
	}

	// After: the first call past the retry interval probes Redis and is
	// decided by the shared counter, which kept the 3 admitted before.
	var dec ratelimit.Decision
	deadline := time.Now().Add(5 * time.Second)
	for d.Degraded() {
		if time.Now().After(deadline) {
			t.Fatal("still degraded 5s after the reload")
		}
		time.Sleep(60 * time.Millisecond)
		dec = allow("after")
	}
	if got := seen(); len(got) != 2 || got[1] {
		t.Fatalf("OnState calls = %v, want [true false]", got)
	}
	if extra := errs.All(); len(extra) > 0 {
		t.Fatalf("store errors after the reload: %v", extra)
	}
	if !dec.Allowed {
		t.Fatalf("first Allow after the reload: %+v, want allowed by the shared limit", dec)
	}
	if time.Now().UTC().YearDay() == day && dec.Remaining != 6 {
		t.Fatalf("first Allow after the reload: %+v, want 6 left in the shared window", dec)
	}
	t.Logf("degraded on %d LOADING replies, first: %v", len(failures), failures[0])
}

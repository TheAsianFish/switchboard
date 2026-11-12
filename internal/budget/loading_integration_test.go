//go:build integration

package budget

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/redistest"
)

// recordingLedger records the errors of the ledger it wraps.
type recordingLedger struct {
	Ledger
	errs *redistest.Errors
}

func (r recordingLedger) Reserve(ctx context.Context, key string, b Budget, id string, amount cost.Nanos) (Reservation, error) {
	res, err := r.Ledger.Reserve(ctx, key, b, id, amount)
	return res, r.errs.Add(err)
}

func (r recordingLedger) Settle(ctx context.Context, key string, b Budget, res Reservation, actual cost.Nanos) error {
	return r.errs.Add(r.Ledger.Settle(ctx, key, b, res, actual))
}

func (r recordingLedger) Balance(ctx context.Context, key string, b Budget) (Balance, error) {
	bal, err := r.Ledger.Balance(ctx, key, b)
	return bal, r.errs.Add(err)
}

// A Redis that answers LOADING (restarting, reading its dataset back) is
// an unreachable store to the budget: requests are admitted on grace
// without an error, and once the load is over the ledger recovers and
// charges what was settled meanwhile. Not parallel: the reload refuses
// every client of the server.
func TestIntegrationLoadingDegradesAndRecovers(t *testing.T) {
	srv := redistest.Loading(t)
	rdb := srv.Client()
	prefix := testPrefix(t, rdb)
	errs := &redistest.Errors{}
	var (
		mu     sync.Mutex
		states []bool
	)
	d := NewDegrading(recordingLedger{NewRedis(rdb, prefix, time.Minute, nil), errs}, DegradeOptions{
		Grace:         1000,
		RetryInterval: 50 * time.Millisecond,
		Timeout:       500 * time.Millisecond,
		OnState: func(degraded bool) {
			mu.Lock()
			states = append(states, degraded)
			mu.Unlock()
		},
	})
	seen := func() []bool { mu.Lock(); defer mu.Unlock(); return append([]bool(nil), states...) }
	ctx := context.Background()
	b := Budget{Limit: 10_000, Period: PeriodDaily}

	// Before: the shared ledger is used.
	r := mustReserve(t, d, "k", b, "before", 100)
	if r.grace {
		t.Fatal("reservation before the reload was admitted on grace")
	}
	mustSettle(t, d, "k", b, r, 30)

	srv.StartLoading()
	g, err := d.Reserve(ctx, "k", b, "during", 400)
	if err != nil {
		t.Fatalf("Reserve during LOADING: %v", err)
	}
	if !g.grace || !d.Degraded() {
		t.Fatalf("Reserve during LOADING: grace %v, degraded %v; want both", g.grace, d.Degraded())
	}
	if got := seen(); len(got) != 1 || !got[0] {
		t.Fatalf("OnState calls = %v, want [true]", got)
	}
	if err := d.Settle(ctx, "k", b, g, 250); err != nil {
		t.Fatalf("Settle during LOADING: %v", err)
	}
	if _, err := d.Balance(ctx, "k", b); err == nil {
		t.Fatal("Balance during LOADING succeeded")
	}
	// A probe after the retry interval meets LOADING again and stays on
	// grace.
	time.Sleep(60 * time.Millisecond)
	g2, err := d.Reserve(ctx, "k", b, "probe", 100)
	if err != nil || !g2.grace {
		t.Fatalf("probe during LOADING: grace %v, err %v; want grace", g2.grace, err)
	}
	mustSettle(t, d, "k", b, g2, 0)
	srv.EndLoading()
	failures := errs.RequireLoading(t)
	if len(failures) < 2 {
		t.Fatalf("store errors during the reload = %v, want the first call's and the probe's", failures)
	}

	// After: a call past the retry interval probes Redis, flushes the
	// pending charge and leaves degraded mode.
	var bal Balance
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(60 * time.Millisecond)
		bal, err = d.Balance(ctx, "k", b)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still unavailable 5s after the reload: %v", err)
		}
	}
	if d.Degraded() {
		t.Fatal("still degraded after a successful Balance")
	}
	if got := seen(); len(got) != 2 || got[1] {
		t.Fatalf("OnState calls = %v, want [true false]", got)
	}
	if extra := errs.All(); len(extra) > 0 {
		t.Fatalf("store errors after the reload: %v", extra)
	}
	// That Balance was read before the flush it triggered.
	wantPosition(t, bal, 30, 0)
	wantPosition(t, mustBalance(t, d, "k", b), 280, 0)
	// The charge reached Redis itself, not only this replica's view.
	raw, err := NewRedis(rdb, prefix, time.Minute, nil).Balance(ctx, "k", b)
	if err != nil {
		t.Fatal(err)
	}
	wantPosition(t, raw, 280, 0)
	t.Logf("degraded on %d LOADING replies, first: %v", len(failures), failures[0])
}

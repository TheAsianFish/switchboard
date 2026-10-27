package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/cost"
)

var errDown = errors.New("connection refused")

// flaky is a primary that can be taken down, or made to refuse plain
// charges for one key, around a working in-memory ledger.
type flaky struct {
	inner *Memory
	down  atomic.Bool
	// slow makes every call block until its context is done.
	slow atomic.Bool

	mu         sync.Mutex
	failCharge string // key whose plain charges fail
	charges    int    // successful plain charges
}

func newFlaky(c *clock) *flaky { return &flaky{inner: NewMemory(time.Minute, c.Now)} }

func (f *flaky) check(ctx context.Context) error {
	if f.slow.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.down.Load() {
		return errDown
	}
	return nil
}

func (f *flaky) Reserve(ctx context.Context, key string, b Budget, id string, amount cost.Nanos) (Reservation, error) {
	if err := f.check(ctx); err != nil {
		return Reservation{}, err
	}
	return f.inner.Reserve(ctx, key, b, id, amount)
}

func (f *flaky) Settle(ctx context.Context, key string, b Budget, r Reservation, actual cost.Nanos) error {
	if err := f.check(ctx); err != nil {
		return err
	}
	if r.ID == "" {
		f.mu.Lock()
		fail := f.failCharge == key
		if !fail {
			f.charges++
		}
		f.mu.Unlock()
		if fail {
			return errDown
		}
	}
	return f.inner.Settle(ctx, key, b, r, actual)
}

func (f *flaky) Balance(ctx context.Context, key string, b Budget) (Balance, error) {
	if err := f.check(ctx); err != nil {
		return Balance{}, err
	}
	return f.inner.Balance(ctx, key, b)
}

// states records OnState transitions.
type states struct {
	mu  sync.Mutex
	got []bool
}

func (s *states) record(degraded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, degraded)
}

func (s *states) want(t *testing.T, want ...bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if fmt.Sprint(s.got) != fmt.Sprint(want) {
		t.Fatalf("OnState transitions = %v, want %v", s.got, want)
	}
}

const grace = 100

func newTestDegrading(t *testing.T) (*Degrading, *flaky, *clock, *states) {
	t.Helper()
	c := &clock{t: t0}
	f := newFlaky(c)
	s := &states{}
	d := NewDegrading(f, DegradeOptions{
		Grace:         grace,
		RetryInterval: time.Second,
		Timeout:       50 * time.Millisecond,
		OnState:       s.record,
		Now:           c.Now,
	})
	return d, f, c, s
}

var monthly = Budget{Limit: 1000, Period: PeriodMonthly}

func TestDegradingGrace(t *testing.T) {
	t.Parallel()
	d, f, c, s := newTestDegrading(t)
	ctx := context.Background()
	f.down.Store(true)

	mustReserve(t, d, "k", monthly, "a", 60)
	if !d.Degraded() {
		t.Fatal("not degraded after a primary failure")
	}
	r := mustReserve(t, d, "k", monthly, "b", 40)
	if r.Window != "2026-10" {
		t.Fatalf("grace reservation window = %q", r.Window)
	}
	if _, err := d.Reserve(ctx, "k", monthly, "c", 1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("Reserve past grace: err = %v, want ErrExhausted", err)
	}
	// Grace is per key.
	mustReserve(t, d, "other", monthly, "a", grace)
	// Settling below the hold gives grace back.
	mustSettle(t, d, "k", monthly, r, 10)
	mustReserve(t, d, "k", monthly, "d", 30)
	if _, err := d.Balance(ctx, "k", monthly); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Balance while degraded: err = %v, want ErrUnavailable", err)
	}

	// The primary stays down: a probe after the interval fails and
	// nothing transitions.
	c.Set(t0.Add(1500 * time.Millisecond))
	if _, err := d.Reserve(ctx, "k", monthly, "e", 1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("Reserve past grace after a failed probe: err = %v", err)
	}
	s.want(t, true)
}

func TestDegradingFlushesOnce(t *testing.T) {
	t.Parallel()
	d, f, c, s := newTestDegrading(t)
	ctx := context.Background()

	// Before the outage: one settled request and one in flight.
	mustSettle(t, d, "k", monthly, mustReserve(t, d, "k", monthly, "before", 100), 100)
	inFlight := mustReserve(t, d, "k", monthly, "in-flight", 300)

	f.down.Store(true)
	// The in-flight request finishes during the outage; its hold stays on
	// the primary until it expires.
	mustSettle(t, d, "k", monthly, inFlight, 200)
	local := mustReserve(t, d, "k", monthly, "during", 80)
	mustSettle(t, d, "k", monthly, local, 50)
	// Zero-cost settles do not create pending charges.
	mustSettle(t, d, "k", monthly, Reservation{}, 0)

	// Back up, but still inside the retry interval: nothing is probed.
	f.down.Store(false)
	mustReserve(t, d, "k", monthly, "still-local", 10)
	if !d.Degraded() {
		t.Fatal("recovered before the retry interval")
	}

	c.Set(t0.Add(time.Second))
	bal, err := d.Balance(ctx, "k", monthly)
	if err != nil {
		t.Fatalf("Balance after recovery: %v", err)
	}
	if d.Degraded() {
		t.Fatal("still degraded after a successful probe")
	}
	// The probe's own answer predates the flush.
	if bal.Spent != 100 {
		t.Fatalf("probe balance spent = %v, want 100", bal.Spent)
	}
	bal = mustBalance(t, d, "k", monthly)
	wantPosition(t, bal, 350, 300)

	// A local reservation from the outage settles after recovery: it is a
	// plain charge on the primary.
	mustSettle(t, d, "k", monthly, Reservation{}, 0)
	mustSettle(t, d, "k", monthly, mustReserve(t, d, "k", monthly, "after", 20), 25)
	wantPosition(t, mustBalance(t, d, "k", monthly), 375, 300)

	// The primary hold from before the outage lapses by TTL.
	c.Set(t0.Add(2 * time.Minute))
	wantPosition(t, mustBalance(t, d, "k", monthly), 375, 0)
	s.want(t, true, false)

	f.mu.Lock()
	charges := f.charges
	f.mu.Unlock()
	if charges != 2 { // one flush plus the zero settle after recovery
		t.Fatalf("plain charges on the primary = %d, want 2", charges)
	}
}

func TestDegradingStillLocalSettleAfterRecovery(t *testing.T) {
	t.Parallel()
	d, f, c, _ := newTestDegrading(t)
	f.down.Store(true)
	local := mustReserve(t, d, "k", monthly, "during", 80)
	f.down.Store(false)
	c.Set(t0.Add(time.Second))
	mustBalance(t, d, "k", monthly) // recovers
	mustSettle(t, d, "k", monthly, local, 60)
	wantPosition(t, mustBalance(t, d, "k", monthly), 60, 0)
}

func TestDegradingFreshGracePerOutage(t *testing.T) {
	t.Parallel()
	d, f, c, s := newTestDegrading(t)
	ctx := context.Background()
	for outage := range 2 {
		f.down.Store(true)
		c.Set(t0.Add(time.Duration(outage) * time.Hour))
		r := mustReserve(t, d, "k", monthly, fmt.Sprintf("o%d", outage), grace)
		if _, err := d.Reserve(ctx, "k", monthly, "x", 1); !errors.Is(err, ErrExhausted) {
			t.Fatalf("outage %d: Reserve past grace: err = %v", outage, err)
		}
		mustSettle(t, d, "k", monthly, r, grace)
		f.down.Store(false)
		c.Set(t0.Add(time.Duration(outage)*time.Hour + time.Second))
		mustBalance(t, d, "k", monthly)
		if d.Degraded() {
			t.Fatalf("outage %d: not recovered", outage)
		}
	}
	wantPosition(t, mustBalance(t, d, "k", monthly), 2*grace, 0)
	s.want(t, true, false, true, false)
}

func TestDegradingFlushFailureKeepsPending(t *testing.T) {
	t.Parallel()
	d, f, c, s := newTestDegrading(t)
	ctx := context.Background()
	f.down.Store(true)
	mustSettle(t, d, "a", monthly, Reservation{}, 70)
	mustSettle(t, d, "b", monthly, Reservation{}, 30)
	f.down.Store(false)
	f.mu.Lock()
	f.failCharge = "b"
	f.mu.Unlock()

	// The probe reaches the primary, but flushing b fails: a may have been
	// charged, b is kept and the ledger stays degraded.
	c.Set(t0.Add(time.Second))
	if _, err := d.Balance(ctx, "a", monthly); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !d.Degraded() {
		t.Fatal("recovered with charges still pending")
	}
	if _, err := d.Balance(ctx, "a", monthly); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Balance inside the retry interval after a failed flush: err = %v", err)
	}

	f.mu.Lock()
	f.failCharge = ""
	f.mu.Unlock()
	c.Set(t0.Add(2 * time.Second))
	mustBalance(t, d, "a", monthly)
	if d.Degraded() {
		t.Fatal("not recovered after the flush succeeded")
	}
	wantPosition(t, mustBalance(t, d, "a", monthly), 70, 0)
	wantPosition(t, mustBalance(t, d, "b", monthly), 30, 0)
	s.want(t, true, false)
}

func TestDegradingNotAStoreFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		call func(context.Context, *Degrading) error
		ctx  func() context.Context
		want error
	}{
		{
			name: "caller cancelled reserve",
			ctx:  cancelled,
			call: func(ctx context.Context, d *Degrading) error {
				_, err := d.Reserve(ctx, "k", monthly, "a", 1)
				return err
			},
			want: context.Canceled,
		},
		{
			name: "caller cancelled balance",
			ctx:  cancelled,
			call: func(ctx context.Context, d *Degrading) error {
				_, err := d.Balance(ctx, "k", monthly)
				return err
			},
			want: context.Canceled,
		},
		{
			name: "exhausted",
			ctx:  context.Background,
			call: func(ctx context.Context, d *Degrading) error {
				_, err := d.Reserve(ctx, "k", monthly, "a", monthly.Limit+1)
				return err
			},
			want: ErrExhausted,
		},
		{
			name: "invalid input",
			ctx:  context.Background,
			call: func(ctx context.Context, d *Degrading) error {
				_, err := d.Reserve(ctx, "k", monthly, "a", -1)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, s := newTestDegrading(t)
			err := tc.call(tc.ctx(), d)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if d.Degraded() {
				t.Fatal("degraded")
			}
			s.want(t)
		})
	}
}

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestDegradingSettleIgnoresCallerCancellation(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newTestDegrading(t)
	r := mustReserve(t, d, "k", monthly, "a", 50)
	if err := d.Settle(cancelled(), "k", monthly, r, 40); err != nil {
		t.Fatalf("Settle with a cancelled context: %v", err)
	}
	wantPosition(t, mustBalance(t, d, "k", monthly), 40, 0)
}

func TestDegradingTimeout(t *testing.T) {
	t.Parallel()
	d, f, _, s := newTestDegrading(t)
	f.slow.Store(true)
	start := time.Now()
	mustReserve(t, d, "k", monthly, "a", 10)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Reserve took %v with a 50ms timeout", elapsed)
	}
	s.want(t, true)
}

// TestDegradingConcurrentOutages flips the primary up and down under load
// and checks that no settled cost is lost or counted twice.
func TestDegradingConcurrentOutages(t *testing.T) {
	t.Parallel()
	c := &clock{t: t0}
	f := newFlaky(c)
	var outages atomic.Int64
	d := NewDegrading(f, DegradeOptions{
		Grace: cost.Dollar, RetryInterval: time.Millisecond, Now: time.Now,
		OnState: func(degraded bool) {
			if degraded {
				outages.Add(1)
			}
		},
	})
	ctx := context.Background()
	b := Budget{Limit: 1000 * cost.Dollar, Period: PeriodTotal}

	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				f.down.Store(false)
				return
			case <-time.After(2 * time.Millisecond):
				f.down.Store(i%2 == 0)
			}
		}
	})

	var (
		wg      sync.WaitGroup
		settled atomic.Int64
	)
	for w := range 16 {
		wg.Go(func() {
			for i := range 100 {
				r, err := d.Reserve(ctx, "k", b, fmt.Sprintf("w%d-%d", w, i), 1000)
				if errors.Is(err, ErrExhausted) {
					continue
				}
				if err != nil {
					t.Errorf("Reserve: %v", err)
					return
				}
				actual := cost.Nanos(1 + (w*100+i)%997)
				if err := d.Settle(ctx, "k", b, r, actual); err != nil {
					t.Errorf("Settle: %v", err)
					return
				}
				settled.Add(int64(actual))
			}
		})
	}
	wg.Wait()
	close(stop)
	flipper.Wait()

	// Probe until recovered; the flush runs on the first success.
	deadline := time.Now().Add(5 * time.Second)
	for d.Degraded() || pendingLen(d) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("did not recover")
		}
		_, _ = d.Balance(ctx, "k", b)
		time.Sleep(time.Millisecond)
	}
	got := mustBalance(t, f.inner, "k", b).Spent
	if got != cost.Nanos(settled.Load()) {
		t.Fatalf("primary spent = %d, want %d settled", got, settled.Load())
	}
	if outages.Load() == 0 {
		t.Fatal("the primary never went down; the test does not exercise degraded mode")
	}
	t.Logf("%d outages, %d settled", outages.Load(), settled.Load())
}

func pendingLen(d *Degrading) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}

// TestDegradingOverRedis takes a real Redis ledger away by closing the
// miniredis server and brings it back.
func TestDegradingOverRedis(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	c := &clock{t: t0}
	m.SetTime(t0)
	s := &states{}
	d := NewDegrading(NewRedis(rdb, "test:", time.Minute, c.Now), DegradeOptions{
		Grace: grace, OnState: s.record, Now: c.Now,
	})
	ctx := context.Background()

	mustSettle(t, d, "k", monthly, mustReserve(t, d, "k", monthly, "before", 100), 90)
	m.Close()
	mustSettle(t, d, "k", monthly, mustReserve(t, d, "k", monthly, "during", 100), 70)
	if _, err := d.Reserve(ctx, "k", monthly, "over", 31); !errors.Is(err, ErrExhausted) {
		t.Fatalf("Reserve past grace: err = %v", err)
	}
	s.want(t, true)

	if err := m.Restart(); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	c.Set(t0.Add(time.Second))
	mustReserve(t, d, "k", monthly, "after", 10)
	wantPosition(t, mustBalance(t, d, "k", monthly), 160, 10)
	s.want(t, true, false)
}

// gatedLedger answers its first Reserve only when released, successfully;
// every other call fails.
type gatedLedger struct {
	Ledger
	started, release chan struct{}
	calls            atomic.Int64
}

func (g *gatedLedger) Reserve(ctx context.Context, key string, b Budget, id string, amount cost.Nanos) (Reservation, error) {
	if g.calls.Add(1) == 1 {
		close(g.started)
		<-g.release
		return g.Ledger.Reserve(ctx, key, b, id, amount)
	}
	return Reservation{}, errDown
}

// A slow call that started before the store failed and succeeds after it
// must not end degraded mode; only a probe can.
func TestDegradingStaleSuccessDoesNotRecover(t *testing.T) {
	t.Parallel()
	c := &clock{t: t0}
	g := &gatedLedger{Ledger: NewMemory(time.Minute, c.Now), started: make(chan struct{}), release: make(chan struct{})}
	s := &states{}
	d := NewDegrading(g, DegradeOptions{Grace: grace, RetryInterval: time.Second, Timeout: time.Minute, OnState: s.record, Now: c.Now})
	ctx := context.Background()

	slow := make(chan error)
	go func() { _, err := d.Reserve(ctx, "k", monthly, "slow", 10); slow <- err }()
	<-g.started
	if _, err := d.Reserve(ctx, "k", monthly, "fails", 10); err != nil {
		t.Fatal(err)
	}
	if !d.Degraded() {
		t.Fatal("not degraded after a failure")
	}
	close(g.release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	if !d.Degraded() {
		t.Error("a success that started before the failure ended degraded mode")
	}
}

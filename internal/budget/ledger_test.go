package budget

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/cost"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// harness is a ledger under test and a way to move every clock it reads.
type harness struct {
	ledger Ledger
	set    func(time.Time)
}

// newMemoryHarness and newRedisHarness build the two implementations the
// contract runs against.
func newMemoryHarness(t *testing.T, ttl time.Duration) harness {
	c := &clock{t: t0}
	return harness{ledger: NewMemory(ttl, c.Now), set: c.Set}
}

func newRedisHarness(t *testing.T, ttl time.Duration) harness {
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := &clock{t: t0}
	m.SetTime(t0)
	set := func(t time.Time) {
		c.Set(t)
		m.SetTime(t) // the scripts read expiry time from Redis TIME
	}
	return harness{ledger: NewRedis(rdb, "test:", ttl, c.Now), set: set}
}

var implementations = []struct {
	name string
	new  func(*testing.T, time.Duration) harness
}{
	{"memory", newMemoryHarness},
	{"redis", newRedisHarness},
}

func mustBalance(t *testing.T, l Ledger, key string, b Budget) Balance {
	t.Helper()
	bal, err := l.Balance(context.Background(), key, b)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	return bal
}

func mustReserve(t *testing.T, l Ledger, key string, b Budget, id string, amount cost.Nanos) Reservation {
	t.Helper()
	r, err := l.Reserve(context.Background(), key, b, id, amount)
	if err != nil {
		t.Fatalf("Reserve(%s, %v): %v", id, amount, err)
	}
	return r
}

func mustSettle(t *testing.T, l Ledger, key string, b Budget, r Reservation, actual cost.Nanos) {
	t.Helper()
	if err := l.Settle(context.Background(), key, b, r, actual); err != nil {
		t.Fatalf("Settle(%s, %v): %v", r.ID, actual, err)
	}
}

func wantPosition(t *testing.T, got Balance, spent, reserved cost.Nanos) {
	t.Helper()
	if got.Spent != spent || got.Reserved != reserved {
		t.Fatalf("balance = spent %v reserved %v, want spent %v reserved %v", got.Spent, got.Reserved, spent, reserved)
	}
}

// TestContract runs the Ledger semantics against every implementation.
func TestContract(t *testing.T) {
	t.Parallel()
	daily := Budget{Limit: 100, Period: PeriodDaily}
	cases := []struct {
		name string
		run  func(t *testing.T, h harness)
	}{
		{"reserve within limit, refuse past it holding nothing", func(t *testing.T, h harness) {
			l := h.ledger
			r := mustReserve(t, l, "k", daily, "a", 60)
			if r.ID != "a" || r.Amount != 60 || r.Window != "2026-10-27" {
				t.Fatalf("reservation = %+v", r)
			}
			mustReserve(t, l, "k", daily, "b", 40)
			_, err := l.Reserve(context.Background(), "k", daily, "c", 1)
			if !errors.Is(err, ErrExhausted) {
				t.Fatalf("Reserve past limit: err = %v, want ErrExhausted", err)
			}
			wantPosition(t, mustBalance(t, l, "k", daily), 0, 100)
			// Another key has its own budget.
			mustReserve(t, l, "other", daily, "a", 100)
		}},
		{"refusal says remaining and needed", func(t *testing.T, h harness) {
			mustReserve(t, h.ledger, "k", daily, "a", 70)
			_, err := h.ledger.Reserve(context.Background(), "k", daily, "b", 50)
			want := "budget k window 2026-10-27: remaining $0.000000030, needed $0.000000050: budget exhausted"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
		}},
		{"settle below the hold frees the difference", func(t *testing.T, h harness) {
			l := h.ledger
			r := mustReserve(t, l, "k", daily, "a", 80)
			mustSettle(t, l, "k", daily, r, 30)
			bal := mustBalance(t, l, "k", daily)
			wantPosition(t, bal, 30, 0)
			if bal.Remaining() != 70 || bal.Limit != 100 || bal.Window != "2026-10-27" {
				t.Fatalf("balance = %+v", bal)
			}
			mustReserve(t, l, "k", daily, "b", 70)
		}},
		{"settle above the hold records the overshoot", func(t *testing.T, h harness) {
			l := h.ledger
			r := mustReserve(t, l, "k", daily, "a", 50)
			mustSettle(t, l, "k", daily, r, 130)
			bal := mustBalance(t, l, "k", daily)
			wantPosition(t, bal, 130, 0)
			if bal.Remaining() != -30 {
				t.Fatalf("remaining = %v, want -30", bal.Remaining())
			}
			if _, err := l.Reserve(context.Background(), "k", daily, "b", 0); !errors.Is(err, ErrExhausted) {
				t.Fatalf("Reserve over budget: err = %v", err)
			}
		}},
		{"unknown and empty ids are plain charges", func(t *testing.T, h harness) {
			l := h.ledger
			held := mustReserve(t, l, "k", daily, "held", 10)
			mustSettle(t, l, "k", daily, Reservation{ID: "never-reserved", Window: held.Window, Amount: 5}, 7)
			mustSettle(t, l, "k", daily, Reservation{}, 3)
			wantPosition(t, mustBalance(t, l, "k", daily), 10, 10)
			// Settling twice charges twice but does not drive reserved negative.
			mustSettle(t, l, "k", daily, held, 1)
			mustSettle(t, l, "k", daily, held, 1)
			wantPosition(t, mustBalance(t, l, "k", daily), 12, 0)
		}},
		{"zero amounts are admitted until the budget is used up", func(t *testing.T, h harness) {
			l := h.ledger
			mustReserve(t, l, "k", daily, "z1", 0)
			mustReserve(t, l, "k", daily, "a", 100)
			if _, err := l.Reserve(context.Background(), "k", daily, "z2", 0); !errors.Is(err, ErrExhausted) {
				t.Fatalf("zero Reserve on a full budget: err = %v", err)
			}
			zero := Budget{Limit: 0, Period: PeriodTotal}
			if _, err := l.Reserve(context.Background(), "k", zero, "z3", 0); !errors.Is(err, ErrExhausted) {
				t.Fatalf("zero Reserve on a zero budget: err = %v", err)
			}
		}},
		{"reserving an id again replaces its hold", func(t *testing.T, h harness) {
			l := h.ledger
			mustReserve(t, l, "k", daily, "a", 90)
			mustReserve(t, l, "k", daily, "a", 60)
			wantPosition(t, mustBalance(t, l, "k", daily), 0, 60)
		}},
		{"an expired hold is charged at its held amount by the next call", func(t *testing.T, h harness) {
			l := h.ledger
			mustReserve(t, l, "k", daily, "a", 70)
			h.set(t0.Add(59 * time.Second))
			wantPosition(t, mustBalance(t, l, "k", daily), 0, 70)
			h.set(t0.Add(61 * time.Second))
			// The hold moves from reserved to spent: expiry frees nothing.
			wantPosition(t, mustBalance(t, l, "k", daily), 70, 0)
			if _, err := l.Reserve(context.Background(), "k", daily, "b", 31); !errors.Is(err, ErrExhausted) {
				t.Fatalf("Reserve past what the expired hold left: err = %v", err)
			}
		}},
		{"a late settle corrects the expiry charge by actual minus held", func(t *testing.T, h harness) {
			l := h.ledger
			below := mustReserve(t, l, "k", daily, "below", 40)
			above := mustReserve(t, l, "k", daily, "above", 20)
			zero := mustReserve(t, l, "k", daily, "zero", 0)
			h.set(t0.Add(61 * time.Second))
			live := mustReserve(t, l, "k", daily, "live", 30)
			wantPosition(t, mustBalance(t, l, "k", daily), 60, 30)
			mustSettle(t, l, "k", daily, below, 15)
			wantPosition(t, mustBalance(t, l, "k", daily), 35, 30)
			mustSettle(t, l, "k", daily, above, 25)
			wantPosition(t, mustBalance(t, l, "k", daily), 40, 30)
			// A zero hold charges nothing when it expires, so its late
			// settle is a plain charge.
			mustSettle(t, l, "k", daily, zero, 5)
			wantPosition(t, mustBalance(t, l, "k", daily), 45, 30)
			// The tombstone is used once: settling again is a plain
			// charge, like settling a live hold twice.
			mustSettle(t, l, "k", daily, below, 1)
			wantPosition(t, mustBalance(t, l, "k", daily), 46, 30)
			// The live hold is untouched by any of it.
			mustSettle(t, l, "k", daily, live, 10)
			wantPosition(t, mustBalance(t, l, "k", daily), 56, 0)
		}},
		{"a tombstone outlives many hold TTLs", func(t *testing.T, h harness) {
			// A settle can wait in a replica's pending charges through an
			// outage far longer than the hold TTL; it must still find the
			// tombstone and not charge the cost a second time.
			l := h.ledger
			r := mustReserve(t, l, "k", daily, "a", 50)
			h.set(t0.Add(10 * time.Hour))
			wantPosition(t, mustBalance(t, l, "k", daily), 50, 0)
			mustSettle(t, l, "k", daily, r, 30)
			wantPosition(t, mustBalance(t, l, "k", daily), 30, 0)
		}},
		{"an id re-reserved and expired twice is corrected once for both", func(t *testing.T, h harness) {
			l := h.ledger
			mustReserve(t, l, "k", daily, "a", 40)
			h.set(t0.Add(61 * time.Second))
			wantPosition(t, mustBalance(t, l, "k", daily), 40, 0)
			r := mustReserve(t, l, "k", daily, "a", 20)
			h.set(t0.Add(122 * time.Second))
			wantPosition(t, mustBalance(t, l, "k", daily), 60, 0)
			mustSettle(t, l, "k", daily, r, 15)
			wantPosition(t, mustBalance(t, l, "k", daily), 15, 0)
		}},
		{"a hold that expires in a window's last second is corrected there", func(t *testing.T, h harness) {
			l := h.ledger
			late := time.Date(2026, 10, 27, 23, 59, 30, 0, time.UTC)
			h.set(late)
			r := mustReserve(t, l, "k", daily, "a", 50)
			h.set(late.Add(2 * time.Minute))
			mustSettle(t, l, "k", daily, r, 20)
			h.set(late)
			wantPosition(t, mustBalance(t, l, "k", daily), 20, 0)
		}},
		{"daily rollover starts a fresh balance, settles land in their window", func(t *testing.T, h harness) {
			l := h.ledger
			late := time.Date(2026, 10, 27, 23, 59, 59, 0, time.UTC)
			h.set(late)
			r := mustReserve(t, l, "k", daily, "a", 100)
			h.set(late.Add(2 * time.Second))
			bal := mustBalance(t, l, "k", daily)
			if bal.Window != "2026-10-28" {
				t.Fatalf("window = %q", bal.Window)
			}
			wantPosition(t, bal, 0, 0)
			mustReserve(t, l, "k", daily, "b", 100)
			mustSettle(t, l, "k", daily, r, 25)
			wantPosition(t, mustBalance(t, l, "k", daily), 0, 100)
			h.set(late)
			wantPosition(t, mustBalance(t, l, "k", daily), 25, 0)
		}},
		{"monthly rollover", func(t *testing.T, h harness) {
			l := h.ledger
			monthly := Budget{Limit: 1000, Period: PeriodMonthly}
			end := time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
			h.set(end)
			r := mustReserve(t, l, "k", monthly, "a", 900)
			h.set(end.Add(2 * time.Minute))
			if bal := mustBalance(t, l, "k", monthly); bal.Window != "2026-11" || bal.Spent != 0 || bal.Reserved != 0 {
				t.Fatalf("November balance = %+v", bal)
			}
			mustSettle(t, l, "k", monthly, r, 800)
			h.set(end)
			if bal := mustBalance(t, l, "k", monthly); bal.Window != "2026-10" || bal.Spent != 800 || bal.Reserved != 0 {
				t.Fatalf("October balance = %+v", bal)
			}
		}},
		{"total budgets never roll over", func(t *testing.T, h harness) {
			l := h.ledger
			total := Budget{Limit: 100, Period: PeriodTotal}
			mustSettle(t, l, "k", total, mustReserve(t, l, "k", total, "a", 10), 10)
			h.set(t0.AddDate(1, 0, 0))
			bal := mustBalance(t, l, "k", total)
			if bal.Window != "total" || bal.Spent != 10 {
				t.Fatalf("balance a year later = %+v", bal)
			}
		}},
		{"sums are exact", func(t *testing.T, h harness) {
			l := h.ledger
			big := Budget{Limit: MaxExact - 1, Period: PeriodTotal}
			var want cost.Nanos
			for i := range 10_000 {
				// Amounts that are not representable as binary fractions
				// of a dollar, the case floating point gets wrong.
				amount := cost.Nanos(123_456_789 + i*7_919)
				want += amount
				mustSettle(t, l, "k", big, Reservation{}, amount)
			}
			if got := mustBalance(t, l, "k", big).Spent; got != want {
				t.Fatalf("spent = %d, want %d", got, want)
			}
		}},
		{"invalid input is rejected", func(t *testing.T, h harness) {
			l := h.ledger
			ctx := context.Background()
			bad := []error{
				func() error { _, err := l.Reserve(ctx, "k", daily, "", 1); return err }(),
				func() error { _, err := l.Reserve(ctx, "k", daily, "a", -1); return err }(),
				func() error { _, err := l.Reserve(ctx, "k", Budget{Limit: 1, Period: "weekly"}, "a", 1); return err }(),
				func() error {
					_, err := l.Reserve(ctx, "k", Budget{Limit: MaxExact, Period: PeriodTotal}, "a", 1)
					return err
				}(),
				l.Settle(ctx, "k", daily, Reservation{}, -1),
				func() error { _, err := l.Balance(ctx, "k", Budget{Limit: -1, Period: PeriodTotal}); return err }(),
			}
			for i, err := range bad {
				if err == nil || errors.Is(err, ErrExhausted) {
					t.Errorf("case %d: err = %v, want a validation error", i, err)
				}
			}
		}},
	}
	for _, impl := range implementations {
		for _, tc := range cases {
			t.Run(impl.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				tc.run(t, impl.new(t, time.Minute))
			})
		}
	}
}

func TestRedisKeyLayout(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	m.SetTime(t0)
	l := NewRedis(rdb, "sb:", time.Minute, func() time.Time { return t0 })
	ctx := context.Background()
	for _, p := range []Period{PeriodDaily, PeriodMonthly, PeriodTotal} {
		b := Budget{Limit: 100, Period: p}
		mustReserve(t, l, "team", b, "a", 10)
	}
	cases := []struct {
		key string
		ttl time.Duration
	}{
		{"sb:bud:{team:2026-10-27}:bal", 72 * time.Hour},
		{"sb:bud:{team:2026-10-27}:exp", 72 * time.Hour},
		{"sb:bud:{team:2026-10-27}:amt", 72 * time.Hour},
		{"sb:bud:{team:2026-10}:bal", 70 * 24 * time.Hour},
		{"sb:bud:{team:total}:bal", 0},
		{"sb:bud:{team:total}:amt", 0},
	}
	for _, tc := range cases {
		if !m.Exists(tc.key) {
			t.Errorf("key %s missing; have %v", tc.key, m.Keys())
			continue
		}
		if got := m.TTL(tc.key); got != tc.ttl {
			t.Errorf("TTL(%s) = %v, want %v", tc.key, got, tc.ttl)
		}
	}
	if n, err := rdb.DBSize(ctx).Result(); err != nil || n != 9 {
		t.Errorf("DBSize = %d, %v; want 9", n, err)
	}
}

// TestRedisTombstoneLivesWithTheWindow checks the tombstone hash takes the
// window's TTL, not the hold's: a settle parked through an outage longer
// than two hold TTLs must still find it.
func TestRedisTombstoneLivesWithTheWindow(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := &clock{t: t0}
	m.SetTime(t0)
	l := NewRedis(rdb, "sb:", time.Minute, c.Now)
	daily := Budget{Limit: 100, Period: PeriodDaily}
	total := Budget{Limit: 100, Period: PeriodTotal}
	rd := mustReserve(t, l, "team", daily, "a", 10)
	rt := mustReserve(t, l, "team", total, "a", 10)
	c.Set(t0.Add(3 * time.Minute))
	m.SetTime(t0.Add(3 * time.Minute))
	// The other keys' TTLs count down only as the server's clock moves;
	// FastForward moves it by two hold TTLs and more.
	m.FastForward(3 * time.Minute)
	wantPosition(t, mustBalance(t, l, "team", daily), 10, 0)
	wantPosition(t, mustBalance(t, l, "team", total), 10, 0)
	for _, tc := range []struct {
		key string
		ttl time.Duration
	}{
		{"sb:bud:{team:2026-10-27}:tomb", 72 * time.Hour},
		{"sb:bud:{team:total}:tomb", 0},
	} {
		if !m.Exists(tc.key) {
			t.Fatalf("tombstone %s missing; have %v", tc.key, m.Keys())
		}
		if got := m.TTL(tc.key); got != tc.ttl {
			t.Errorf("TTL(%s) = %v, want %v", tc.key, got, tc.ttl)
		}
	}
	mustSettle(t, l, "team", daily, rd, 4)
	mustSettle(t, l, "team", total, rt, 4)
	wantPosition(t, mustBalance(t, l, "team", daily), 4, 0)
	wantPosition(t, mustBalance(t, l, "team", total), 4, 0)
	if v := m.HGet("sb:bud:{team:2026-10-27}:tomb", "a"); v != "" {
		t.Errorf("tombstone still there after its settle: %q", v)
	}
}

func TestMemoryDropsOldWindows(t *testing.T) {
	t.Parallel()
	c := &clock{t: t0}
	l := NewMemory(time.Minute, c.Now)
	daily := Budget{Limit: 100, Period: PeriodDaily}
	total := Budget{Limit: 100, Period: PeriodTotal}
	mustSettle(t, l, "k", daily, Reservation{}, 5)
	mustSettle(t, l, "k", total, Reservation{}, 5)
	c.Set(t0.Add(4 * 24 * time.Hour))
	mustBalance(t, l, "k", daily) // triggers the sweep
	l.mu.Lock()
	n := len(l.accounts)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("accounts after four days = %d, want 1 (the total budget)", n)
	}
}

// TestRedisConcurrentReplicas is the cross-replica property: three ledgers
// on three clients, many concurrent reservations against one key, and the
// admitted sum never exceeds the limit.
func TestRedisConcurrentReplicas(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	var replicas []Ledger
	for range 3 {
		rdb := redis.NewClient(&redis.Options{Addr: m.Addr(), PoolSize: 20})
		t.Cleanup(func() { _ = rdb.Close() })
		replicas = append(replicas, NewRedis(rdb, "test:", time.Minute, nil))
	}
	concurrentProperty(t, replicas, "key")
}

// concurrentProperty reserves from 300 goroutines spread over replicas,
// checks the admitted sum against the limit, then settles every admitted
// reservation and checks spent is the exact sum of actuals.
func concurrentProperty(t *testing.T, replicas []Ledger, key string) {
	t.Helper()
	ctx := context.Background()
	b := Budget{Limit: 10 * cost.Dollar, Period: PeriodMonthly}
	type admitted struct {
		l Ledger
		r Reservation
	}
	var (
		mu       sync.Mutex
		ok       []admitted
		refused  int
		reserved cost.Nanos
		wg       sync.WaitGroup
	)
	for i := range 300 {
		wg.Go(func() {
			l := replicas[i%len(replicas)]
			amount := cost.Nanos(rand.Int64N(int64(10*cost.Cent))) + 1
			r, err := l.Reserve(ctx, key, b, fmt.Sprintf("req-%d", i), amount)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok = append(ok, admitted{l, r})
				reserved += r.Amount
			case errors.Is(err, ErrExhausted):
				refused++
			default:
				t.Errorf("Reserve: %v", err)
			}
		})
	}
	wg.Wait()
	if reserved > b.Limit {
		t.Fatalf("admitted %v against a limit of %v", reserved, b.Limit)
	}
	if refused == 0 {
		t.Fatalf("nothing was refused; the test does not exercise the limit")
	}
	bal, err := replicas[0].Balance(ctx, key, b)
	if err != nil || bal.Reserved != reserved || bal.Spent != 0 {
		t.Fatalf("balance after reserving = %+v, %v; want reserved %v", bal, err, reserved)
	}

	var spent cost.Nanos
	for _, a := range ok {
		actual := cost.Nanos(rand.Int64N(int64(a.r.Amount) + 1))
		spent += actual
		wg.Go(func() {
			if err := a.l.Settle(ctx, key, b, a.r, actual); err != nil {
				t.Errorf("Settle: %v", err)
			}
		})
	}
	wg.Wait()
	bal, err = replicas[len(replicas)-1].Balance(ctx, key, b)
	if err != nil || bal.Spent != spent || bal.Reserved != 0 {
		t.Fatalf("balance after settling = %+v, %v; want spent %v reserved 0", bal, err, spent)
	}
	t.Logf("%d admitted (%v), %d refused, %v spent", len(ok), reserved, refused, spent)
}

func TestMemoryConcurrent(t *testing.T) {
	t.Parallel()
	concurrentProperty(t, []Ledger{NewMemory(time.Minute, nil)}, "key")
}

package budget

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/cost"
)

func newSim(t *testing.T, planted bool) *Simulator {
	t.Helper()
	cfg := DefaultSimConfig()
	cfg.Planted = planted
	s, err := NewSimulator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func reserve(n, replica int, hold, c int64) SimStep {
	return SimStep{Op: OpReserve, Replica: replica, Hold: cost.Nanos(hold), Cost: cost.Nanos(c), N: n}
}

func op(n int, o SimOp, replica int) SimStep { return SimStep{Op: o, Replica: replica, N: n} }

// TestSimSchedules runs hand-written schedules, one per way of losing (or
// not losing) spend, and checks the invariants, PLAN.md's rule and the
// fault-log classes on each.
func TestSimSchedules(t *testing.T) {
	t.Parallel()
	ttl := DefaultSimConfig().HoldTTL
	tests := []struct {
		name         string
		steps        []SimStep
		billed       int64
		spent        int64
		i1, i3       bool
		rule         string
		classes      []string
		wantExpired  []string
		wantLost     bool
		wantExercise []SimFault
	}{
		{
			name:   "reserve and settle",
			steps:  []SimStep{reserve(0, 0, 20, 10), op(1, OpSettle, 0)},
			billed: 10, spent: 10, i1: true, i3: true, rule: ClassOther,
		},
		{
			// Settled at quiescence: an unsettled request on a live
			// replica is not lost.
			name:   "left in flight on a live replica",
			steps:  []SimStep{reserve(0, 1, 40, 30)},
			billed: 30, spent: 30, i1: true, i3: true, rule: ClassOther,
		},
		{
			name:   "killed with a Redis hold in flight",
			steps:  []SimStep{reserve(0, 0, 20, 10), op(1, OpKill, 0)},
			billed: 10, spent: 0, i3: true,
			rule:         ClassExpiredHold,
			classes:      []string{ClassExpiredHold},
			wantExpired:  []string{"q0"},
			wantExercise: []SimFault{FaultKill},
		},
		{
			name: "killed holding pending spend",
			steps: []SimStep{op(0, OpStoreDown, 0), reserve(1, 0, 20, 10),
				op(2, OpSettle, 0), op(3, OpKill, 0)},
			billed: 10, spent: 0, i3: true,
			rule:         ClassKilledPending,
			classes:      []string{ClassKilledPending},
			wantExercise: []SimFault{FaultOutage, FaultKill},
		},
		{
			// Neither pending spend nor a hold on the store: the cost was
			// only ever in the dead process's grace allowance.
			name:   "killed with a grace hold in flight",
			steps:  []SimStep{op(0, OpStoreDown, 0), reserve(1, 0, 20, 10), op(2, OpKill, 0)},
			billed: 10, spent: 0, i3: true,
			rule:         ClassOther,
			classes:      []string{ClassOther},
			wantExercise: []SimFault{FaultOutage, FaultKill},
		},
		{
			// The store applied the settle; the replica keeps the cost as
			// pending and charges it again.
			name:   "settle reply lost",
			steps:  []SimStep{reserve(0, 0, 20, 10), op(1, OpSettleLost, 0)},
			billed: 10, spent: 20, i1: true, i3: true,
			rule:         ClassOther,
			wantLost:     true,
			wantExercise: []SimFault{FaultSettleLost},
		},
		{
			// The store holds q0 but the replica admitted it on grace; the
			// orphaned hold expires and the cost is charged once.
			name:   "reserve reply lost",
			steps:  []SimStep{{Op: OpReserveLost, Hold: 20, Cost: 10}, op(1, OpSettle, 0)},
			billed: 10, spent: 10, i1: true, i3: true,
			rule:         ClassExpiredHold,
			wantExpired:  []string{"q0"},
			wantLost:     true,
			wantExercise: []SimFault{FaultReserveLost},
		},
		{
			// Settled while the store is down: the hold stays and expires,
			// the cost waits in pending and is flushed. Nothing lost.
			name: "settled during an outage past the TTL",
			steps: []SimStep{reserve(0, 0, 20, 10), op(1, OpStoreDown, 0), op(2, OpSettle, 0),
				{Op: OpClockTTL, Advance: ttl + time.Second, N: 3}, op(4, OpStoreUp, 0)},
			billed: 10, spent: 10, i1: true, i3: true,
			rule:         ClassExpiredHold,
			wantExpired:  []string{"q0"},
			wantExercise: []SimFault{FaultOutage, FaultClockTTL},
		},
	}
	s := newSim(t, false)
	for _, tt := range tests {
		o, err := s.Run(7, tt.steps, true)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if int64(o.Billed) != tt.billed || int64(o.Spent) != tt.spent {
			t.Errorf("%s: billed %d spent %d, want %d and %d\n%v", tt.name, o.Billed, o.Spent, tt.billed, tt.spent, o.Trace)
		}
		if o.I1 != tt.i1 || o.I3 != tt.i3 || !o.I2 {
			t.Errorf("%s: I1 %v I2 %v I3 %v, want %v true %v", tt.name, o.I1, o.I2, o.I3, tt.i1, tt.i3)
		}
		if got := o.Classify(); got != tt.rule {
			t.Errorf("%s: rule class %q, want %q", tt.name, got, tt.rule)
		}
		if got := o.Classes(); !slices.Equal(got, tt.classes) {
			t.Errorf("%s: classes %q, want %q", tt.name, got, tt.classes)
		}
		if !slices.Equal(o.Expired, tt.wantExpired) {
			t.Errorf("%s: expired %q, want %q", tt.name, o.Expired, tt.wantExpired)
		}
		if o.LostReply != tt.wantLost {
			t.Errorf("%s: lost reply %v, want %v", tt.name, o.LostReply, tt.wantLost)
		}
		if !slices.Equal(o.Exercised, tt.wantExercise) {
			t.Errorf("%s: exercised %q, want %q", tt.name, o.Exercised, tt.wantExercise)
		}
		if o.Reserved != 0 {
			t.Errorf("%s: reserved %d at quiescence", tt.name, o.Reserved)
		}
	}
}

// TestSimPlanted checks the planted bug: off by default, and when on, a
// settle it picks releases the hold and charges nothing, a loss no kill
// accounts for.
func TestSimPlanted(t *testing.T) {
	t.Parallel()
	if l := NewRedis(nil, "", 0, nil); l.dropSettle != nil {
		t.Fatal("NewRedis armed the planted bug")
	}
	s := newSim(t, true)
	// Find a request id the planted bug picks for seed 1.
	r := &simRun{seed: 1}
	n := 0
	for !r.dropSettle("q" + strconv.Itoa(n)) {
		n++
	}
	o, err := s.Run(1, []SimStep{reserve(n, 0, 20, 10), op(n+1, OpSettle, 0)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if o.Planted != 1 || o.Spent != 0 || o.Billed != 10 {
		t.Fatalf("planted %d spent %d billed %d, want 1, 0, 10\n%v", o.Planted, o.Spent, o.Billed, o.Trace)
	}
	if got, want := o.Classes(), []string{ClassOther, ClassUnaccounted}; !slices.Equal(got, want) {
		t.Fatalf("classes %q, want %q", got, want)
	}
	if got := o.Classify(); got != ClassOther {
		t.Fatalf("rule class %q, want %q", got, ClassOther)
	}
	// The same schedule without the switch loses nothing.
	o, err = newSim(t, false).Run(1, []SimStep{reserve(n, 0, 20, 10), op(n+1, OpSettle, 0)}, false)
	if err != nil || !o.I1 || o.Planted != 0 {
		t.Fatalf("unplanted run: %v, I1 %v, planted %d", err, o.I1, o.Planted)
	}
}

// TestSimBound checks I2 at its edge with no faults: one process admits
// up to the limit, and with three processes and no time passing the bound
// is three graces.
func TestSimBound(t *testing.T) {
	t.Parallel()
	s := newSim(t, false)
	cfg := s.Config()
	o := SimOutcome{Billed: cfg.Limit + 3*cfg.Grace, Processes: 3}
	if b, ok := s.bound(o); !ok || b != 3*cfg.Grace {
		t.Fatalf("bound %d held %v, want %d true", b, ok, 3*cfg.Grace)
	}
	o.Billed++
	if _, ok := s.bound(o); ok {
		t.Fatal("one nanodollar over the bound held")
	}
	// Half a refill period adds half a grace per process; under-estimates
	// add themselves.
	o = SimOutcome{Billed: cfg.Limit + 3*cfg.Grace*3/2 + 7, Processes: 3, Elapsed: cfg.GraceRefill / 2, Under: 7}
	if b, ok := s.bound(o); !ok || b != 3*cfg.Grace*3/2+7 {
		t.Fatalf("bound %d held %v", b, ok)
	}
}

// TestSimDeterministic runs the same seeds twice on two simulators.
func TestSimDeterministic(t *testing.T) {
	t.Parallel()
	a, b := newSim(t, false), newSim(t, false)
	for seed := uint64(0); seed < 20; seed++ {
		steps := Generate(seed, a.Config())
		if !reflect.DeepEqual(steps, Generate(seed, b.Config())) {
			t.Fatalf("seed %d: two schedules", seed)
		}
		if len(steps) < 1 || len(steps) > a.Config().MaxSteps {
			t.Fatalf("seed %d: %d steps", seed, len(steps))
		}
		x, err1 := a.Run(seed, steps, true)
		y, err2 := b.Run(seed, steps, true)
		if err := errors.Join(err1, err2); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("seed %d: two outcomes\n%+v\n%+v", seed, x, y)
		}
	}
}

// TestSimShrinkIsMinimal shrinks a padded schedule and checks that every
// remaining step is needed.
func TestSimShrinkIsMinimal(t *testing.T) {
	t.Parallel()
	s := newSim(t, false)
	steps := []SimStep{
		reserve(0, 1, 10, 5), op(1, OpSettle, 1), op(2, OpStoreDown, 0),
		{Op: OpClockShort, Advance: 2 * time.Second, N: 3}, reserve(4, 2, 40, 40),
		reserve(5, 0, 20, 10), op(6, OpSettle, 2), op(7, OpSettle, 0), op(8, OpStoreUp, 0),
		op(9, OpKill, 0), reserve(10, 1, 20, 20),
	}
	pred := func(o SimOutcome) bool { return slices.Contains(o.Classes(), ClassKilledPending) }
	o, err := s.Run(3, steps, false)
	if err != nil || !pred(o) {
		t.Fatalf("padded schedule: %v, classes %q", err, o.Classes())
	}
	minimal, mo, err := s.Shrink(3, steps, pred)
	if err != nil {
		t.Fatal(err)
	}
	want := []SimStep{steps[2], steps[5], steps[7], steps[9]}
	if !reflect.DeepEqual(minimal, want) {
		t.Fatalf("minimal %v, want %v", minimal, want)
	}
	if len(mo.Trace) == 0 || !pred(mo) {
		t.Fatalf("minimal run not traced or not failing: %+v", mo)
	}
	for i := range minimal {
		o, err := s.Run(3, slices.Delete(slices.Clone(minimal), i, i+1), false)
		if err != nil {
			t.Fatal(err)
		}
		if pred(o) {
			t.Fatalf("step %d of the minimal schedule is not needed", i)
		}
	}
}

// TestSimConnLosesAppliedReply checks the fault wrapper directly: a lost
// reply comes after the store applied the call, and a down store applies
// nothing.
func TestSimConnLosesAppliedReply(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	run := &simRun{sim: &Simulator{m: m}, exercised: map[SimFault]bool{}, expired: map[string]bool{}}
	c := &simConn{run: run, inner: rdb}
	ctx := context.Background()
	c.lose = true
	if err := c.Eval(ctx, "return redis.call('INCR', KEYS[1])", []string{"n"}).Err(); !errors.Is(err, errSimLost) {
		t.Fatalf("lost call: %v", err)
	}
	if got := m.HGet("n", ""); got != "" {
		t.Fatalf("unexpected hash %q", got)
	}
	if v, _ := m.Get("n"); v != "1" {
		t.Fatalf("store did not apply the call: n = %q", v)
	}
	if !c.disarm() || !run.lostReply {
		t.Fatal("lost reply not recorded")
	}
	run.down = true
	if err := c.Eval(ctx, "return redis.call('INCR', KEYS[1])", []string{"n"}).Err(); !errors.Is(err, errSimUnreachable) {
		t.Fatalf("down call: %v", err)
	}
	if v, _ := m.Get("n"); v != "1" || !run.exercised[FaultOutage] {
		t.Fatalf("down store applied the call (n = %q) or outage not recorded", v)
	}
}

// plantedVictim returns the first request number whose id the planted bug
// picks for seed.
func plantedVictim(seed uint64) int {
	r := &simRun{seed: seed}
	n := 0
	for !r.dropSettle("q" + strconv.Itoa(n)) {
		n++
	}
	return n
}

// TestSimAccounting checks that the fault log accounts for every run's
// money exactly, double charges included, so a lost reply's second charge
// cannot hide a loss and a planted settle the store never applied is not
// counted as fired.
func TestSimAccounting(t *testing.T) {
	t.Parallel()
	n := plantedVictim(1)
	tests := []struct {
		name                   string
		planted                bool
		steps                  []SimStep
		billed, spent, doubled int64
		lostPending, lostGrace int64
		plantedN               int
		plantedLost            int64
		i1                     bool
		classes                []string
	}{
		{
			// r0's pending is a lost settle's cost the store already has,
			// so killing r0 loses nothing; r1's grace request is lost.
			// Without the double charge the shortfall (10) fits inside
			// either kill's loss and no class was needed.
			name: "kill drops pending the store already applied",
			steps: []SimStep{reserve(0, 0, 20, 10), op(1, OpSettleLost, 0), op(2, OpStoreDown, 0),
				reserve(3, 1, 20, 10), op(4, OpKill, 1), op(5, OpKill, 0)},
			billed: 20, spent: 10, lostGrace: 10,
			classes: []string{ClassOther},
		},
		{
			// r0 flushes its lost settle's cost again, which pays for the
			// grace request r1 lost: I1 holds, the double charge is seen.
			name: "double charge covers a lost grace request",
			steps: []SimStep{reserve(0, 0, 20, 10), op(1, OpSettleLost, 0), op(2, OpStoreDown, 0),
				reserve(3, 1, 20, 10), op(4, OpKill, 1), op(5, OpStoreUp, 0)},
			billed: 20, spent: 20, doubled: 10, lostGrace: 10, i1: true,
		},
		{
			name:    "planted settle refused by a down store",
			planted: true,
			steps:   []SimStep{reserve(n, 0, 20, 10), op(n+1, OpStoreDown, 0), op(n+2, OpSettle, 0)},
			billed:  10, spent: 10, i1: true,
		},
		{
			// The store applied the emptied settle but the replica never
			// heard, so it charges the cost from pending: fired, nothing
			// lost.
			name:    "planted settle with its reply lost",
			planted: true,
			steps:   []SimStep{reserve(n, 0, 20, 10), op(n+1, OpSettleLost, 0)},
			billed:  10, spent: 10, plantedN: 1, i1: true,
		},
		{
			name:    "planted settle applied",
			planted: true,
			steps:   []SimStep{reserve(n, 0, 20, 10), op(n+1, OpSettle, 0)},
			billed:  10, spent: 0, plantedN: 1, plantedLost: 10,
			classes: []string{ClassOther, ClassUnaccounted},
		},
	}
	for _, tt := range tests {
		o, err := newSim(t, tt.planted).Run(1, tt.steps, true)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		got := []int64{int64(o.Billed), int64(o.Spent), int64(o.Doubled), int64(o.LostPending), int64(o.LostGraceHold), int64(o.Planted), int64(o.PlantedLost)}
		want := []int64{tt.billed, tt.spent, tt.doubled, tt.lostPending, tt.lostGrace, int64(tt.plantedN), tt.plantedLost}
		if !slices.Equal(got, want) {
			t.Errorf("%s: billed, spent, doubled, pending, grace, planted, planted lost = %v, want %v\n%v", tt.name, got, want, o.Trace)
		}
		if !o.Balanced() {
			t.Errorf("%s: not balanced (unaccounted %d)", tt.name, o.Unaccounted())
		}
		if o.I1 != tt.i1 || !o.I3 {
			t.Errorf("%s: I1 %v I3 %v, want %v true", tt.name, o.I1, o.I3, tt.i1)
		}
		if c := o.Classes(); !slices.Equal(c, tt.classes) {
			t.Errorf("%s: classes %q, want %q", tt.name, c, tt.classes)
		}
	}
}

// TestSimBoundNeedsHoldsToOutliveNoRequest pins the case I2's bound does
// not cover: holds that expire under requests still in flight on a live
// process free budget with no outage at all. The gateway's config rules
// it out (budget.reservation_ttl must be longer than every provider's
// timeout), but the generator can draw it, so a clean I2 depends on the
// search not reaching it.
func TestSimBoundNeedsHoldsToOutliveNoRequest(t *testing.T) {
	t.Parallel()
	ttl := DefaultSimConfig().HoldTTL
	var steps []SimStep
	for round := range 4 {
		if round > 0 {
			steps = append(steps, SimStep{Op: OpClockTTL, Advance: ttl + time.Second, N: len(steps)})
		}
		for _, h := range []int64{40, 40, 20} {
			steps = append(steps, reserve(len(steps), 0, h, h))
		}
	}
	o, err := newSim(t, false).Run(1, steps, false)
	if err != nil {
		t.Fatal(err)
	}
	// Three processes, three hold TTLs plus 3 s: 150 × (1 + 45m3s / 1h).
	if o.Billed != 400 || o.Bound != 262 || o.I2 || !o.I1 || !o.Balanced() {
		t.Fatalf("billed %d bound %d, I2 %v I1 %v balanced %v; want 400, 262, false, true, true", o.Billed, o.Bound, o.I2, o.I1, o.Balanced())
	}
}

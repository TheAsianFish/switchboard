package budgetsim

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/budget"
)

// TestRunArmIsOrderIndependent checks an arm on two worker counts gives
// the same summary and records, and that records round-trip.
func TestRunArmIsOrderIndependent(t *testing.T) {
	t.Parallel()
	cfg := budget.DefaultSimConfig()
	a1, r1, err := RunArm(context.Background(), ArmMain, cfg, 0, 12, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	a3, r3, err := RunArm(context.Background(), ArmMain, cfg, 0, 12, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	a1.WallSeconds, a3.WallSeconds = 0, 0
	if !reflect.DeepEqual(a1, a3) || !reflect.DeepEqual(r1, r3) {
		t.Fatalf("one worker and three disagree:\n%+v\n%+v", a1, a3)
	}
	if a1.Seeds != 12 || a1.Steps == 0 || a1.Runs < 12 || len(a1.Exercised) != len(budget.SimFaults) {
		t.Fatalf("arm %+v", a1)
	}
	if len(r1) == 0 {
		t.Fatal("no seed lost spend in 12; the default config should")
	}
	for _, rec := range r1 {
		if len(rec.Rule)+len(rec.Classes) == 0 {
			t.Fatalf("record %+v has nothing to say", rec)
		}
	}
	path := filepath.Join(t.TempDir(), RecordsFile(ArmMain))
	if err := WriteRecords(path, r1); err != nil {
		t.Fatal(err)
	}
	back, err := ReadRecords(path)
	if err != nil || !reflect.DeepEqual(back, r1) {
		t.Fatalf("round trip: %v\n%+v\n%+v", err, back, r1)
	}
}

// TestSummariseCounts checks the aggregation on hand-made results.
func TestSummariseCounts(t *testing.T) {
	t.Parallel()
	cfg := budget.DefaultSimConfig()
	step := budget.SimStep{Op: budget.OpReserve, Hold: 10, Cost: 10}
	kill := budget.SimStep{Op: budget.OpKill, N: 1}
	lost := budget.SimOutcome{Billed: 10, LostRedisHold: 10, Exercised: []budget.SimFault{budget.FaultKill}}
	over := budget.SimOutcome{Billed: cfg.Limit + 50, Spent: cfg.Limit + 60, Bound: 300, LostReply: true, I1: true, I2: true}
	results := []budget.SimSeedResult{
		{Seed: 5, Steps: 3, Outcome: lost,
			Violations: []budget.SimViolation{{Invariant: "I1", Class: budget.ClassExpiredHold, Minimal: []budget.SimStep{step, kill}, Outcome: lost}},
			Classes:    []budget.SimClassShrink{{Class: budget.ClassExpiredHold, Minimal: []budget.SimStep{step, kill}, Outcome: lost}}},
		{Seed: 2, Steps: 4, Outcome: lost,
			Violations: []budget.SimViolation{{Invariant: "I1", Class: budget.ClassExpiredHold, Minimal: []budget.SimStep{step, step, kill}, Outcome: lost}}},
		{Seed: 9, Steps: 1, Outcome: over},
	}
	arm := Summarise(ArmMain, cfg, 0, results)
	if arm.Steps != 8 || arm.Exercised[1].Seeds != 2 || arm.OverLimit != 1 || arm.OverCounted != 1 || arm.OverCountedLost != 1 {
		t.Fatalf("arm %+v", arm)
	}
	if c := arm.Closest; c == nil || c.Seed != 9 || c.Overshoot != 50 || c.Bound != 300 {
		t.Fatalf("closest %+v", arm.Closest)
	}
	i1 := arm.Violations[0]
	if i1.Invariant != "I1" || i1.Seeds != 2 {
		t.Fatalf("I1 %+v", i1)
	}
	eh := i1.ByClass[1]
	if eh.Class != budget.ClassExpiredHold || eh.Seeds != 2 || !reflect.DeepEqual(eh.Lengths, []LenCount{{2, 1}, {3, 1}}) {
		t.Fatalf("expired hold %+v", eh)
	}
	if eh.Example == nil || eh.Example.Seed != 5 || len(eh.Example.Steps) != 2 {
		t.Fatalf("example %+v", eh.Example)
	}
	if arm.Classes[1].Seeds != 1 || arm.Classes[0].Seeds != 0 {
		t.Fatalf("classes %+v", arm.Classes)
	}
}

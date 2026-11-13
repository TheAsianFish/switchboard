package budgetsim

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/budget"
)

// committedDir is the committed `make sim-budget` run.
const committedDir = "../../eval/results/" + Dir

// sliceSeeds is how many seeds of each arm make check re-runs.
const sliceSeeds = 200

// TestSliceMatchesCommitted is the 200-seed slice of `make sim-budget`
// that runs in make check: seeds 0 to 199 of both arms, each schedule run
// once and compared with the committed records (billed, spent, the
// invariants broken and the fault-log classes), so a change to the
// ledger or the simulator that moves any of them fails here until the
// results are re-run. Every seed's money must balance against the fault
// log. The first seed of each class in the slice is also shrunk again and
// must reach the committed length.
func TestSliceMatchesCommitted(t *testing.T) {
	t.Parallel()
	var sum Summary
	b, err := os.ReadFile(filepath.Join(committedDir, "summary.json"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("no committed run; make sim-budget writes one")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatal(err)
	}
	for _, arm := range sum.Arms {
		if arm.FirstSeed != 0 || arm.Seeds < sliceSeeds {
			t.Fatalf("arm %s covers seeds %d to %d, not the slice", arm.Name, arm.FirstSeed, arm.FirstSeed+uint64(arm.Seeds))
		}
		recs, err := ReadRecords(filepath.Join(committedDir, RecordsFile(arm.Name)))
		if err != nil {
			t.Fatal(err)
		}
		cfg := budget.DefaultSimConfig()
		cfg.Planted = arm.Planted
		t.Run(arm.Name, func(t *testing.T) {
			t.Parallel()
			checkSlice(t, cfg, recs)
		})
	}
}

func checkSlice(t *testing.T, cfg budget.SimConfig, recs []Record) {
	byseed := map[uint64]Record{}
	for _, r := range recs {
		byseed[r.Seed] = r
	}
	outcomes := make([]budget.SimOutcome, sliceSeeds)
	const workers = 4
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sim, err := budget.NewSimulator(cfg)
			if err != nil {
				errs[w] = err
				return
			}
			defer sim.Close()
			for i := w; i < sliceSeeds; i += workers {
				seed := uint64(i)
				if outcomes[i], err = sim.Run(seed, budget.Generate(seed, cfg), false); err != nil {
					errs[w] = err
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	shrinkFirst := map[string]bool{}
	sim, err := budget.NewSimulator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	for i, o := range outcomes {
		seed := uint64(i)
		if !o.Balanced() {
			t.Errorf("seed %d: the fault log does not account for the money: billed %d spent %d doubled %d, unaccounted %d against %d planted",
				seed, o.Billed, o.Spent, o.Doubled, o.Unaccounted(), o.PlantedLost)
		}
		rec, ok := byseed[seed]
		var broken []string
		for _, inv := range budget.Invariants {
			if !o.Holds(inv) {
				broken = append(broken, inv)
			}
		}
		if !ok {
			if len(broken) > 0 || len(o.Classes()) > 0 {
				t.Errorf("seed %d: broke %q, classes %q; the committed run recorded nothing", seed, broken, o.Classes())
			}
			continue
		}
		var wantBroken, wantClasses []string
		for _, r := range rec.Rule {
			wantBroken = append(wantBroken, r.Invariant)
		}
		for _, c := range rec.Classes {
			wantClasses = append(wantClasses, c.Class)
		}
		if int64(o.Billed) != rec.Billed || int64(o.Spent) != rec.Spent || !slices.Equal(broken, wantBroken) || !slices.Equal(o.Classes(), wantClasses) {
			t.Errorf("seed %d: billed %d spent %d broke %q classes %q; committed %d, %d, %q, %q",
				seed, o.Billed, o.Spent, broken, o.Classes(), rec.Billed, rec.Spent, wantBroken, wantClasses)
			continue
		}
		for _, c := range rec.Classes {
			if shrinkFirst[c.Class] {
				continue
			}
			shrinkFirst[c.Class] = true
			minimal, _, err := sim.Shrink(seed, budget.Generate(seed, cfg), func(o budget.SimOutcome) bool {
				return slices.Contains(o.Classes(), c.Class)
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(minimal) != c.Steps {
				t.Errorf("seed %d: %s shrank to %d steps, committed %d", seed, c.Class, len(minimal), c.Steps)
			}
		}
	}
	if cfg.Planted && !shrinkFirst[budget.ClassUnaccounted] {
		t.Error("the planted bug caused no unaccounted loss in the slice")
	}
	if !cfg.Planted && !shrinkFirst[budget.ClassOther] {
		t.Error("no seed in the slice needed the third way; the committed run says some do")
	}
}

// Package budgetsim runs the budget ledger's fault-schedule simulator
// (budget.Simulator) over a range of seeds, once as the code is and once
// with the planted bug, and writes what it found under
// eval/results/m11-budget-sim/: summary.json, which `make results`
// renders, and one gzipped JSON line per seed that lost spend or broke
// an invariant.
package budgetsim

import (
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/budget"
)

// Dir is the results subdirectory.
const Dir = "m11-budget-sim"

// The two arms.
const (
	ArmMain    = "main"
	ArmPlanted = "planted"
)

// Summary is summary.json.
type Summary struct {
	Meta   Meta   `json:"meta"`
	Config Config `json:"config"`
	Arms   []Arm  `json:"arms"`
}

// Meta records where and how the run was made.
type Meta struct {
	Commit   string         `json:"commit"`
	Date     string         `json:"date"`
	Command  string         `json:"command"`
	Workers  int            `json:"workers"`
	Hardware bench.Hardware `json:"hardware"`
}

// Config is the simulator configuration, in report units.
type Config struct {
	Replicas      int                `json:"replicas"`
	Limit         int64              `json:"limit"`
	Grace         int64              `json:"grace"`
	GraceRefill   string             `json:"grace_refill"`
	RetryInterval string             `json:"retry_interval"`
	HoldTTL       string             `json:"hold_ttl"`
	MaxSteps      int                `json:"max_steps"`
	Holds         []int64            `json:"holds"`
	Weights       []budget.SimWeight `json:"weights"`
	Faults        []budget.SimFault  `json:"faults"`
	Invariants    []string           `json:"invariants"`
	Classes       []string           `json:"classes"`
}

// Arm is one pass over the seeds.
type Arm struct {
	Name      string `json:"name"`
	Planted   bool   `json:"planted"`
	FirstSeed uint64 `json:"first_seed"`
	Seeds     int    `json:"seeds"`
	// Steps is the total length of the drawn schedules; Runs counts every
	// schedule executed, shrinking included.
	Steps       int     `json:"steps"`
	Runs        int     `json:"runs"`
	WallSeconds float64 `json:"wall_seconds"`
	// Exercised counts, per fault, the seeds whose full schedule
	// exercised it.
	Exercised []Count `json:"exercised"`
	// Violations counts, per invariant, the seeds that broke it, split by
	// PLAN.md's rule applied to the minimal schedule.
	Violations []Violation `json:"violations"`
	// Classes counts, per way of losing spend (budget.SimOutcome.Classes),
	// the seeds whose shortfall needs it, each shrunk without slipping.
	Classes []Class `json:"classes"`
	// PlantedFired counts seeds where the planted bug emptied a settle the
	// store applied in the full schedule, and PlantedLost those of them
	// where it kept spend out of the store (the replica heard the reply,
	// and the request cost something).
	PlantedFired int `json:"planted_fired"`
	PlantedLost  int `json:"planted_lost"`
	// PlantedFound counts the fired seeds whose loss is unaccounted.
	PlantedFound int `json:"planted_found"`
	// Unbalanced counts seeds whose money the fault log does not account
	// for exactly (budget.SimOutcome.Balanced): spent ≠ billed − every
	// recorded loss + the double charges.
	Unbalanced int `json:"unbalanced"`
	// UnaccountedPlanted counts the unaccounted class's minimal schedules
	// in which the planted bug fired.
	UnaccountedPlanted int `json:"unaccounted_planted"`
	// OverLimit counts seeds whose billed spend passed the limit, and
	// Closest is the one nearest its I2 bound (largest overshoot / bound),
	// so a clean I2 can be read against how hard it was pushed.
	OverLimit int        `json:"over_limit"`
	Closest   *Overshoot `json:"closest,omitempty"`
	// OverCounted counts seeds where spent ended above billed, and
	// OverCountedLost those of them with a lost reply (I3 says all).
	OverCounted     int `json:"over_counted"`
	OverCountedLost int `json:"over_counted_lost_reply"`
}

// Overshoot is one seed's billed spend over the limit against its bound.
type Overshoot struct {
	Seed      uint64 `json:"seed"`
	Overshoot int64  `json:"overshoot"`
	Bound     int64  `json:"bound"`
}

// Count is a named count of seeds.
type Count struct {
	Name  string `json:"name"`
	Seeds int    `json:"seeds"`
}

// Violation is one invariant's failures.
type Violation struct {
	Invariant string  `json:"invariant"`
	Seeds     int     `json:"seeds"`
	ByClass   []Class `json:"by_class"`
}

// Class is one class's seeds, the lengths their minimal schedules shrank
// to, and the shortest of them (lowest seed on a tie).
type Class struct {
	Class   string     `json:"class"`
	Seeds   int        `json:"seeds"`
	Lengths []LenCount `json:"lengths"`
	Example *Example   `json:"example,omitempty"`
}

// LenCount is how many minimal schedules had Steps steps.
type LenCount struct {
	Steps int `json:"steps"`
	Seeds int `json:"seeds"`
}

// Example is one minimal schedule and its traced run.
type Example struct {
	Seed      uint64   `json:"seed"`
	Steps     []string `json:"steps"`
	Trace     []string `json:"trace"`
	Billed    int64    `json:"billed"`
	Spent     int64    `json:"spent"`
	Bound     int64    `json:"bound"`
	Pending   int64    `json:"lost_pending"`
	RedisHold int64    `json:"lost_redis_hold"`
	GraceHold int64    `json:"lost_grace_hold"`
	Planted   int      `json:"planted"`
}

// Record is one line of seeds-<arm>.jsonl.gz: a seed that lost spend or
// broke an invariant.
type Record struct {
	Seed   uint64      `json:"seed"`
	Billed int64       `json:"billed"`
	Spent  int64       `json:"spent"`
	Rule   []RecordInv `json:"rule,omitempty"`
	// Classes are the full run's classes with each one's shrunk length.
	Classes []RecordClass `json:"classes,omitempty"`
}

// RecordInv is a broken invariant under PLAN.md's rule.
type RecordInv struct {
	Invariant string `json:"inv"`
	Class     string `json:"class"`
	Steps     int    `json:"steps"`
}

// RecordClass is a class and the length it shrank to.
type RecordClass struct {
	Class string `json:"class"`
	Steps int    `json:"steps"`
}

// RecordOf condenses a seed's result; ok is false if the seed neither
// lost spend nor broke an invariant.
func RecordOf(r budget.SimSeedResult) (Record, bool) {
	rec := Record{Seed: r.Seed, Billed: int64(r.Outcome.Billed), Spent: int64(r.Outcome.Spent)}
	for _, v := range r.Violations {
		rec.Rule = append(rec.Rule, RecordInv{Invariant: v.Invariant, Class: v.Class, Steps: len(v.Minimal)})
	}
	for _, c := range r.Classes {
		rec.Classes = append(rec.Classes, RecordClass{Class: c.Class, Steps: len(c.Minimal)})
	}
	return rec, len(rec.Rule) > 0 || len(rec.Classes) > 0
}

// ConfigOf describes cfg for the summary.
func ConfigOf(cfg budget.SimConfig) Config {
	c := Config{
		Replicas:      cfg.Replicas,
		Limit:         int64(cfg.Limit),
		Grace:         int64(cfg.Grace),
		GraceRefill:   cfg.GraceRefill.String(),
		RetryInterval: cfg.RetryInterval.String(),
		HoldTTL:       cfg.HoldTTL.String(),
		MaxSteps:      cfg.MaxSteps,
		Weights:       cfg.Weights,
		Faults:        budget.SimFaults,
		Invariants:    budget.Invariants,
		Classes:       []string{budget.ClassKilledPending, budget.ClassExpiredHold, budget.ClassOther, budget.ClassUnaccounted},
	}
	for _, h := range cfg.Holds {
		c.Holds = append(c.Holds, int64(h))
	}
	return c
}

// RunArm checks seeds [first, first+n) on workers simulators and returns
// the arm's summary and its records in seed order.
func RunArm(ctx context.Context, name string, cfg budget.SimConfig, first uint64, n, workers int, progress io.Writer) (Arm, []Record, error) {
	start := time.Now()
	results := make([]budget.SimSeedResult, n)
	runs := make([]int, workers)
	seeds := make(chan int)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sim, err := budget.NewSimulator(cfg)
			if err != nil {
				errs <- err
				return
			}
			defer sim.Close()
			for i := range seeds {
				r, err := sim.Check(first+uint64(i), true)
				if err != nil {
					errs <- err
					return
				}
				// Only the examples keep their traces.
				r.Outcome.Trace = nil
				results[i] = r
			}
			runs[w] = sim.Runs()
		}()
	}
	var err error
feed:
	for i := range n {
		select {
		case seeds <- i:
		case err = <-errs:
			break feed
		case <-ctx.Done():
			err = ctx.Err()
			break feed
		}
		if progress != nil && (i+1)%10000 == 0 {
			_, _ = fmt.Fprintf(progress, "budget-sim %s: %d/%d seeds, %s\n", name, i+1, n, time.Since(start).Round(time.Second))
		}
	}
	close(seeds)
	wg.Wait()
	if err == nil {
		select {
		case err = <-errs:
		default:
		}
	}
	if err != nil {
		return Arm{}, nil, fmt.Errorf("budget-sim %s: %w", name, err)
	}
	arm := Summarise(name, cfg, first, results)
	for _, r := range runs {
		arm.Runs += r
	}
	arm.WallSeconds = time.Since(start).Round(10 * time.Millisecond).Seconds()
	var recs []Record
	for _, r := range results {
		if rec, ok := RecordOf(r); ok {
			recs = append(recs, rec)
		}
	}
	return arm, recs, nil
}

// Summarise aggregates an arm's results. It is independent of the order
// the seeds were checked in.
func Summarise(name string, cfg budget.SimConfig, first uint64, results []budget.SimSeedResult) Arm {
	arm := Arm{Name: name, Planted: cfg.Planted, FirstSeed: first, Seeds: len(results)}
	exercised := map[budget.SimFault]int{}
	type key struct{ inv, class string }
	invSeeds := map[string]int{}
	byRule := map[key]*Class{}
	add := func(m map[key]*Class, k key, minimal []budget.SimStep, o budget.SimOutcome, seed uint64) {
		c := m[k]
		if c == nil {
			c = &Class{Class: k.class}
			m[k] = c
		}
		c.Seeds++
		c.Lengths = addLen(c.Lengths, len(minimal))
		if c.Example == nil || len(minimal) < len(c.Example.Steps) || (len(minimal) == len(c.Example.Steps) && seed < c.Example.Seed) {
			c.Example = example(seed, minimal, o)
		}
	}
	classMap := map[key]*Class{}
	for _, r := range results {
		arm.Steps += r.Steps
		for _, f := range r.Outcome.Exercised {
			exercised[f]++
		}
		o := r.Outcome
		if o.Planted > 0 {
			arm.PlantedFired++
			if o.PlantedLost > 0 {
				arm.PlantedLost++
			}
			if o.Unaccounted() > 0 {
				arm.PlantedFound++
			}
		}
		if !o.Balanced() {
			arm.Unbalanced++
		}
		if over := int64(o.Billed - cfg.Limit); over > 0 {
			arm.OverLimit++
			// over/bound > best.over/best.bound, without division.
			if c := arm.Closest; c == nil || over*c.Bound > c.Overshoot*int64(o.Bound) ||
				(over*c.Bound == c.Overshoot*int64(o.Bound) && r.Seed < c.Seed) {
				arm.Closest = &Overshoot{Seed: r.Seed, Overshoot: over, Bound: int64(o.Bound)}
			}
		}
		if o.Spent > o.Billed {
			arm.OverCounted++
			if o.LostReply {
				arm.OverCountedLost++
			}
		}
		for _, v := range r.Violations {
			invSeeds[v.Invariant]++
			add(byRule, key{v.Invariant, v.Class}, v.Minimal, v.Outcome, r.Seed)
		}
		for _, c := range r.Classes {
			add(classMap, key{"", c.Class}, c.Minimal, c.Outcome, r.Seed)
			if c.Class == budget.ClassUnaccounted && c.Outcome.Planted > 0 {
				arm.UnaccountedPlanted++
			}
		}
	}
	for _, f := range budget.SimFaults {
		arm.Exercised = append(arm.Exercised, Count{Name: string(f), Seeds: exercised[f]})
	}
	classes := []string{budget.ClassKilledPending, budget.ClassExpiredHold, budget.ClassOther, budget.ClassUnaccounted}
	for _, inv := range budget.Invariants {
		v := Violation{Invariant: inv, Seeds: invSeeds[inv]}
		for _, cl := range classes[:3] {
			if c := byRule[key{inv, cl}]; c != nil {
				v.ByClass = append(v.ByClass, *c)
			} else {
				v.ByClass = append(v.ByClass, Class{Class: cl})
			}
		}
		arm.Violations = append(arm.Violations, v)
	}
	for _, cl := range classes {
		if c := classMap[key{"", cl}]; c != nil {
			arm.Classes = append(arm.Classes, *c)
		} else {
			arm.Classes = append(arm.Classes, Class{Class: cl})
		}
	}
	return arm
}

func addLen(l []LenCount, steps int) []LenCount {
	i, found := slices.BinarySearchFunc(l, steps, func(e LenCount, s int) int { return cmp.Compare(e.Steps, s) })
	if found {
		l[i].Seeds++
		return l
	}
	return slices.Insert(l, i, LenCount{Steps: steps, Seeds: 1})
}

func example(seed uint64, minimal []budget.SimStep, o budget.SimOutcome) *Example {
	e := &Example{
		Seed: seed, Trace: o.Trace,
		Billed: int64(o.Billed), Spent: int64(o.Spent), Bound: int64(o.Bound),
		Pending: int64(o.LostPending), RedisHold: int64(o.LostRedisHold), GraceHold: int64(o.LostGraceHold),
		Planted: o.Planted,
	}
	for _, s := range minimal {
		e.Steps = append(e.Steps, s.String())
	}
	return e
}

// WriteRecords writes recs as gzipped JSON lines.
func WriteRecords(path string, recs []Record) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("budget-sim: %w", err)
	}
	zw := gzip.NewWriter(f)
	enc := json.NewEncoder(zw)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			_ = f.Close()
			return fmt.Errorf("budget-sim: write %s: %w", path, err)
		}
	}
	if err := errors.Join(zw.Close(), f.Close()); err != nil {
		return fmt.Errorf("budget-sim: write %s: %w", path, err)
	}
	return nil
}

// ReadRecords reads a file WriteRecords wrote.
func ReadRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("budget-sim: %w", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("budget-sim: read %s: %w", path, err)
	}
	dec := json.NewDecoder(zr)
	var recs []Record
	for {
		var r Record
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			return recs, nil
		} else if err != nil {
			return nil, fmt.Errorf("budget-sim: read %s: %w", path, err)
		}
		recs = append(recs, r)
	}
}

// RecordsFile names an arm's records file.
func RecordsFile(arm string) string { return "seeds-" + arm + ".jsonl.gz" }

// Main is `switchboard eval budget-sim`. args excludes the subcommand
// names. It returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("budget-sim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("seeds", 100000, "seeds to check per arm")
	first := fs.Uint64("first", 0, "first seed")
	out := fs.String("out", filepath.Join("eval/results", Dir), "directory to write")
	workers := fs.Int("workers", runtime.GOMAXPROCS(0), "simulators run in parallel")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || *n < 1 || *workers < 1 {
		_, _ = fmt.Fprintln(stderr, "usage: switchboard eval budget-sim [-seeds N] [-first S] [-workers W] [-out DIR]")
		return 2
	}
	if err := run(ctx, *n, *first, *workers, *out, stdout, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func run(ctx context.Context, n int, first uint64, workers int, out string, stdout, stderr io.Writer) error {
	sum := Summary{
		Meta: Meta{
			Commit:   gitCommit(ctx),
			Date:     time.Now().UTC().Format(time.RFC3339),
			Command:  fmt.Sprintf("switchboard eval budget-sim -seeds %d -first %d", n, first),
			Workers:  workers,
			Hardware: bench.DetectHardware(),
		},
		Config: ConfigOf(budget.DefaultSimConfig()),
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("budget-sim: %w", err)
	}
	for _, name := range []string{ArmMain, ArmPlanted} {
		cfg := budget.DefaultSimConfig()
		cfg.Planted = name == ArmPlanted
		arm, recs, err := RunArm(ctx, name, cfg, first, n, workers, stderr)
		if err != nil {
			return err
		}
		if err := WriteRecords(filepath.Join(out, RecordsFile(name)), recs); err != nil {
			return err
		}
		sum.Arms = append(sum.Arms, arm)
		_, _ = fmt.Fprintf(stdout, "%s: %d seeds, %d runs, %.0f s\n", name, arm.Seeds, arm.Runs, arm.WallSeconds)
	}
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return fmt.Errorf("budget-sim: %w", err)
	}
	if err := os.WriteFile(filepath.Join(out, "summary.json"), append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("budget-sim: %w", err)
	}
	return nil
}

func gitCommit(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	c := strings.TrimSpace(string(out))
	if err := exec.CommandContext(ctx, "git", "diff", "--quiet", "HEAD", "--", ".", ":(exclude)eval/results").Run(); err != nil {
		c += "-dirty"
	}
	return c
}

package results

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/budgetsim"
	"github.com/TheAsianFish/switchboard/internal/chaos"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// M12SimDir holds M12's `make sim-budget` run, on the ledger that charges
// expired holds.
const M12SimDir = budgetsim.Dir

// M12Sim is M12's re-run of M11's simulator, set against M11's run.
type M12Sim struct {
	Summary  budgetsim.Summary
	Hardware string
	Main     *M11Arm
	Planted  *M11Arm
	// M11Main is M11's main arm, for the before column; nil without M11.
	M11Main *M11Arm
	// I4Closest describes the seed whose over-count came nearest I4's
	// bound.
	I4Closest string
	// ExpiredOnly counts the main arm's seeds whose only loss is a dead
	// request's cost above its expired hold.
	ExpiredOnly int
	// Checks are PLAN.md's M12 checks on the simulator.
	Checks []M11Check
	// Classes are the main arm's fault-log classes beside M11's seeds.
	Classes []M12ClassRow
}

// M12ClassRow is a fault-log class with M11's count beside it.
type M12ClassRow struct {
	M11ClassRow
	M11Seeds string
}

// M12NamedArm is an orphan arm with its name, for ranging.
type M12NamedArm struct {
	Name string
	*M12Arm
}

func loadM12Sim(dir string, m11 *M11) (*M12Sim, error) {
	var s budgetsim.Summary
	err := readJSON(filepath.Join(dir, "summary.json"), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m := &M12Sim{Summary: s, Hardware: describeHardware(s.Meta.Hardware)}
	for i := range s.Arms {
		a := s.Arms[i]
		if len(a.Violations) != len(budget.Invariants) {
			return nil, fmt.Errorf("results: m12: arm %q has %d invariants, want %d", a.Name, len(a.Violations), len(budget.Invariants))
		}
		for j, v := range a.Violations {
			if v.Invariant != budget.Invariants[j] {
				return nil, fmt.Errorf("results: m12: arm %q: invariant %d is %q, want %q", a.Name, j, v.Invariant, budget.Invariants[j])
			}
		}
		x := buildM11Arm(a)
		recs, err := budgetsim.ReadRecords(filepath.Join(dir, budgetsim.RecordsFile(a.Name)))
		if err != nil {
			return nil, fmt.Errorf("results: m12: %w", err)
		}
		lost, unclassified := 0, 0
		for _, r := range recs {
			if r.Billed > r.Spent {
				lost++
				if len(r.Classes) == 0 {
					unclassified++
				}
				if a.Name == budgetsim.ArmMain && len(r.Classes) == 1 && r.Classes[0].Class == budget.ClassExpiredHold {
					m.ExpiredOnly++
				}
			}
		}
		if a.Violations[0].Seeds != lost {
			return nil, fmt.Errorf("results: m12: arm %q: summary.json has %d seeds that lost spend, its records %d", a.Name, a.Violations[0].Seeds, lost)
		}
		x.setUnclassified(unclassified)
		switch a.Name {
		case budgetsim.ArmMain:
			m.Main = x
		case budgetsim.ArmPlanted:
			m.Planted = x
		}
	}
	if m.Main == nil || m.Planted == nil {
		return nil, errors.New("results: m12: summary.json needs a main and a planted arm")
	}
	if m11 != nil {
		m.M11Main = m11.Main
	}
	m.I4Closest = "–"
	if c := m.Main.I4Closest; c != nil {
		m.I4Closest = fmt.Sprintf("%d over billed against a bound of %d (seed %d)", c.Overshoot, c.Bound, c.Seed)
	}
	m.Checks = m12SimChecks(m)
	for _, row := range m.Main.Classes {
		r := M12ClassRow{M11ClassRow: row, M11Seeds: "–"}
		if m.M11Main != nil {
			r.M11Seeds = seedCount(m.M11Main.class(row.Class).Seeds)
		}
		m.Classes = append(m.Classes, r)
	}
	return m, nil
}

func m12SimChecks(m *M12Sim) []M11Check {
	main, planted := m.Main, m.Planted
	eh, kp := main.class(budget.ClassExpiredHold), main.class(budget.ClassKilledPending)
	before := "M11's run not found"
	if m.M11Main != nil {
		before = fmt.Sprintf("M11: %s", seedCount(m.M11Main.class(budget.ClassExpiredHold).Seeds))
	}
	var c []M11Check
	c = append(c, M11Check{
		"The \"expired hold\" class is gone",
		fmt.Sprintf("%s seeds still lose spend to an expired hold (%s); that part of each loss is what a dead request cost above its hold, and %s of those seeds lose nothing else", seedCount(eh.Seeds), before, seedCount(m.ExpiredOnly)),
		yesNo(eh.Seeds == 0),
	})
	c = append(c, M11Check{
		"\"Killed pending\" is still found",
		fmt.Sprintf("%s seeds", seedCount(kp.Seeds)),
		yesNo(kp.Seeds > 0),
	})
	i2, i3, i4 := main.Violations[1], main.Violations[2], main.Violations[3]
	c = append(c, M11Check{
		"I4 holds on every seed: without a lost reply, spent − billed ≤ Σ(held − cost) over expired holds",
		fmt.Sprintf("%s violations; %s seeds charged an expired hold; closest: %s", seedCount(i4.Seeds), seedCount(main.ExpiredCharged), m.I4Closest),
		yesNo(i4.Seeds == 0),
	})
	c = append(c, M11Check{
		"I2 and I3 still hold on every seed",
		fmt.Sprintf("I2: %s violations, %s seeds over the limit, closest %s; I3: %s violations", seedCount(i2.Seeds), main.OverLimit, main.Closest, seedCount(i3.Seeds)),
		yesNo(i2.Seeds+i3.Seeds == 0),
	})
	c = append(c, M11Check{
		"The fault log still accounts for every seed's money",
		fmt.Sprintf("unbalanced seeds: %s in the main arm, %s in the planted arm", main.Unbalanced, planted.Unbalanced),
		yesNo(main.Arm.Unbalanced == 0 && planted.Arm.Unbalanced == 0),
	})
	pu := planted.class(budget.ClassUnaccounted)
	c = append(c, M11Check{
		"The planted bug is still found",
		fmt.Sprintf("fired on %s seeds, kept spend out of the store on %s, unaccounted on %s; %s minimal schedules unaccounted, none in the main arm: %s",
			seedCount(planted.PlantedFired), seedCount(planted.PlantedLost), seedCount(planted.PlantedFound), seedCount(pu.Seeds), seedCount(main.class(budget.ClassUnaccounted).Seeds)),
		yesNo(planted.PlantedFound > 0 && planted.PlantedFound == planted.PlantedLost && main.class(budget.ClassUnaccounted).Seeds == 0),
	})
	return c
}

// M12OrphanDir holds the `make chaos-orphan` arms and the flap re-run.
const M12OrphanDir = "m12-orphan"

// M12 is the expired-hold milestone: the orphan run's two arms, the flap
// schedule re-run on the fixed ledger, and the simulator re-run.
type M12 struct {
	Before, After *M12Arm
	// Arms is Before and After, those that ran, for ranging.
	Arms []M12NamedArm
	// Flap is the M8 flap schedule re-run on the fixed ledger, set
	// against M8's committed runs; nil if it has not run.
	Flap *M12Flap
	Sim  *M12Sim
	// Checks are the hypotheses and checks, in PLAN.md's order.
	Checks []M11Check
	// Headline is the README's line.
	Headline string
}

// M12Arm is one orphan arm, formatted.
type M12Arm struct {
	S                                   *chaos.OrphanSummary
	Commit, GatewayCommit, Date         string
	Requests, ByReplica, Client         string
	Kill, Died, ExpiredBy               string
	InFlight, OutputTokens              string
	Billed, Held, HeldMinusBilled, Gap  string
	HeldOverBilled, OverCountOverBilled string
	Timeline                            []M12Point
	HeldCheck                           string
}

// M12Point is one snapshot row.
type M12Point struct {
	Label, At, Billed, RawSpent, RawReserved, Spent, Reserved, Gap string
}

// M12Flap is the flap re-run against M8's committed arms.
type M12Flap struct {
	Rerun           *chaos.Summary
	MinGap, M8Gap   string
	Overshoot, M8Ov string
	Commit, Date    string
	Held            bool
}

func loadM12(resultsDir string, m11 *M11) (*M12, error) {
	dir := filepath.Join(resultsDir, M12OrphanDir)
	m := &M12{}
	var err error
	if m.Before, err = loadM12Arm(filepath.Join(dir, "before")); err != nil {
		return nil, err
	}
	if m.After, err = loadM12Arm(filepath.Join(dir, "after")); err != nil {
		return nil, err
	}
	if m.Flap, err = loadM12Flap(filepath.Join(dir, "flap-rerun"), filepath.Join(resultsDir, M8Dir)); err != nil {
		return nil, err
	}
	if m.Sim, err = loadM12Sim(filepath.Join(resultsDir, M12SimDir), m11); err != nil {
		return nil, err
	}
	for _, a := range []M12NamedArm{{"before", m.Before}, {"after", m.After}} {
		if a.M12Arm != nil {
			m.Arms = append(m.Arms, a)
		}
	}
	if m.Before == nil && m.After == nil && m.Flap == nil && m.Sim == nil {
		return nil, nil
	}
	m.Checks = m12Checks(m)
	if m.Before != nil && m.After != nil && m.Sim != nil {
		eh := m.Sim.Main.class(budget.ClassExpiredHold).Seeds
		was := "–"
		if m.Sim.M11Main != nil {
			was = seedCount(m.Sim.M11Main.class(budget.ClassExpiredHold).Seeds)
		}
		m.Headline = fmt.Sprintf("Streams cut off by a replica kill with Redis up: before the fix, %s of them left %s out of spent after the hold TTL (their bill: %s); after it, %s of them left spent %s above the bill, their holds' margin (%s× what they were billed). Simulator re-run: seeds losing spend to an expired hold %s → %s, the remaining loss what a dead request cost above its hold; %s",
			m.Before.InFlight, m.Before.Gap, m.Before.Billed, m.After.InFlight, neg(m.After.S.AfterTTL), m.After.OverCountOverBilled, was, seedCount(eh), m12SimVerdict(m.Sim))
	}
	return m, nil
}

// neg formats −gap of p, the over-count.
func neg(p *chaos.OrphanPoint) string {
	if p == nil {
		return "–"
	}
	return q6USD6(-p.Gap)
}

// q6USD6 formats n in dollars to six places, a minus sign before the $.
func q6USD6(n cost.Nanos) string {
	if n < 0 {
		return "−" + q6USD6(-n)
	}
	return fmt.Sprintf("$%.6f", n.USD())
}

func loadM12Arm(dir string) (*M12Arm, error) {
	var s chaos.OrphanSummary
	err := readJSON(filepath.Join(dir, chaos.SummaryFile), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := s.InFlight
	a := &M12Arm{
		S: &s, Commit: s.Meta.Commit, GatewayCommit: s.Meta.GatewayCommit, Date: s.Meta.Date,
		Requests: seedCount(s.Requests), Kill: sec1(s.KillSeconds), Died: sec1(s.DiedSeconds), ExpiredBy: sec1(s.ExpiredBySeconds),
		InFlight: seedCount(f.Count), OutputTokens: seedCount(f.OutputTokens),
		Billed: q6USD6(f.Billed), Held: q6USD6(f.Held), HeldMinusBilled: q6USD6(f.HeldMinusBilled),
		HeldOverBilled:      strconv.FormatFloat(f.HeldOverBilled, 'f', 2, 64),
		OverCountOverBilled: strconv.FormatFloat(s.After.OverCountOverBilled, 'f', 2, 64),
		Gap:                 "–",
	}
	var parts []string
	for _, r := range sortedKeysInt(s.ByReplica) {
		parts = append(parts, fmt.Sprintf("%s %d", r, s.ByReplica[r]))
	}
	a.ByReplica = strings.Join(parts, ", ")
	parts = nil
	for _, r := range sortedKeysInt(s.Client) {
		parts = append(parts, fmt.Sprintf("%s %d", r, s.Client[r]))
	}
	a.Client = strings.Join(parts, ", ")
	if s.AfterTTL != nil {
		a.Gap = q6USD6(s.AfterTTL.Gap)
	}
	if h := s.HeldCheck; h != nil {
		a.HeldCheck = fmt.Sprintf("%s at %s s against %s recomputed (%s)", q6USD6(h.Reserved), sec1(h.AtSeconds), q6USD6(h.Held), map[bool]string{true: "equal", false: "not equal"}[h.Equal])
	}
	opt := func(n *cost.Nanos) string {
		if n == nil {
			return "–"
		}
		return q6USD6(*n)
	}
	for _, p := range s.Timeline {
		a.Timeline = append(a.Timeline, M12Point{
			Label: p.Label, At: sec1(p.AtSeconds), Billed: q6USD6(p.Billed),
			RawSpent: q6USD6(p.RawSpent), RawReserved: q6USD6(p.RawReserved),
			Spent: opt(p.Spent), Reserved: opt(p.Reserved), Gap: q6USD6(p.Gap),
		})
	}
	return a, nil
}

func sec1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func sortedKeysInt(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func loadM12Flap(dir, m8dir string) (*M12Flap, error) {
	var s chaos.Summary
	err := readJSON(filepath.Join(dir, chaos.SummaryFile), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.Flap == nil {
		return nil, errors.New("results: m12: the flap re-run has no flap result")
	}
	var m8 chaos.Summary
	if err := readJSON(filepath.Join(m8dir, "after", chaos.SummaryFile), &m8); err != nil {
		return nil, fmt.Errorf("results: m12: M8's committed run: %w", err)
	}
	if m8.Flap == nil {
		return nil, errors.New("results: m12: M8's committed run has no flap result")
	}
	return &M12Flap{
		Rerun: &s, MinGap: m8USD(s.Flap.MinGap), M8Gap: m8USD(m8.Flap.MinGap),
		Overshoot: q6USD(s.Flap.Overshoot), M8Ov: q6USD(m8.Flap.Overshoot),
		Commit: s.Meta.Commit, Date: s.Meta.Date,
		Held: s.Flap.MinGap >= m8.Flap.MinGap,
	}, nil
}

func m12Checks(m *M12) []M11Check {
	var c []M11Check
	if a := m.Before; a != nil {
		h := a.S.Before
		c = append(c, M11Check{
			"Before: after the TTL the gap is positive and equals what the provider billed for the requests in flight on the killed replica, within one request",
			fmt.Sprintf("gap %s; %s requests in flight billed %s; difference %s, tolerance %s", q6USD6(h.Gap), a.InFlight, q6USD6(h.Billed), q6USD6(h.Diff), q6USD6(h.Tolerance)),
			yesNo(h.Holds),
		})
	}
	if a := m.After; a != nil {
		h := a.S.After
		c = append(c, M11Check{
			"After: the gap after the TTL is at most 0, and the over-count at most held − billed over the requests in flight on the killed replica",
			fmt.Sprintf("gap %s; over-count %s against held − billed %s (%s requests); over-count %s× what they were billed, the bound %s×",
				q6USD6(h.Gap), q6USD6(h.OverCount), q6USD6(h.Bound), a.InFlight, a.OverCountOverBilled, strconv.FormatFloat(h.BoundOverBilled, 'f', 2, 64)),
			yesNo(h.Holds),
		})
	}
	if f := m.Flap; f != nil {
		c = append(c, M11Check{
			"The flap schedule re-run shows no reconciliation gap more negative than M8's committed one",
			fmt.Sprintf("most negative gap after the first outage %s against M8's %s; overshoot %s against M8's %s", f.MinGap, f.M8Gap, f.Overshoot, f.M8Ov),
			yesNo(f.Held),
		})
	}
	if m.Sim != nil {
		c = append(c, m.Sim.Checks...)
	}
	return c
}

// m12SimVerdict says whether I4 and the exact balance held, from the run.
func m12SimVerdict(m *M12Sim) string {
	i4 := m.Main.Violations[3].Seeds
	unbalanced := m.Main.Arm.Unbalanced + m.Planted.Arm.Unbalanced
	if i4 == 0 && unbalanced == 0 {
		return "I4 and the exact balance held on every seed"
	}
	return fmt.Sprintf("I4 failed on %s seeds and %s seeds did not balance", seedCount(i4), seedCount(unbalanced))
}

package results

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/budgetsim"
)

// M11Dir holds M11's `make sim-budget` summary and per-seed records, run
// on the ledger before expired holds were charged (M12 writes its own).
const M11Dir = "m11-budget-sim"

// m11Invariants are the invariants M11's run checked; M12 added I4.
var m11Invariants = []string{"I1", "I2", "I3"}

// M11 is the fault-schedule simulator section.
type M11 struct {
	Summary  budgetsim.Summary
	Hardware string
	// Config, formatted for prose.
	Weights string
	Holds   string
	Main    *M11Arm
	Planted *M11Arm
	// Checks are PLAN.md's hypotheses and null-result checks against the
	// run, in PLAN.md's order.
	Checks []M11Check
	// Headline is the README's one line.
	Headline string
	// RuleSlipped is true when PLAN.md's rule filed every lost-spend seed
	// of the main arm as "expired hold", the slippage the section explains.
	RuleSlipped bool
	// LongShrinks is true when a listed class's minimal schedule is over
	// four steps, which the section explains.
	LongShrinks bool
}

// M11Arm is one arm, formatted.
type M11Arm struct {
	budgetsim.Arm
	Seeds, Steps, Runs, Wall string
	Exercised                []M11Share
	// Rule is one row per invariant: seeds and PLAN.md's rule's classes.
	Rule []M11RuleRow
	// Classes is one row per class read from the fault log.
	Classes []M11ClassRow
	// I1 is the number of seeds that lost spend.
	I1 string
	// Unclassified is the number of those seeds no class is needed for
	// on its own (read from the records file), and its share of I1.
	Unclassified, UnclassifiedShare string
	// RuleExamples are the rule's minimal schedules, one per invariant and
	// class seen; Examples the fault-log classes' shortest.
	RuleExamples []M11Example
	Examples     []M11Example
	OverLimit    string
	Closest      string
	OverCounted  string
	// Fired, PlantedLostSeeds and Found are PlantedFired, PlantedLost and
	// PlantedFound, OverCountedLostReply OverCountedLost, and Unbalanced
	// the seeds the fault log does not balance, formatted.
	Fired, PlantedLostSeeds, Found, OverCountedLostReply, Unbalanced string
}

// Example returns the fault-log class's minimal schedule, or nil.
func (a *M11Arm) Example(class string) *M11Example {
	for i := range a.Examples {
		if a.Examples[i].Title == class {
			return &a.Examples[i]
		}
	}
	return nil
}

// M11Share is a fault and the share of seeds that exercised it.
type M11Share struct {
	Name, Seeds, Share string
	// AtLeast10 is PLAN.md's floor.
	AtLeast10 bool
}

// M11RuleRow is one invariant under PLAN.md's rule.
type M11RuleRow struct {
	Invariant, Seeds                        string
	KilledPending, ExpiredHold, Other, Long string
}

// M11ClassRow is one class from the fault log.
type M11ClassRow struct {
	Class, Seeds, ShareOfI1, Lengths, Longest string
}

// M11Example is one minimal schedule.
type M11Example struct {
	Title string
	Seed  string
	Steps []string
	// Line is Steps joined with "; ".
	Line  string
	Trace []string
	// Sums is billed, spent and what the kills lost, for prose.
	Sums string
}

// M11Check is one hypothesis or check.
type M11Check struct {
	Hypothesis, Measured, Held string
}

func loadM11(dir string) (*M11, error) {
	var s budgetsim.Summary
	err := readJSON(filepath.Join(dir, "summary.json"), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	unclassified := map[string]int{}
	for _, a := range s.Arms {
		recs, err := budgetsim.ReadRecords(filepath.Join(dir, budgetsim.RecordsFile(a.Name)))
		if err != nil {
			return nil, fmt.Errorf("results: m11: %w", err)
		}
		lost := 0
		for _, r := range recs {
			if r.Billed > r.Spent {
				lost++
				if len(r.Classes) == 0 {
					unclassified[a.Name]++
				}
			}
		}
		if len(a.Violations) > 0 && a.Violations[0].Seeds != lost {
			return nil, fmt.Errorf("results: m11: arm %q: summary.json has %d seeds that lost spend, its records %d", a.Name, a.Violations[0].Seeds, lost)
		}
	}
	m, err := buildM11(s)
	if err != nil {
		return nil, err
	}
	m.Main.setUnclassified(unclassified[budgetsim.ArmMain])
	m.Planted.setUnclassified(unclassified[budgetsim.ArmPlanted])
	return m, nil
}

// setUnclassified records how many seeds lost spend without needing any
// one class: the shortfall fits inside more than one way's loss, so no
// single way is needed on its own.
func (a *M11Arm) setUnclassified(n int) {
	a.Unclassified = seedCount(n)
	a.UnclassifiedShare = share(n, a.Violations[0].Seeds)
}

func buildM11(s budgetsim.Summary) (*M11, error) {
	m := &M11{Summary: s, Hardware: describeHardware(s.Meta.Hardware)}
	var w []string
	for _, x := range s.Config.Weights {
		w = append(w, fmt.Sprintf("%s %d", x.Op, x.Weight))
	}
	m.Weights = strings.Join(w, ", ")
	var h []string
	for _, x := range s.Config.Holds {
		h = append(h, strconv.FormatInt(x, 10))
	}
	m.Holds = strings.Join(h, ", ")
	for i := range s.Arms {
		if len(s.Arms[i].Violations) != len(m11Invariants) || len(s.Arms[i].Exercised) == 0 {
			return nil, fmt.Errorf("results: m11: arm %q needs %d invariants and its faults", s.Arms[i].Name, len(m11Invariants))
		}
		for j, v := range s.Arms[i].Violations {
			if v.Invariant != m11Invariants[j] {
				return nil, fmt.Errorf("results: m11: arm %q: invariant %d is %q, want %q", s.Arms[i].Name, j, v.Invariant, m11Invariants[j])
			}
		}
		a := buildM11Arm(s.Arms[i])
		switch a.Name {
		case budgetsim.ArmMain:
			m.Main = a
		case budgetsim.ArmPlanted:
			m.Planted = a
		}
	}
	if m.Main == nil || m.Planted == nil {
		return nil, errors.New("results: m11: summary.json needs a main and a planted arm")
	}
	m.Checks = m11Checks(m.Main, m.Planted)
	i1 := m.Main.Violations[0]
	m.LongShrinks = over4(m.Main.class(budget.ClassKilledPending))+over4(m.Main.class(budget.ClassExpiredHold)) > 0
	m.RuleSlipped = i1.Seeds > 0 && m.Main.rule("I1", budget.ClassExpiredHold).Seeds == i1.Seeds
	mc := m.Main.class(budget.ClassOther)
	third := "no lost spend needed a third way"
	if mc.Seeds > 0 {
		third = fmt.Sprintf("%s needed a third way (a request admitted on grace and in flight when its process died), shortest %s", seedCount(mc.Seeds), steps(shortest(mc)))
	}
	held := "I2 and I3 held on every seed"
	if i2, i3 := m.Main.Violations[1].Seeds, m.Main.Violations[2].Seeds; i2+i3 > 0 {
		held = fmt.Sprintf("I2 failed on %s seeds and I3 on %s", seedCount(i2), seedCount(i3))
	}
	m.Headline = fmt.Sprintf("%s seeded fault schedules on three replicas: %s lost spend; by the fault log, %s; %s; a planted bug was found in %s of the %s seeds it hit",
		m.Main.Seeds, m.Main.I1, third, held, seedCount(m.Planted.PlantedFound), seedCount(m.Planted.PlantedFired))
	return m, nil
}

func buildM11Arm(a budgetsim.Arm) *M11Arm {
	x := &M11Arm{Arm: a, Seeds: seedCount(a.Seeds), Steps: seedCount(a.Steps), Runs: seedCount(a.Runs)}
	x.Wall = strconv.FormatFloat(a.WallSeconds/60, 'f', 1, 64) + " min"
	for _, e := range a.Exercised {
		x.Exercised = append(x.Exercised, M11Share{Name: e.Name, Seeds: seedCount(e.Seeds), Share: share(e.Seeds, a.Seeds), AtLeast10: e.Seeds*10 >= a.Seeds})
	}
	for _, v := range a.Violations {
		row := M11RuleRow{Invariant: v.Invariant, Seeds: seedCount(v.Seeds)}
		longest := 0
		for _, c := range v.ByClass {
			cell := seedCount(c.Seeds)
			switch c.Class {
			case budget.ClassKilledPending:
				row.KilledPending = cell
			case budget.ClassExpiredHold:
				row.ExpiredHold = cell
			case budget.ClassOther:
				row.Other = cell
			}
			if c.Seeds > 0 {
				longest = max(longest, longestOf(c))
				x.RuleExamples = append(x.RuleExamples, m11Example(fmt.Sprintf("%s, %s", v.Invariant, c.Class), c.Example))
			}
		}
		row.Long = "–"
		if longest > 0 {
			row.Long = strconv.Itoa(longest)
		}
		x.Rule = append(x.Rule, row)
		if v.Invariant == "I1" {
			x.I1 = seedCount(v.Seeds)
		}
	}
	i1 := a.Violations[0].Seeds
	for _, c := range a.Classes {
		row := M11ClassRow{Class: c.Class, Seeds: seedCount(c.Seeds), ShareOfI1: share(c.Seeds, i1), Lengths: lengths(c.Lengths), Longest: "–"}
		if c.Seeds > 0 {
			row.Longest = strconv.Itoa(longestOf(c))
			x.Examples = append(x.Examples, m11Example(c.Class, c.Example))
		}
		x.Classes = append(x.Classes, row)
	}
	x.OverLimit = seedCount(a.OverLimit)
	x.Closest = "–"
	if c := a.Closest; c != nil {
		x.Closest = fmt.Sprintf("%d over the limit against a bound of %d (seed %d)", c.Overshoot, c.Bound, c.Seed)
	}
	x.OverCounted = seedCount(a.OverCounted)
	x.Fired, x.PlantedLostSeeds, x.Found = seedCount(a.PlantedFired), seedCount(a.PlantedLost), seedCount(a.PlantedFound)
	x.Unbalanced = seedCount(a.Unbalanced)
	x.OverCountedLostReply = seedCount(a.OverCountedLost)
	return x
}

func (a *M11Arm) class(name string) budgetsim.Class {
	for _, c := range a.Arm.Classes {
		if c.Class == name {
			return c
		}
	}
	return budgetsim.Class{Class: name}
}

func (a *M11Arm) rule(inv, class string) budgetsim.Class {
	for _, v := range a.Violations {
		if v.Invariant != inv {
			continue
		}
		for _, c := range v.ByClass {
			if c.Class == class {
				return c
			}
		}
	}
	return budgetsim.Class{Class: class}
}

func m11Example(title string, e *budgetsim.Example) M11Example {
	if e == nil {
		return M11Example{Title: title}
	}
	return M11Example{
		Title: title, Seed: strconv.FormatUint(e.Seed, 10), Steps: e.Steps, Line: strings.Join(e.Steps, "; "), Trace: e.Trace,
		Sums: fmt.Sprintf("billed %d, spent %d; killed processes held %d pending, %d in Redis holds, %d in grace holds",
			e.Billed, e.Spent, e.Pending, e.RedisHold, e.GraceHold),
	}
}

func m11Checks(main, planted *M11Arm) []M11Check {
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	var c []M11Check
	floor := true
	var low []string
	least := main.Arm.Exercised[0]
	for _, e := range main.Arm.Exercised {
		if e.Seeds*10 < main.Arm.Seeds {
			floor = false
			low = append(low, e.Name)
		}
		if e.Seeds < least.Seeds {
			least = e
		}
	}
	measured := fmt.Sprintf("least exercised: %s, %s of seeds", least.Name, share(least.Seeds, main.Arm.Seeds))
	if len(low) > 0 {
		measured += "; under 10%: " + strings.Join(low, ", ")
	}
	c = append(c, M11Check{"Every fault exercised by at least 10% of seeds", measured, yes(floor)})

	kp, eh, ot := main.rule("I1", budget.ClassKilledPending), main.rule("I1", budget.ClassExpiredHold), main.rule("I1", budget.ClassOther)
	i1 := main.Violations[0].Seeds
	c = append(c, M11Check{
		"PLAN.md's rule rediscovers both listed classes",
		fmt.Sprintf("killed pending %s, expired hold %s of %s seeds that lost spend", seedCount(kp.Seeds), seedCount(eh.Seeds), seedCount(i1)),
		yes(kp.Seeds > 0 && eh.Seeds > 0),
	})
	c = append(c, M11Check{
		"Under PLAN.md's rule, every I1 failure is killed pending or expired hold",
		fmt.Sprintf("other: %s", seedCount(ot.Seeds)),
		yes(ot.Seeds == 0),
	})
	fk, fe, fo := main.class(budget.ClassKilledPending), main.class(budget.ClassExpiredHold), main.class(budget.ClassOther)
	c = append(c, M11Check{
		"By the fault log, both listed classes are rediscovered",
		fmt.Sprintf("killed pending %s, expired hold %s", seedCount(fk.Seeds), seedCount(fe.Seeds)),
		yes(fk.Seeds > 0 && fe.Seeds > 0),
	})
	c = append(c, M11Check{
		"By the fault log, no lost spend needs a third way",
		fmt.Sprintf("other: %s of %s (%s)", seedCount(fo.Seeds), seedCount(i1), share(fo.Seeds, i1)),
		yes(fo.Seeds == 0),
	})
	worst := 0
	var parts []string
	for _, cl := range []budgetsim.Class{fk, fe} {
		if cl.Seeds == 0 {
			continue
		}
		l := longestOf(cl)
		worst = max(worst, l)
		parts = append(parts, fmt.Sprintf("%s: %d (%s of %s over four)", cl.Class, l, seedCount(over4(cl)), seedCount(cl.Seeds)))
	}
	c = append(c, M11Check{"Each listed class shrinks to four steps or fewer, by the fault log", "longest " + strings.Join(parts, "; "), yes(worst > 0 && worst <= 4)})
	c = append(c, M11Check{
		"I2 is pushed: billed passes the limit on at least 10% of seeds",
		fmt.Sprintf("%s of %s (%s)", main.OverLimit, main.Seeds, share(main.Arm.OverLimit, main.Arm.Seeds)),
		yes(main.Arm.OverLimit*10 >= main.Arm.Seeds),
	})
	i2, i3 := main.Violations[1], main.Violations[2]
	c = append(c, M11Check{
		"I2 holds on every seed",
		fmt.Sprintf("%s violations; %s seeds over the limit, closest: %s", seedCount(i2.Seeds), main.OverLimit, main.Closest),
		yes(i2.Seeds == 0),
	})
	c = append(c, M11Check{
		"I3 holds on every seed",
		fmt.Sprintf("%s violations; %s seeds ended with spent above billed, %s of them after a lost reply", seedCount(i3.Seeds), main.OverCounted, seedCount(main.OverCountedLost)),
		yes(i3.Seeds == 0),
	})
	pu := planted.class(budget.ClassUnaccounted)
	po := planted.rule("I1", budget.ClassOther)
	c = append(c, M11Check{
		"The planted bug is found outside both listed classes",
		fmt.Sprintf("fired on %s seeds, kept spend out of the store on %s; their loss unaccounted by any kill on %s; %s minimal schedules unaccounted, %s with the bug firing (shortest %s); PLAN.md's rule filed %s planted-arm I1 seeds as other against %s in the main arm",
			seedCount(planted.PlantedFired), seedCount(planted.PlantedLost), seedCount(planted.PlantedFound), seedCount(pu.Seeds), seedCount(planted.UnaccountedPlanted), steps(shortest(pu)), seedCount(po.Seeds), seedCount(ot.Seeds)),
		yes(planted.PlantedFound > 0 && planted.UnaccountedPlanted > 0 && main.class(budget.ClassUnaccounted).Seeds == 0),
	})
	c = append(c, M11Check{
		"The fault log accounts for every seed's money (spent = billed − the kills' and the planted bug's losses + double charges)",
		fmt.Sprintf("unbalanced seeds: %s in the main arm, %s in the planted arm", main.Unbalanced, planted.Unbalanced),
		yes(main.Arm.Unbalanced == 0 && planted.Arm.Unbalanced == 0),
	})
	return c
}

func longestOf(c budgetsim.Class) int {
	if len(c.Lengths) == 0 {
		return 0
	}
	return c.Lengths[len(c.Lengths)-1].Steps
}

func shortest(c budgetsim.Class) int {
	if len(c.Lengths) == 0 {
		return 0
	}
	return c.Lengths[0].Steps
}

func over4(c budgetsim.Class) int {
	n := 0
	for _, l := range c.Lengths {
		if l.Steps > 4 {
			n += l.Seeds
		}
	}
	return n
}

func lengths(l []budgetsim.LenCount) string {
	if len(l) == 0 {
		return "–"
	}
	var parts []string
	for _, x := range l {
		parts = append(parts, fmt.Sprintf("%d: %s", x.Steps, seedCount(x.Seeds)))
	}
	return strings.Join(parts, ", ")
}

func share(n, of int) string {
	if of == 0 {
		return "–"
	}
	return strconv.FormatFloat(100*float64(n)/float64(of), 'f', 1, 64) + "%"
}

// seedCount formats n with thousands separators, e.g. 100,000.
func seedCount(n int) string { return thousands(strconv.Itoa(n)) }

func steps(n int) string {
	if n == 1 {
		return "1 step"
	}
	return strconv.Itoa(n) + " steps"
}

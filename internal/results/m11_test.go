package results

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/budgetsim"
)

func m11Violations(i1 budgetsim.Violation) []budgetsim.Violation {
	empty := func(inv string) budgetsim.Violation {
		return budgetsim.Violation{Invariant: inv, ByClass: []budgetsim.Class{
			{Class: budget.ClassKilledPending}, {Class: budget.ClassExpiredHold}, {Class: budget.ClassOther},
		}}
	}
	return []budgetsim.Violation{i1, empty("I2"), empty("I3")}
}

// m11Summary is a synthetic summary in which the hypotheses hold.
func m11Summary() budgetsim.Summary {
	main := budgetsim.Arm{
		Name: budgetsim.ArmMain, Seeds: 10000, Steps: 54321, Runs: 99,
		Exercised: []budgetsim.Count{{Name: "kill", Seeds: 5000}, {Name: "partition", Seeds: 1000}},
		Violations: m11Violations(budgetsim.Violation{Invariant: "I1", Seeds: 1239, ByClass: []budgetsim.Class{
			{Class: budget.ClassKilledPending, Seeds: 1234, Lengths: []budgetsim.LenCount{{Steps: 2, Seeds: 1234}}},
			{Class: budget.ClassExpiredHold, Seeds: 5, Lengths: []budgetsim.LenCount{{Steps: 3, Seeds: 5}}},
			{Class: budget.ClassOther},
		}}),
		Classes: []budgetsim.Class{
			{Class: budget.ClassKilledPending, Seeds: 1239, Lengths: []budgetsim.LenCount{{Steps: 2, Seeds: 1234}, {Steps: 3, Seeds: 5}},
				Example: &budgetsim.Example{Seed: 77, Steps: []string{"a", "b"}}},
			{Class: budget.ClassExpiredHold, Seeds: 5, Lengths: []budgetsim.LenCount{{Steps: 4, Seeds: 5}}},
			{Class: budget.ClassOther},
			{Class: budget.ClassUnaccounted},
		},
		OverLimit: 1000,
	}
	planted := budgetsim.Arm{
		Name: budgetsim.ArmPlanted, Planted: true, Seeds: 10000,
		Exercised:    []budgetsim.Count{{Name: "kill", Seeds: 5000}},
		Violations:   m11Violations(budgetsim.Violation{Invariant: "I1", Seeds: 9}),
		Classes:      []budgetsim.Class{{Class: budget.ClassUnaccounted, Seeds: 3, Lengths: []budgetsim.LenCount{{Steps: 4, Seeds: 3}}}},
		PlantedFired: 7, PlantedFound: 3, UnaccountedPlanted: 2,
	}
	return budgetsim.Summary{Arms: []budgetsim.Arm{main, planted}}
}

func m11Build(t *testing.T, mut func(main, planted *budgetsim.Arm)) *M11 {
	t.Helper()
	s := m11Summary()
	if mut != nil {
		mut(&s.Arms[0], &s.Arms[1])
	}
	m, err := buildM11(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildM11Exercised(t *testing.T) {
	t.Parallel()
	m := m11Build(t, func(main, _ *budgetsim.Arm) {
		main.Exercised = append(main.Exercised, budgetsim.Count{Name: "rare", Seeds: 999})
	})
	want := []M11Share{
		{"kill", "5,000", "50.0%", true},
		{"partition", "1,000", "10.0%", true},
		{"rare", "999", "10.0%", false},
	}
	if len(m.Main.Exercised) != len(want) {
		t.Fatalf("exercised = %+v", m.Main.Exercised)
	}
	for i, w := range want {
		if m.Main.Exercised[i] != w {
			t.Errorf("exercised[%d] = %+v, want %+v", i, m.Main.Exercised[i], w)
		}
	}
	c := m.Checks[0]
	if c.Held != "no" || !strings.Contains(c.Measured, "under 10%: rare") {
		t.Errorf("check 0 = %+v", c)
	}
	if h := m11Build(t, nil).Checks[0]; h.Held != "yes" || strings.Contains(h.Measured, "under 10%") {
		t.Errorf("check 0 (all exercised) = %+v", h)
	}
}

func TestBuildM11Rule(t *testing.T) {
	t.Parallel()
	m := m11Build(t, nil)
	want := []M11RuleRow{
		{Invariant: "I1", Seeds: "1,239", KilledPending: "1,234", ExpiredHold: "5", Other: "0", Long: "3"},
		{Invariant: "I2", Seeds: "0", KilledPending: "0", ExpiredHold: "0", Other: "0", Long: "–"},
		{Invariant: "I3", Seeds: "0", KilledPending: "0", ExpiredHold: "0", Other: "0", Long: "–"},
	}
	if len(m.Main.Rule) != len(want) {
		t.Fatalf("rule = %+v", m.Main.Rule)
	}
	for i, w := range want {
		if m.Main.Rule[i] != w {
			t.Errorf("rule[%d] = %+v, want %+v", i, m.Main.Rule[i], w)
		}
	}
	if m.Main.I1 != "1,239" {
		t.Errorf("I1 = %q", m.Main.I1)
	}
}

func TestBuildM11Classes(t *testing.T) {
	t.Parallel()
	m := m11Build(t, func(main, _ *budgetsim.Arm) {
		main.Classes[0].Lengths = []budgetsim.LenCount{{Steps: 2, Seeds: 1234}, {Steps: 3, Seeds: 5}}
	})
	want := []M11ClassRow{
		{Class: budget.ClassKilledPending, Seeds: "1,239", ShareOfI1: "100.0%", Lengths: "2: 1,234, 3: 5", Longest: "3"},
		{Class: budget.ClassExpiredHold, Seeds: "5", ShareOfI1: "0.4%", Lengths: "4: 5", Longest: "4"},
		{Class: budget.ClassOther, Seeds: "0", ShareOfI1: "0.0%", Lengths: "–", Longest: "–"},
		{Class: budget.ClassUnaccounted, Seeds: "0", ShareOfI1: "0.0%", Lengths: "–", Longest: "–"},
	}
	if len(m.Main.Classes) != len(want) {
		t.Fatalf("classes = %+v", m.Main.Classes)
	}
	for i, w := range want {
		if m.Main.Classes[i] != w {
			t.Errorf("classes[%d] = %+v, want %+v", i, m.Main.Classes[i], w)
		}
	}
}

func TestBuildM11RuleSlipped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mut  func(main, _ *budgetsim.Arm)
		want bool
	}{
		{"mixed classes", nil, false},
		{"every seed expired hold", func(main, _ *budgetsim.Arm) {
			bc := main.Violations[0].ByClass
			bc[0].Seeds = 0
			bc[1].Seeds = 1239
			bc[1].Lengths = []budgetsim.LenCount{{Steps: 3, Seeds: 1239}}
		}, true},
		{"no I1 seeds", func(main, _ *budgetsim.Arm) {
			main.Violations[0] = m11Violations(budgetsim.Violation{Invariant: "I1"})[0]
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := m11Build(t, tt.mut).RuleSlipped; got != tt.want {
				t.Errorf("RuleSlipped = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildM11Checks(t *testing.T) {
	t.Parallel()
	const (
		third   = 4
		shrink  = 5
		pushed  = 6
		planted = 9
	)
	tests := []struct {
		name string
		mut  func(main, planted *budgetsim.Arm)
		row  int
		hyp  string
		want string
	}{
		{"baseline third way", nil, third, "By the fault log, no lost spend needs a third way", "yes"},
		{"other seeds", func(main, _ *budgetsim.Arm) { main.Classes[2].Seeds = 2 }, third, "By the fault log, no lost spend needs a third way", "no"},
		{"baseline shrink", nil, shrink, "Each listed class shrinks to four steps or fewer", "yes"},
		{"killed pending length 5", func(main, _ *budgetsim.Arm) {
			main.Classes[0].Lengths = []budgetsim.LenCount{{Steps: 2, Seeds: 1234}, {Steps: 5, Seeds: 5}}
		}, shrink, "Each listed class shrinks to four steps or fewer", "no"},
		{"baseline pushed", nil, pushed, "I2 is pushed: billed passes the limit on at least 10% of seeds", "yes"},
		{"over limit under 10%", func(main, _ *budgetsim.Arm) { main.OverLimit = 999 }, pushed, "I2 is pushed: billed passes the limit on at least 10% of seeds", "no"},
		{"baseline planted", nil, planted, "The planted bug is found outside both listed classes", "yes"},
		{"planted not found", func(_, p *budgetsim.Arm) { p.PlantedFound = 0 }, planted, "The planted bug is found outside both listed classes", "no"},
		{"planted unaccounted none", func(_, p *budgetsim.Arm) { p.UnaccountedPlanted = 0 }, planted, "The planted bug is found outside both listed classes", "no"},
		{"main has unaccounted", func(main, _ *budgetsim.Arm) { main.Classes[3].Seeds = 1 }, planted, "The planted bug is found outside both listed classes", "no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := m11Build(t, tt.mut)
			if len(m.Checks) != 10 {
				t.Fatalf("%d checks", len(m.Checks))
			}
			c := m.Checks[tt.row]
			if c.Hypothesis != tt.hyp {
				t.Fatalf("row %d = %q, want %q", tt.row, c.Hypothesis, tt.hyp)
			}
			if c.Held != tt.want {
				t.Errorf("Held = %q, want %q (measured %q)", c.Held, tt.want, c.Measured)
			}
		})
	}
}

func TestM11ArmExample(t *testing.T) {
	t.Parallel()
	m := m11Build(t, func(main, _ *budgetsim.Arm) {
		main.Classes[2].Seeds = 1
		main.Classes[2].Example = &budgetsim.Example{Seed: 42, Steps: []string{"kill r1", "settle", "retry"}}
	})
	e := m.Main.Example(budget.ClassOther)
	if e == nil {
		t.Fatal("no example for other")
	}
	if e.Line != "kill r1; settle; retry" || e.Seed != "42" || e.Title != budget.ClassOther {
		t.Errorf("example = %+v", e)
	}
	if m.Main.Example("nope") != nil {
		t.Error("Example(nope) != nil")
	}
}

func TestBuildM11Headline(t *testing.T) {
	t.Parallel()
	m := m11Build(t, nil)
	for _, w := range []string{"10,000 seeded fault schedules", "1,239 lost spend"} {
		if !strings.Contains(m.Headline, w) {
			t.Errorf("headline %q lacks %q", m.Headline, w)
		}
	}
}

func TestBuildM11MissingArm(t *testing.T) {
	t.Parallel()
	for _, keep := range []string{budgetsim.ArmMain, budgetsim.ArmPlanted} {
		s := m11Summary()
		var arms []budgetsim.Arm
		for _, a := range s.Arms {
			if a.Name == keep {
				arms = append(arms, a)
			}
		}
		s.Arms = arms
		if _, err := buildM11(s); err == nil {
			t.Errorf("only %s arm: no error", keep)
		}
	}
}

func TestLoadM11(t *testing.T) {
	t.Parallel()
	t.Run("missing dir", func(t *testing.T) {
		t.Parallel()
		m, err := loadM11(filepath.Join(t.TempDir(), "absent"))
		if m != nil || err != nil {
			t.Errorf("got %v, %v", m, err)
		}
	})
	t.Run("missing records file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		b, err := json.Marshal(m11Summary())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := budgetsim.WriteRecords(filepath.Join(dir, budgetsim.RecordsFile(budgetsim.ArmMain)), nil); err != nil {
			t.Fatal(err)
		}
		if m, err := loadM11(dir); err == nil || m != nil {
			t.Errorf("got %v, %v; want an error", m, err)
		}
	})
	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		b, err := json.Marshal(m11Summary())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, a := range []string{budgetsim.ArmMain, budgetsim.ArmPlanted} {
			if err := budgetsim.WriteRecords(filepath.Join(dir, budgetsim.RecordsFile(a)), nil); err != nil {
				t.Fatal(err)
			}
		}
		m, err := loadM11(dir)
		if err != nil || m == nil || m.Main == nil {
			t.Fatalf("got %v, %v", m, err)
		}
	})
}

func TestStepsShareSeedCount(t *testing.T) {
	t.Parallel()
	if got := steps(1); got != "1 step" {
		t.Errorf("steps(1) = %q", got)
	}
	if got := steps(3); got != "3 steps" {
		t.Errorf("steps(3) = %q", got)
	}
	if got := share(1, 3); got != "33.3%" {
		t.Errorf("share(1, 3) = %q", got)
	}
	if got := share(1, 0); got != "–" {
		t.Errorf("share(1, 0) = %q", got)
	}
	if got := seedCount(1234567); got != "1,234,567" {
		t.Errorf("seedCount = %q", got)
	}
}

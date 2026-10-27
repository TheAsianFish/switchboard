package cacheeval

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestPerturb(t *testing.T) {
	t.Parallel()
	q := "Janet has 16 eggs. She eats 3 in 2024 and sells the rest at $2 each, or 1,200 in total."
	sol := "16 - 3 = <<16-3=13>>13 eggs. 13 * 2 = <<13*2=26>>26 dollars.\n#### 26"
	seen := map[string]bool{}
	for seed := range uint64(50) {
		twin, from, to, ok := perturb(q, sol, rand.New(rand.NewPCG(seed, 1)))
		if !ok {
			t.Fatal("no number to perturb")
		}
		seen[from] = true
		// Only numbers the solution uses are changed; never the year or
		// a part of 1,200.
		if from != "16" && from != "3" && from != "2" {
			t.Fatalf("changed %q", from)
		}
		if strings.Count(twin, to) == 0 || twin == q || len(twin) < len(q)-1 {
			t.Fatalf("twin %q (%s -> %s)", twin, from, to)
		}
		if !strings.Contains(twin, "2024") || !strings.Contains(twin, "1,200") {
			t.Fatalf("twin changed a year or a grouped number: %q", twin)
		}
	}
	if len(seen) != 3 {
		t.Errorf("perturbed %v, want each of 16, 3 and 2 for some seed", seen)
	}
	if _, _, _, ok := perturb("How many legs does a dog have?", "#### 4", rand.New(rand.NewPCG(1, 1))); ok {
		t.Error("perturbed a question without numbers")
	}
}

func TestPerturbValues(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"3": "4", "9": "10", "10": "12", "16": "19", "200": "240"} {
		_, from, to, ok := perturb("There are "+in+" cats.", "#### "+in, rand.New(rand.NewPCG(1, 1)))
		if !ok || from != in || to != want {
			t.Errorf("%s -> %s (%v), want %s", in, to, ok, want)
		}
	}
}

func TestAnswers(t *testing.T) {
	t.Parallel()
	if g := gold("She makes 18 dollars.\n#### 18"); g != "18" {
		t.Errorf("gold = %q", g)
	}
	for out, want := range map[string]string{
		"Step 1: 16 - 3 = 13\nAnswer: 26":         "26",
		"So the total is $1,248.\nAnswer: $1,248": "1248",
		"It ends at -5 degrees.":                  "-5",
		"The answer: 3.60 dollars":                "3.60",
		"no number at all":                        "",
	} {
		if got := finalAnswer(out); got != want {
			t.Errorf("finalAnswer(%q) = %q, want %q", out, got, want)
		}
	}
	if !sameValue("1,248", "1248") || !sameValue("3.60", "3.6") || sameValue("3", "") {
		t.Error("sameValue")
	}
}

// The builder's check ignores order (a paraphrase may move quantities
// around), unlike the gateway's guard.
func TestKeepsNumbers(t *testing.T) {
	t.Parallel()
	if !keepsNumbers("Rose bought 12 onions and 4 potatoes, 4 times Sophia.", "Rose was 4 times Sophia: 12 onions and 4 potatoes.") {
		t.Error("reordered numbers should be kept")
	}
	if !keepsNumbers("three eggs at $2.50", "3 eggs at $2.5") {
		t.Error("words and trailing zeros")
	}
	if keepsNumbers("16 eggs", "15 eggs") || keepsNumbers("3 eggs", "3 eggs and 3 hens") {
		t.Error("different numbers kept")
	}
}

func TestSampleRows(t *testing.T) {
	t.Parallel()
	rows := make([]GSMRow, 10)
	for i := range rows {
		rows[i].Index = i
	}
	a := sampleRows(rows, 4, rand.New(rand.NewPCG(1, 2)))
	b := sampleRows(rows, 4, rand.New(rand.NewPCG(1, 2)))
	if len(a) != 4 {
		t.Fatalf("%d rows", len(a))
	}
	for i := range a {
		if a[i].Index != b[i].Index || (i > 0 && a[i].Index <= a[i-1].Index) {
			t.Fatalf("sample %v / %v not seeded or not in file order", a, b)
		}
	}
	if len(sampleRows(rows, 20, rand.New(rand.NewPCG(1, 2)))) != 10 {
		t.Error("oversampling")
	}
}

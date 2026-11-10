package routereval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/router"
)

// synthetic builds a table where the cheap candidate answers the "easy"
// half (short prompts) and the dear one answers everything but a few, so
// a router that reads prompt length should sit well above the line.
func synthetic(n int, seed uint64) *Table {
	rng := rand.New(rand.NewPCG(seed, 9))
	t := &Table{Models: []string{"small", "large"}}
	for i := range n {
		easy := i%2 == 0
		ds := DatasetGSM8K
		if i%3 == 0 {
			ds = DatasetMMLU
		}
		text := "How many apples are left over after 3 are eaten?"
		if !easy {
			text = "A train leaves at 9 and travels 120 km at 80 km/h, stops 15 minutes, then continues 200 km at 100 km/h; a second train leaves at 10 and travels 320 km at 110 km/h. Which arrives first and by how many minutes?"
		}
		emb := make([]float32, 4)
		for k := range emb {
			emb[k] = float32(rng.NormFloat64())
		}
		t.Questions = append(t.Questions, Question{ID: fmt.Sprintf("q%03d", i), Dataset: ds})
		smallRight := easy && rng.Float64() < 0.95
		largeRight := rng.Float64() < 0.9
		t.Correct = append(t.Correct, []bool{smallRight, largeRight})
		t.Cost = append(t.Cost, []float64{1 + rng.Float64()*0.1, 10 + rng.Float64()})
		t.Cheap = append(t.Cheap, router.Cheap(text))
		t.Embedding = append(t.Embedding, emb)
	}
	return t
}

func TestRunBeatsTheLineOnLearnableData(t *testing.T) {
	t.Parallel()
	tab := synthetic(200, 1)
	s, w, err := Run(tab, CostGFLOPs, Options{Folds: 5, Bootstrap: 200, Seed: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cheapest != "small" || s.Best != "large" {
		t.Fatalf("line ends %s → %s, want small → large", s.Cheapest, s.Best)
	}
	op := s.OperatingPoint
	if !op.Met || op.Gain < 0.1 || op.GainCI[0] <= 0 {
		t.Errorf("operating point %+v: want the rule met and a clear gain over the line", op)
	}
	if op.PctBestCost > 80 {
		t.Errorf("operating point costs %.1f%% of the best candidate; routing the easy half should save more", op.PctBestCost)
	}
	if len(s.Variants) != 3 || s.Variants[2].Name != "router-cost" || s.Variants[2].Knob != "cost weight" {
		t.Fatalf("variants %v: want router, router-cheap and the cost-aware diagnosis", s.Variants)
	}
	for _, v := range s.Variants {
		if v.AUCGain <= 0 {
			t.Errorf("%s: mean gain %v, want positive", v.Name, v.AUCGain)
		}
		if v.Knob == "cost weight" {
			// Weight 0 sends each question to its likeliest candidate, a
			// large weight to the cheapest.
			if last := v.Frontier[len(v.Frontier)-1]; last.Share["small"] != 1 {
				t.Errorf("%s: largest weight shares %v, want all small", v.Name, last.Share)
			}
			continue
		}
		if v.AUROC["small"] < 0.8 {
			t.Errorf("%s: AUROC for small %v, want the length feature to separate it", v.Name, v.AUROC["small"])
		}
		first, last := v.Frontier[0], v.Frontier[len(v.Frontier)-1]
		if first.Share["small"] != 1 || last.Share["large"] != 1 {
			t.Errorf("%s: frontier runs from %v to %v, want all-small to all-large", v.Name, first.Share, last.Share)
		}
	}
	if s.Oracle.Accuracy < s.Candidates[1].Accuracy {
		t.Errorf("oracle %v below the best candidate %v", s.Oracle.Accuracy, s.Candidates[1].Accuracy)
	}
	if len(s.DatasetRules) != 4 {
		t.Errorf("%d dataset rules, want 2×2", len(s.DatasetRules))
	}
	if w.Fallback != "large" || w.QualityTarget != op.Target || w.Candidates[0].Model != "small" {
		t.Errorf("weights: fallback %s target %v first %s", w.Fallback, w.QualityTarget, w.Candidates[0].Model)
	}
	w.Embedding, w.Dimensions = "fake", 4
	if _, err := w.Marshal(); err != nil {
		t.Errorf("shipped weights do not validate: %v", err)
	}
}

func TestRunIsDeterministic(t *testing.T) {
	t.Parallel()
	enc := func() []byte {
		s, w, err := Run(synthetic(120, 2), CostGFLOPs, Options{Folds: 5, Bootstrap: 100, Seed: 3}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(s)
		b, _ := json.Marshal(w)
		return append(a, b...)
	}
	if !bytes.Equal(enc(), enc()) {
		t.Error("two runs on the same data differ")
	}
}

func TestFoldsStratify(t *testing.T) {
	t.Parallel()
	var qs []Question
	var idx []int
	for i := range 100 {
		ds := DatasetGSM8K
		if i >= 60 {
			ds = DatasetMMLU
		}
		qs = append(qs, Question{Dataset: ds})
		idx = append(idx, i)
	}
	f := Folds(qs, idx, 5, 1)
	count := map[[2]any]int{}
	for i, k := range f {
		if k < 0 || k >= 5 {
			t.Fatalf("row %d in fold %d", i, k)
		}
		count[[2]any{k, qs[i].Dataset}]++
	}
	for k := range 5 {
		if count[[2]any{k, DatasetGSM8K}] != 12 || count[[2]any{k, DatasetMMLU}] != 8 {
			t.Errorf("fold %d: %d GSM8K, %d MMLU; want 12 and 8", k, count[[2]any{k, DatasetGSM8K}], count[[2]any{k, DatasetMMLU}])
		}
	}
}

func TestLineAndAUCGain(t *testing.T) {
	t.Parallel()
	l := Line{CheapAcc: 0.4, CheapCost: 100, BestAcc: 0.8, BestCost: 300}
	for _, tt := range []struct{ cost, want float64 }{{50, 0.4}, {100, 0.4}, {200, 0.6}, {300, 0.8}, {400, 0.8}} {
		if got := l.At(tt.cost); math.Abs(got-tt.want) > 1e-12 {
			t.Errorf("At(%v) = %v, want %v", tt.cost, got, tt.want)
		}
	}
	onLine := []FrontierPoint{{Point: Point{Accuracy: 0.4, CostPer1K: 100}}, {Point: Point{Accuracy: 0.8, CostPer1K: 300}}}
	if g := AUCGain(onLine, l, l); math.Abs(g) > 1e-9 {
		t.Errorf("a frontier on the line gains %v, want 0", g)
	}
	// A frontier that reaches the best accuracy at the cheap end's cost
	// is above the line by (0.8-0.4)/2 on average.
	above := []FrontierPoint{{Point: Point{Accuracy: 0.8, CostPer1K: 100}}, {Point: Point{Accuracy: 0.8, CostPer1K: 300}}}
	if g := AUCGain(above, l, l); math.Abs(g-0.2) > 1e-3 {
		t.Errorf("gain %v, want 0.2", g)
	}
}

func TestHull(t *testing.T) {
	t.Parallel()
	// A middle candidate above the line lifts the hull; one below it and
	// one dearer but no better are left out.
	h := NewHull([]float64{100, 200, 250, 300, 400}, []float64{0.4, 0.7, 0.5, 0.8, 0.75})
	if !slices.Equal(h.Cost, []float64{100, 200, 300}) {
		t.Fatalf("hull vertices at %v, want 100, 200, 300", h.Cost)
	}
	for _, tt := range []struct{ cost, want float64 }{{50, 0.4}, {150, 0.55}, {250, 0.75}, {500, 0.8}} {
		if got := h.At(tt.cost); math.Abs(got-tt.want) > 1e-12 {
			t.Errorf("At(%v) = %v, want %v", tt.cost, got, tt.want)
		}
	}
	// On two candidates the hull is the line.
	l := Line{CheapAcc: 0.4, CheapCost: 100, BestAcc: 0.8, BestCost: 300}
	two := NewHull([]float64{300, 100}, []float64{0.8, 0.4})
	for _, c := range []float64{100, 170, 300} {
		if math.Abs(two.At(c)-l.At(c)) > 1e-12 {
			t.Errorf("two-candidate hull at %v = %v, line %v", c, two.At(c), l.At(c))
		}
	}
}

func TestWilsonAndAUROC(t *testing.T) {
	t.Parallel()
	ci := Wilson(50, 100)
	if math.Abs(ci[0]-0.4038) > 1e-3 || math.Abs(ci[1]-0.5962) > 1e-3 {
		t.Errorf("Wilson(50, 100) = %v, want about [0.404, 0.596]", ci)
	}
	if ci := Wilson(0, 10); ci[0] != 0 || ci[1] < 0.27 || ci[1] > 0.28 {
		t.Errorf("Wilson(0, 10) = %v", ci)
	}
	if a := AUROC([]float64{0.1, 0.2, 0.3, 0.4}, []bool{false, false, true, true}); a != 1 {
		t.Errorf("perfect ranking AUROC %v", a)
	}
	if a := AUROC([]float64{0.5, 0.5, 0.5, 0.5}, []bool{false, true, false, true}); a != 0.5 {
		t.Errorf("all-tied AUROC %v, want 0.5", a)
	}
	if a := AUROC([]float64{1, 2}, []bool{true, true}); !math.IsNaN(a) {
		t.Errorf("one-class AUROC %v, want NaN", a)
	}
}

func TestBuildTableNeedsEveryLabel(t *testing.T) {
	t.Parallel()
	qs := []Question{{ID: "a", Dataset: DatasetGSM8K, Question: "1+1?", Answer: "2"}, {ID: "b", Dataset: DatasetGSM8K, Question: "2+2?", Answer: "4"}}
	labels := map[string][]Label{
		"m1": {{ID: "a", Output: "Answer: 2", InputTokens: 10, OutputTokens: 5}, {ID: "b", Output: "Answer: 5", Correct: true, InputTokens: 10, OutputTokens: 5}},
		"m2": {{ID: "a", Output: "Answer: 2", Correct: true, InputTokens: 10, OutputTokens: 5}},
	}
	embs := []Embedding{{ID: "a", Vector: []float32{1}}, {ID: "b", Vector: []float32{1}}}
	params := map[string]float64{"m1": 1, "m2": 2}
	if _, _, _, err := BuildTable(qs, []string{"m1", "m2"}, labels, embs, params); err == nil {
		t.Fatal("a missing label was accepted")
	}
	labels["m2"] = append(labels["m2"], Label{ID: "b", Output: "4", InputTokens: 10, OutputTokens: 5})
	tab, unit, regraded, err := BuildTable(qs, []string{"m1", "m2"}, labels, embs, params)
	if err != nil {
		t.Fatal(err)
	}
	// m1/a and m1/b and m2/b were recorded with the wrong grade.
	if unit != CostGFLOPs || regraded != 3 {
		t.Errorf("unit %s, regraded %d; want gflops and 3", unit, regraded)
	}
	if !tab.Correct[0][0] || tab.Correct[1][0] || !tab.Correct[1][1] {
		t.Errorf("grades %v: want the current grader's", tab.Correct)
	}
	if tab.Cost[0][1] != 2*2*15 {
		t.Errorf("cost %v, want 2 × params × tokens = 60", tab.Cost[0][1])
	}
	if _, _, _, err := BuildTable(qs, []string{"m1", "m2"}, labels, embs, map[string]float64{"m1": 1}); err == nil {
		t.Error("a candidate with neither price nor parameter count was accepted")
	}
}

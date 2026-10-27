package cacheeval

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestWilson(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		k, n         int
		p, low, high float64
	}{
		{"empty", 0, 0, 0, 0, 1},
		{"half of ten", 5, 10, 0.5, 0.236593, 0.763407},
		{"none of 381", 0, 381, 0, 0, 3.841459 / (381 + 3.841459)},
		{"none of 380", 0, 380, 0, 0, 3.841459 / (380 + 3.841459)},
		{"all of 4", 4, 4, 1, 4 / (4 + 3.841459), 1},
		{"one of 100", 1, 100, 0.01, 0.001767, 0.054486},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := Wilson(tt.k, tt.n)
			if r.K != tt.k || r.N != tt.n || math.Abs(r.P-tt.p) > 1e-6 || math.Abs(r.Low-tt.low) > 2e-6 || math.Abs(r.High-tt.high) > 2e-6 {
				t.Errorf("Wilson(%d, %d) = %+v, want p %v [%v, %v]", tt.k, tt.n, r, tt.p, tt.low, tt.high)
			}
		})
	}
	// 381 clean negatives are the fewest whose upper bound is under 1%.
	if _, _, h := wilson(0, 381); h > 0.01 {
		t.Errorf("upper(0/381) = %v, want <= 0.01", h)
	}
	if _, _, h := wilson(0, 380); h <= 0.01 {
		t.Errorf("upper(0/380) = %v, want > 0.01", h)
	}
}

func TestThresholds(t *testing.T) {
	t.Parallel()
	ts := Thresholds()
	if len(ts) != 297 || ts[0] != 0.5 || ts[195] != 0.9875 || ts[196] != 0.99 || ts[296] != 1 {
		t.Fatalf("grid has %d points from %v to %v", len(ts), ts[0], ts[len(ts)-1])
	}
	for i, v := range ts {
		// Every point is exactly the float64 nearest its four-decimal value.
		x := 0.5 + float64(i)/400
		if i >= 196 {
			x = 0.99 + float64(i-196)/10000
		}
		want, err := strconv.ParseFloat(fmt.Sprintf("%.4f", x), 64)
		if err != nil || v != want {
			t.Errorf("point %d is %v, want %v", i, v, want)
		}
	}
	// 0.5 + 80 * 0.0025 accumulated in floating point is not 0.7; the
	// grid's point is, so a pair at exactly 0.7 hits there.
	if ts[80] != 0.7 || !hit(SimPair{Cos: 0.7}, ts[80], false) {
		t.Errorf("ts[80] = %v", ts[80])
	}
}

// pairs makes n pairs with the same label, cosine and number flag.
func pairs(n int, same bool, cos float64, numbersEqual bool) []SimPair {
	out := make([]SimPair, n)
	for i := range out {
		out[i] = SimPair{ID: fmt.Sprint(i), Same: same, Cos: cos, NumbersEqual: numbersEqual}
	}
	return out
}

func concat(sets ...[]SimPair) []SimPair {
	var out []SimPair
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

func TestSweepCurve(t *testing.T) {
	t.Parallel()
	// Positives at 0.97 and 0.9 (numbers equal) and 0.7 (numbers differ);
	// 400 negatives at 0.6 with equal numbers and 2 at 0.95 whose numbers
	// differ.
	mixed := concat(
		[]SimPair{{Same: true, Cos: 0.97, NumbersEqual: true}, {Same: true, Cos: 0.9, NumbersEqual: true}, {Same: true, Cos: 0.7}},
		pairs(400, false, 0.6, true), pairs(2, false, 0.95, false),
	)
	tests := []struct {
		name               string
		pairs              []SimPair
		guard              bool
		pointT, upperT     float64 // 0: not found
		pointHit, upperHit int
	}{
		{
			// 2 false hits of 402 is under 1% observed from 0.6025 on, but
			// the upper bound only clears 1% once they are gone at 0.9525.
			name: "no guard", pairs: mixed,
			pointT: 0.6025, pointHit: 3, upperT: 0.9525, upperHit: 1,
		},
		{
			// The guard removes the 0.95 negatives and the 0.7 positive.
			name: "guard", pairs: mixed, guard: true,
			pointT: 0.6025, pointHit: 2, upperT: 0.6025, upperHit: 2,
		},
		{
			// Without negatives the observed rate is 0 everywhere and the
			// interval is [0, 1], so only the point rule is met.
			name: "no negatives", pairs: pairs(5, true, 0.8, true),
			pointT: 0.5, pointHit: 5,
		},
		{
			// Negatives identical in embedding never stop hitting.
			name: "nothing meets the target", pairs: concat(pairs(3, true, 1, true), pairs(500, false, 1, true)),
		},
		{
			// Few negatives: the point is met, the bound never is.
			name: "too few negatives", pairs: concat(pairs(3, true, 0.9, true), pairs(10, false, 0.8, true)),
			pointT: 0.8025, pointHit: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := SweepCurve("m", "s", tt.pairs, tt.guard, 0.01)
			if len(c.Points) != 297 {
				t.Fatalf("%d points", len(c.Points))
			}
			check := func(what string, op Operating, wantT float64, wantHit int) {
				t.Helper()
				if wantT == 0 {
					if op.Found {
						t.Errorf("%s found at %v, want none", what, op.Threshold)
					}
					return
				}
				if !op.Found || op.Threshold != wantT || op.HitRate.K != wantHit {
					t.Errorf("%s = %+v, want threshold %v with %d hits", what, op, wantT, wantHit)
				}
			}
			check("AtPoint", c.AtPoint, tt.pointT, tt.pointHit)
			check("AtUpper", c.AtUpper, tt.upperT, tt.upperHit)
		})
	}
}

func TestSweepCurveCounts(t *testing.T) {
	t.Parallel()
	c := SweepCurve("m", "s", concat(pairs(3, true, 0.8, true), pairs(2, false, 0.7, false)), false, 0.01)
	if c.Positives != 3 || c.Negatives != 2 {
		t.Fatalf("positives %d negatives %d", c.Positives, c.Negatives)
	}
	// At 0.7 everything hits; just above it the negatives stop.
	at := func(th float64) SweepPoint {
		for _, p := range c.Points {
			if p.Threshold == th {
				return p
			}
		}
		t.Fatalf("no point at %v", th)
		return SweepPoint{}
	}
	if p := at(0.7); p.HitRate.K != 3 || p.FalseHitRate.K != 2 {
		t.Errorf("at 0.7: %+v", p)
	}
	if p := at(0.7025); p.HitRate.K != 3 || p.FalseHitRate.K != 0 {
		t.Errorf("at 0.7025: %+v", p)
	}
	if p := at(1); p.HitRate.K != 0 {
		t.Errorf("at 1: %+v", p)
	}
}

func TestBaselineOf(t *testing.T) {
	t.Parallel()
	b := BaselineOf("s", []SimPair{
		{Same: true, Exact: true, Norm: true},
		{Same: true, Norm: true},
		{Same: true},
		{Same: true},
		{Same: false, Norm: true},
		{Same: false},
	})
	if b.ExactHitRate.K != 1 || b.ExactHitRate.N != 4 || b.NormalisedHitRate.K != 2 ||
		b.ExactFalseHitRate.K != 0 || b.ExactFalseHitRate.N != 2 || b.NormalisedFalseHitRate.K != 1 {
		t.Errorf("baseline = %+v", b)
	}
}

func curveFor(set string, upper float64, guard bool) Curve {
	// Positives spread over the grid so each threshold has its own rate.
	ps := concat(pairs(1, true, 0.55, true), pairs(1, true, 0.75, true), pairs(1, true, 0.95, true), pairs(1, true, 0.99, true))
	if upper > 0 {
		ps = concat(ps, pairs(400, false, upper-thresholdStep, true))
	} else {
		ps = concat(ps, pairs(400, false, 1, true))
	}
	return SweepCurve("m", set, ps, guard, 0.01)
}

func TestJointOf(t *testing.T) {
	t.Parallel()
	a, b := curveFor("a", 0.7, true), curveFor("b", 0.9, true)
	if a.AtUpper.Threshold != 0.7 || b.AtUpper.Threshold != 0.9 {
		t.Fatalf("fixture: %v %v", a.AtUpper, b.AtUpper)
	}
	j := JointOf("m", true, []Curve{a, b})
	// At 0.9 two of the four positives (0.95, 0.99) hit on both sets.
	if !j.Found || j.Threshold != 0.9 || len(j.PerSet) != 2 || j.PerSet[0].HitRate.K != 2 || j.PerSet[1].HitRate.K != 2 || j.MeanHitRate != 0.5 {
		t.Errorf("joint = %+v", j)
	}
	if j := JointOf("m", true, []Curve{a, curveFor("c", 0, true)}); j.Found || len(j.PerSet) != 0 {
		t.Errorf("joint with an unmet set = %+v", j)
	}
	if j := JointOf("m", true, nil); j.Found {
		t.Errorf("joint of nothing = %+v", j)
	}
}

func TestChoose(t *testing.T) {
	t.Parallel()
	dims := map[string]int{"small": 384, "big": 768}
	models := []string{"big", "small"}
	tests := []struct {
		name      string
		joints    []Joint
		model     string
		threshold float64
	}{
		{
			name: "higher mean hit rate wins",
			joints: []Joint{
				{Model: "big", Guard: true, Found: true, Threshold: 0.9, MeanHitRate: 0.6},
				{Model: "small", Guard: true, Found: true, Threshold: 0.8, MeanHitRate: 0.5},
			},
			model: "big", threshold: 0.9,
		},
		{
			name: "tie goes to fewer dimensions",
			joints: []Joint{
				{Model: "big", Guard: true, Found: true, Threshold: 0.9, MeanHitRate: 0.5},
				{Model: "small", Guard: true, Found: true, Threshold: 0.85, MeanHitRate: 0.5},
			},
			model: "small", threshold: 0.85,
		},
		{
			name: "unguarded joints do not count",
			joints: []Joint{
				{Model: "big", Guard: false, Found: true, Threshold: 0.7, MeanHitRate: 0.9},
				{Model: "small", Guard: true, Found: true, Threshold: 0.95, MeanHitRate: 0.1},
				{Model: "big", Guard: true, Found: false},
			},
			model: "small", threshold: 0.95,
		},
		{
			name:   "none found ships semantic matching off",
			joints: []Joint{{Model: "big", Guard: true}, {Model: "small", Guard: true}, {Model: "small", Found: true, Threshold: 0.9}},
			model:  "", threshold: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := Choose(tt.joints, dims, models)
			if c.Model != tt.model || c.Threshold != tt.threshold || !c.Guard || !strings.HasPrefix(c.Rule, ChosenRule) {
				t.Errorf("Choose = %+v", c)
			}
			if (c.Model == "") != strings.Contains(c.Rule, "ships off") {
				t.Errorf("rule = %q", c.Rule)
			}
		})
	}
}

func TestSummarise(t *testing.T) {
	t.Parallel()
	mk := func(model, set string, dims int, negCos float64) Sims {
		ps := concat(pairs(10, true, 0.9, true), pairs(400, false, negCos, true))
		ps[0].Exact, ps[0].Norm = true, true
		return Sims{Model: model, Set: set, Dims: dims, Pairs: ps}
	}
	sims := []Sims{
		mk("b", "x", 768, 0.8), mk("a", "y", 384, 0.7), mk("a", "x", 384, 0.6), mk("b", "y", 768, 0.7),
	}
	s, err := Summarise(sims, []string{"a", "b"}, []string{"x", "y"}, 0.01, nil, DataMeta{Model: "gen"}, RunInfo{Commit: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Curves) != 8 || len(s.Joints) != 4 || len(s.Baselines) != 2 {
		t.Fatalf("%d curves, %d joints, %d baselines", len(s.Curves), len(s.Joints), len(s.Baselines))
	}
	// Order: model, then guard off before on, then set.
	if c := s.Curves[2]; c.Model != "a" || !c.Guard || c.Set != "x" {
		t.Errorf("curve 2 = %s %v %s", c.Model, c.Guard, c.Set)
	}
	if s.Baselines[0].Set != "x" || s.Baselines[0].ExactHitRate.K != 1 {
		t.Errorf("baseline = %+v", s.Baselines[0])
	}
	// Both models hit every positive at their joint thresholds (a: 0.7025,
	// b: 0.8025), so the smaller one wins the tie.
	if s.Chosen.Model != "a" || s.Chosen.Threshold != 0.7025 || s.Data.Model != "gen" || s.Commit != "c" {
		t.Errorf("chosen = %+v", s.Chosen)
	}
	if _, err := Summarise(sims[:3], []string{"a", "b"}, []string{"x", "y"}, 0.01, nil, DataMeta{}, RunInfo{}); err == nil {
		t.Error("missing similarity file accepted")
	}
	if _, err := Summarise(append(sims, sims[0]), []string{"a", "b"}, []string{"x", "y"}, 0.01, nil, DataMeta{}, RunInfo{}); err == nil {
		t.Error("duplicate similarity file accepted")
	}
}

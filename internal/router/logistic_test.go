package router

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestFitScaler(t *testing.T) {
	t.Parallel()
	X := [][]float64{{1, 7, 0}, {3, 7, 10}, {5, 7, 20}}
	s := FitScaler(X)
	if !slices.Equal(s.Mean, []float64{3, 7, 10}) {
		t.Errorf("mean %v", s.Mean)
	}
	if s.Std[1] != 1 {
		t.Errorf("constant column std %v, want 1", s.Std[1])
	}
	want0 := math.Sqrt(8.0 / 3)
	if math.Abs(s.Std[0]-want0) > 1e-12 {
		t.Errorf("std[0] %v, want %v", s.Std[0], want0)
	}

	// Scaled columns have mean 0 and std 1, the constant one is all 0,
	// and Apply does not touch its input.
	in := slices.Clone(X[0])
	scaled := make([][]float64, len(X))
	for i, row := range X {
		scaled[i] = s.Apply(row)
	}
	if !slices.Equal(in, X[0]) {
		t.Error("Apply modified its input")
	}
	again := FitScaler(scaled)
	for j := range again.Mean {
		if math.Abs(again.Mean[j]) > 1e-12 {
			t.Errorf("scaled mean[%d] = %v", j, again.Mean[j])
		}
	}
	for _, row := range scaled {
		if row[1] != 0 {
			t.Errorf("constant column scaled to %v", row[1])
		}
	}
	if math.Abs(again.Std[0]-1) > 1e-12 || math.Abs(again.Std[2]-1) > 1e-12 {
		t.Errorf("scaled std %v", again.Std)
	}

	// Round trip: undoing the scaling restores the input.
	for i, row := range scaled {
		for j := range row {
			if back := row[j]*s.Std[j] + s.Mean[j]; math.Abs(back-X[i][j]) > 1e-12 {
				t.Errorf("round trip [%d][%d] = %v, want %v", i, j, back, X[i][j])
			}
		}
	}

	if e := FitScaler(nil); len(e.Mean) != 0 || len(e.Std) != 0 {
		t.Errorf("empty: %+v", e)
	}
}

func TestProbStable(t *testing.T) {
	t.Parallel()
	m := Logistic{Weights: []float64{1}}
	for _, z := range []float64{-1000, -40, 0, 40, 1000} {
		p := m.Prob([]float64{z})
		if math.IsNaN(p) || p < 0 || p > 1 {
			t.Errorf("Prob(%v) = %v", z, p)
		}
	}
	if p := m.Prob([]float64{0}); p != 0.5 {
		t.Errorf("Prob(0) = %v", p)
	}
}

// synthetic draws n rows of d standard-normal features and labels from a
// logistic model with the given weights and bias.
func synthetic(seed uint64, n int, w []float64, b float64) ([][]float64, []bool) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	X := make([][]float64, n)
	y := make([]bool, n)
	for i := range n {
		row := make([]float64, len(w))
		z := b
		for j := range row {
			row[j] = rng.NormFloat64()
			z += w[j] * row[j]
		}
		X[i] = row
		y[i] = rng.Float64() < sigmoid(z)
	}
	return X, y
}

func TestTrainRecoversWeights(t *testing.T) {
	t.Parallel()
	trueW := []float64{2, -1.5, 0, 1, 0}
	X, y := synthetic(1, 4000, trueW, 0.5)
	m, err := Train(X, y, TrainOptions{L2: 1})
	if err != nil {
		t.Fatal(err)
	}
	for j, w := range trueW {
		if math.Abs(m.Weights[j]-w) > 0.25 {
			t.Errorf("w[%d] = %.3f, want about %v", j, m.Weights[j], w)
		}
		if w != 0 && math.Signbit(m.Weights[j]) != math.Signbit(w) {
			t.Errorf("w[%d] = %.3f has the wrong sign", j, m.Weights[j])
		}
	}
	if math.Abs(m.Bias-0.5) > 0.25 {
		t.Errorf("bias %.3f, want about 0.5", m.Bias)
	}
}

// gradientDescent minimises the same objective as Train by plain
// gradient descent with a fixed step of 1/L, L the gradient's Lipschitz
// bound. It is slow but too simple to be wrong in the same way.
func gradientDescent(rows [][]float64, y []bool, l2 float64, iters int) Logistic {
	d := len(rows[0])
	var norm2 float64
	for _, row := range rows {
		norm2 += 1
		for _, v := range row {
			norm2 += v * v
		}
	}
	step := 1 / (norm2/4 + l2)
	w := make([]float64, d)
	var b float64
	g := make([]float64, d)
	for range iters {
		clear(g)
		var gb float64
		m := Logistic{Weights: w, Bias: b}
		for i, row := range rows {
			r := m.Prob(row)
			if y[i] {
				r--
			}
			for j := range g {
				g[j] += r * row[j]
			}
			gb += r
		}
		for j := range w {
			w[j] -= step * (g[j] + l2*w[j])
		}
		b -= step * gb
	}
	return Logistic{Weights: w, Bias: b}
}

func TestTrainMatchesGradientDescent(t *testing.T) {
	t.Parallel()
	X, y := synthetic(2, 60, []float64{1, -2, 0.5}, -0.3)
	for _, l2 := range []float64{0.1, 1, 10} {
		got, err := Train(X, y, TrainOptions{L2: l2})
		if err != nil {
			t.Fatal(err)
		}
		want := gradientDescent(X, y, l2, 20_000)
		for j := range want.Weights {
			if math.Abs(got.Weights[j]-want.Weights[j]) > 1e-4 {
				t.Errorf("L2 %v: w[%d] = %v, gradient descent %v", l2, j, got.Weights[j], want.Weights[j])
			}
		}
		if math.Abs(got.Bias-want.Bias) > 1e-4 {
			t.Errorf("L2 %v: bias = %v, gradient descent %v", l2, got.Bias, want.Bias)
		}
	}
}

func TestTrainL2Shrinks(t *testing.T) {
	t.Parallel()
	X, y := synthetic(3, 300, []float64{1.5, -1, 0.5, 0, 2}, 0)
	prev := math.Inf(1)
	for _, l2 := range []float64{0.01, 1, 10, 100, 1000} {
		m, err := Train(X, y, TrainOptions{L2: l2})
		if err != nil {
			t.Fatal(err)
		}
		var norm float64
		for _, w := range m.Weights {
			norm += w * w
		}
		norm = math.Sqrt(norm)
		if norm >= prev {
			t.Errorf("L2 %v: ||w|| = %v, not below %v", l2, norm, prev)
		}
		prev = norm
	}
}

func TestTrainConstantLabels(t *testing.T) {
	t.Parallel()
	X, _ := synthetic(4, 9, []float64{1, 1}, 0)
	tests := []struct {
		name  string
		label bool
		rate  float64
	}{
		{"all true", true, 9.5 / 10},
		{"all false", false, 0.5 / 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			y := make([]bool, len(X))
			for i := range y {
				y[i] = tt.label
			}
			m, err := Train(X, y, TrainOptions{L2: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(m.Weights, []float64{0, 0}) {
				t.Errorf("weights %v, want zero", m.Weights)
			}
			if p := m.Prob(X[0]); math.Abs(p-tt.rate) > 1e-12 {
				t.Errorf("P = %v, want %v", p, tt.rate)
			}
		})
	}
}

func TestTrainDeterministic(t *testing.T) {
	t.Parallel()
	X, y := synthetic(5, 400, []float64{0.5, -0.5, 1, 0, 0, 0.3, -1, 0.2}, 0.1)
	var out [2][]byte
	for i := range out {
		m, err := Train(X, y, TrainOptions{L2: 2})
		if err != nil {
			t.Fatal(err)
		}
		if out[i], err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
	}
	if string(out[0]) != string(out[1]) {
		t.Errorf("two runs differ:\n%s\n%s", out[0], out[1])
	}
}

func TestTrainWideAndCollinear(t *testing.T) {
	t.Parallel()
	// More features than rows, with a duplicated column: the shape of the
	// real problem (hundreds of embedding dimensions, a few hundred rows).
	X, y := synthetic(6, 50, make([]float64, 120), 0)
	for i := range X {
		X[i][1] = X[i][0]
		y[i] = X[i][0] > 0
	}
	m, err := Train(X, y, TrainOptions{L2: 1})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.Weights[0]-m.Weights[1]) > 1e-6 || m.Weights[0] <= 0 {
		t.Errorf("duplicated column weights %v, %v", m.Weights[0], m.Weights[1])
	}
}

func TestTrainErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		X    [][]float64
		y    []bool
		opt  TrainOptions
	}{
		{"no rows", nil, nil, TrainOptions{L2: 1}},
		{"label count", [][]float64{{1}}, []bool{true, false}, TrainOptions{L2: 1}},
		{"ragged", [][]float64{{1}, {1, 2}}, []bool{true, false}, TrainOptions{L2: 1}},
		{"NaN", [][]float64{{math.NaN()}, {1}}, []bool{true, false}, TrainOptions{L2: 1}},
		{"negative L2", [][]float64{{0}, {1}}, []bool{true, false}, TrainOptions{L2: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Train(tt.X, tt.y, tt.opt); !errors.Is(err, ErrBadTrainingData) {
				t.Errorf("err = %v, want ErrBadTrainingData", err)
			}
		})
	}
}

// TestTrainSolversAgree checks that the row-space (Woodbury) Newton
// solver used for wide data reaches the same optimum as the one that
// forms the full Hessian.
func TestTrainSolversAgree(t *testing.T) {
	t.Parallel()
	w := make([]float64, 80)
	for j := range w {
		w[j] = float64(j%7-3) / 4
	}
	X, y := synthetic(7, 40, w, 0.4)
	for _, l2 := range []float64{0.1, 1, 30} {
		opt := TrainOptions{L2: l2, Tol: 1e-12}
		primal, err := train(X, y, opt, false)
		if err != nil {
			t.Fatal(err)
		}
		dual, err := train(X, y, opt, true)
		if err != nil {
			t.Fatal(err)
		}
		auto, err := Train(X, y, opt)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(auto.Weights, dual.Weights) || auto.Bias != dual.Bias {
			t.Errorf("L2 %v: Train did not use the row-space solver for wide data", l2)
		}
		for j := range primal.Weights {
			if math.Abs(primal.Weights[j]-dual.Weights[j]) > 1e-8 {
				t.Errorf("L2 %v: w[%d] primal %v, dual %v", l2, j, primal.Weights[j], dual.Weights[j])
			}
		}
		if math.Abs(primal.Bias-dual.Bias) > 1e-8 {
			t.Errorf("L2 %v: bias primal %v, dual %v", l2, primal.Bias, dual.Bias)
		}
	}
}

// BenchmarkTrain is the shape of the router eval: about 480 prompts, 9
// cheap features plus a 768-dimensional embedding. Nested cross-validation
// calls Train hundreds of times, so this must stay well under a second.
func BenchmarkTrain(b *testing.B) {
	const n, d = 480, 777
	rng := rand.New(rand.NewPCG(8, 9))
	trueW := make([]float64, d)
	for j := range trueW {
		trueW[j] = rng.NormFloat64() * 0.1
	}
	X, y := synthetic(8, n, trueW, 0.3)
	for b.Loop() {
		if _, err := Train(X, y, TrainOptions{L2: 1}); err != nil {
			b.Fatal(err)
		}
	}
}

package router

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(w *Weights)
	}{
		{"version", func(w *Weights) { w.Version = 2 }},
		{"features changed", func(w *Weights) { w.Features[0] = "chars" }},
		{"features missing", func(w *Weights) { w.Features = w.Features[1:] }},
		{"embedding without dimensions", func(w *Weights) { w.Dimensions = 0 }},
		{"dimensions without embedding", func(w *Weights) { w.Embedding = "" }},
		{"negative dimensions", func(w *Weights) { w.Dimensions = -1 }},
		{"scaler length", func(w *Weights) { w.Scaler.Mean = w.Scaler.Mean[1:] }},
		{"scaler std zero", func(w *Weights) { w.Scaler.Std[3] = 0 }},
		{"scaler mean NaN", func(w *Weights) { w.Scaler.Mean[3] = math.NaN() }},
		{"cost unit", func(w *Weights) { w.CostUnit = "tokens" }},
		{"no candidates", func(w *Weights) { w.Candidates = nil }},
		{"empty model", func(w *Weights) { w.Candidates[0].Model = "" }},
		{"duplicate model", func(w *Weights) { w.Candidates[1].Model = "small" }},
		{"unsorted", func(w *Weights) { w.Candidates[0].Cost = 5 }},
		{"negative cost", func(w *Weights) { w.Candidates[0].Cost = -1 }},
		{"infinite cost", func(w *Weights) { w.Candidates[2].Cost = math.Inf(1) }},
		{"accuracy above 1", func(w *Weights) { w.Candidates[0].Accuracy = 1.1 }},
		{"weights length", func(w *Weights) { w.Candidates[1].Logistic.Weights = w.Candidates[1].Logistic.Weights[1:] }},
		{"weight NaN", func(w *Weights) { w.Candidates[1].Logistic.Weights[0] = math.NaN() }},
		{"bias infinite", func(w *Weights) { w.Candidates[1].Logistic.Bias = math.Inf(-1) }},
		{"fallback unknown", func(w *Weights) { w.Fallback = "huge" }},
		{"target negative", func(w *Weights) { w.QualityTarget = -0.1 }},
		{"target above 1", func(w *Weights) { w.QualityTarget = 1.01 }},
	}
	if err := testWeights(4, 0, 0, 0).Validate(); err != nil {
		t.Fatalf("valid weights rejected: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := testWeights(4, 0, 0, 0)
			tt.mutate(w)
			if err := w.Validate(); !errors.Is(err, ErrInvalidWeights) {
				t.Errorf("Validate = %v, want ErrInvalidWeights", err)
			}
			if _, err := w.Marshal(); err == nil {
				t.Error("Marshal accepted invalid weights")
			}
		})
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	w := testWeights(4, -0.123456789012345, 1e-300, 3)
	w.Scaler.Mean[2], w.Scaler.Std[2] = 0.1, 1.0/3
	w.Candidates[0].Logistic.Weights[5] = -2.5e-7
	w.Candidates[1].Cost = 2.0000000000000004
	data, err := w.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if data[len(data)-1] != '\n' {
		t.Error("no trailing newline")
	}
	path := filepath.Join(t.TempDir(), "weights.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := back.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Errorf("round trip changed the file:\n%s\n%s", data, again)
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	good, err := testWeights(0, 0, 0, 0).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data string
	}{
		{"not JSON", "nope"},
		{"unknown field", `{"version":1,"extra":true}`},
		{"trailing data", string(good) + `{"version":1}`},
		{"empty object", `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(tt.data)); !errors.Is(err, ErrInvalidWeights) {
				t.Errorf("Parse = %v, want ErrInvalidWeights", err)
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

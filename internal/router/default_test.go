package router

import (
	"bytes"
	"os"
	"testing"
)

// TestDefaultIsTheEvals fails when the built-in weights are not the ones
// the last `make eval-router` wrote beside its results.
func TestDefaultIsTheEvals(t *testing.T) {
	t.Parallel()
	w, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	eval, err := os.ReadFile("../../eval/results/q5-router/weights.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(eval, defaultWeights) {
		t.Fatal("internal/router/weights.json differs from eval/results/q5-router/weights.json; run make eval-router")
	}
	if w.Embedding == "" || len(w.Candidates) < 2 {
		t.Errorf("built-in weights: embedding %q, %d candidates", w.Embedding, len(w.Candidates))
	}
}

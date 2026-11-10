package router

import (
	_ "embed"
	"fmt"
)

// defaultWeights are the weights `make eval-router` trained on the whole
// eval set; the eval writes this file and its own copy in
// eval/results/q5-router/, and a test keeps the two equal.
//
//go:embed weights.json
var defaultWeights []byte

// Default returns the built-in weights.
func Default() (*Weights, error) {
	w, err := Parse(defaultWeights)
	if err != nil {
		return nil, fmt.Errorf("built-in router weights: %w", err)
	}
	return w, nil
}

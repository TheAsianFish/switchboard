package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
)

// WeightsVersion is the weights file format this package reads and
// writes. It changes when the meaning of a field changes.
const WeightsVersion = 1

// Cost units a weights file may use.
const (
	CostUSD    = "usd"
	CostGFLOPs = "gflops"
)

// ErrInvalidWeights is returned for a weights file that fails validation.
var ErrInvalidWeights = errors.New("router: invalid weights")

// Candidate is one model the router may pick.
type Candidate struct {
	// Model is the client-facing model name in the gateway config.
	Model string `json:"model"`
	// Cost is the mean cost per request in Weights.CostUnit, measured on
	// the training data. Only the order of costs matters to the router.
	Cost float64 `json:"cost"`
	// Accuracy is the share of training prompts the model answered
	// correctly. The router uses it only when it cannot predict.
	Accuracy float64 `json:"accuracy"`
	// Logistic predicts P(correct) from the scaled feature vector.
	Logistic Logistic `json:"logistic"`
}

// Weights is a trained router: everything needed to turn a prompt into a
// model choice, written by the router eval and read by the gateway.
type Weights struct {
	Version int `json:"version"`
	// Embedding names the embedding model whose vectors follow the cheap
	// features; "" for the cheap-only variant.
	Embedding string `json:"embedding"`
	// Dimensions is the embedding length; 0 when Embedding is "".
	Dimensions int `json:"dimensions"`
	// Features names the cheap features the file was trained on. It must
	// equal CheapFeatureNames, so a file from an older feature set is
	// refused rather than silently misread.
	Features []string `json:"features"`
	Scaler   Scaler   `json:"scaler"`
	// CostUnit is CostUSD or CostGFLOPs.
	CostUnit string `json:"cost_unit"`
	// Candidates are sorted by Cost ascending, so the first one that
	// clears the target is the cheapest.
	Candidates []Candidate `json:"candidates"`
	// Fallback is the candidate used when none clears the target: the
	// most accurate one.
	Fallback string `json:"fallback"`
	// QualityTarget is the default P(correct) a candidate must reach,
	// chosen by the eval.
	QualityTarget float64 `json:"quality_target"`
	// Source says which command and commit produced the file.
	Source string `json:"source"`
}

// dims is the length of the feature vector the weights expect.
func (w *Weights) dims() int { return len(CheapFeatureNames) + w.Dimensions }

// Validate checks that the weights are internally consistent and match
// this build's features, so that a bad file is refused at load time
// rather than producing nonsense, or a panic, on the first request.
func (w *Weights) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidWeights, fmt.Sprintf(format, args...))
	}
	if w.Version != WeightsVersion {
		return bad("version %d, want %d", w.Version, WeightsVersion)
	}
	if !slices.Equal(w.Features, CheapFeatureNames) {
		return bad("features %v do not match this build's %v; retrain", w.Features, CheapFeatureNames)
	}
	if w.Dimensions < 0 {
		return bad("dimensions %d", w.Dimensions)
	}
	if (w.Embedding == "") != (w.Dimensions == 0) {
		return bad("embedding %q with dimensions %d", w.Embedding, w.Dimensions)
	}
	d := w.dims()
	if len(w.Scaler.Mean) != d || len(w.Scaler.Std) != d {
		return bad("scaler has %d means and %d stds, want %d", len(w.Scaler.Mean), len(w.Scaler.Std), d)
	}
	for j := range d {
		if !finite(w.Scaler.Mean[j]) {
			return bad("scaler mean %d is %v", j, w.Scaler.Mean[j])
		}
		if !finite(w.Scaler.Std[j]) || w.Scaler.Std[j] <= 0 {
			return bad("scaler std %d is %v", j, w.Scaler.Std[j])
		}
	}
	if w.CostUnit != CostUSD && w.CostUnit != CostGFLOPs {
		return bad("cost unit %q, want %q or %q", w.CostUnit, CostUSD, CostGFLOPs)
	}
	if len(w.Candidates) == 0 {
		return bad("no candidates")
	}
	seen := make(map[string]bool, len(w.Candidates))
	for i, c := range w.Candidates {
		switch {
		case c.Model == "":
			return bad("candidate %d has no model", i)
		case seen[c.Model]:
			return bad("candidate %q listed twice", c.Model)
		case !finite(c.Cost) || c.Cost < 0:
			return bad("candidate %q cost %v", c.Model, c.Cost)
		case i > 0 && c.Cost < w.Candidates[i-1].Cost:
			return bad("candidates not sorted by cost: %q after %q", c.Model, w.Candidates[i-1].Model)
		case !finite(c.Accuracy) || c.Accuracy < 0 || c.Accuracy > 1:
			return bad("candidate %q accuracy %v", c.Model, c.Accuracy)
		case len(c.Logistic.Weights) != d:
			return bad("candidate %q has %d weights, want %d", c.Model, len(c.Logistic.Weights), d)
		case !finite(c.Logistic.Bias):
			return bad("candidate %q bias %v", c.Model, c.Logistic.Bias)
		}
		for j, v := range c.Logistic.Weights {
			if !finite(v) {
				return bad("candidate %q weight %d is %v", c.Model, j, v)
			}
		}
		seen[c.Model] = true
	}
	if !seen[w.Fallback] {
		return bad("fallback %q is not a candidate", w.Fallback)
	}
	if !finite(w.QualityTarget) || w.QualityTarget < 0 || w.QualityTarget > 1 {
		return bad("quality target %v not in [0, 1]", w.QualityTarget)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Load reads and validates a weights file.
func Load(path string) (*Weights, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("router: read weights: %w", err)
	}
	w, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return w, nil
}

// Parse decodes and validates weights. Unknown fields are refused: a
// field this build does not know may change what the file means.
func Parse(data []byte) (*Weights, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w Weights
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidWeights, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after the weights object", ErrInvalidWeights)
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

// Marshal encodes the weights as indented JSON with a trailing newline.
// The output is deterministic, and Parse then Marshal reproduces it byte
// for byte, so a committed file diffs cleanly. Invalid weights are
// refused, so nothing is written that Load would reject.
func (w *Weights) Marshal() ([]byte, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("router: encode weights: %w", err)
	}
	return append(data, '\n'), nil
}

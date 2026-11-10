package router

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
)

// testWeights builds valid weights with dims embedding dimensions and
// three candidates, cheapest first, whose predictions are controlled by
// their biases alone (all weights zero, identity scaler).
func testWeights(dims int, biases ...float64) *Weights {
	d := len(CheapFeatureNames) + dims
	s := Scaler{Mean: make([]float64, d), Std: make([]float64, d)}
	for j := range s.Std {
		s.Std[j] = 1
	}
	names := []string{"small", "medium", "large"}
	w := &Weights{
		Version:       WeightsVersion,
		Features:      slices.Clone(CheapFeatureNames),
		Scaler:        s,
		CostUnit:      CostUSD,
		Fallback:      "large",
		QualityTarget: 0.8,
		Source:        "test",
	}
	if dims > 0 {
		w.Embedding = embed.FakeModel
		w.Dimensions = dims
	}
	for i, b := range biases {
		w.Candidates = append(w.Candidates, Candidate{
			Model:    names[i],
			Cost:     float64(i + 1),
			Accuracy: 0.5 + 0.1*float64(i),
			Logistic: Logistic{Weights: make([]float64, d), Bias: b},
		})
	}
	return w
}

func logit(p float64) float64 { return math.Log(p / (1 - p)) }

func only(models ...string) func(string) bool {
	return func(m string) bool { return slices.Contains(models, m) }
}

func TestChoose(t *testing.T) {
	t.Parallel()
	// P(correct): small 0.6, medium 0.85, large 0.9.
	w := testWeights(0, logit(0.6), logit(0.85), logit(0.9))
	x := make([]float64, len(CheapFeatureNames))
	tests := []struct {
		name       string
		target     float64
		allowed    func(string) bool
		wantModel  string
		wantReason string
	}{
		{"cheapest above target", 0.8, nil, "medium", ReasonTarget},
		{"low target picks cheapest", 0.5, nil, "small", ReasonTarget},
		{"target 0 picks cheapest allowed", 0, only("medium", "large"), "medium", ReasonTarget},
		{"none clears: fallback", 0.95, nil, "large", ReasonFallback},
		{"target above 1: fallback", 1.5, nil, "large", ReasonFallback},
		{"allowed filter skips cheaper", 0.5, only("large"), "large", ReasonTarget},
		{"fallback not allowed: best allowed", 0.95, only("small", "medium"), "medium", ReasonBestAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, err := w.Choose(x, tt.target, tt.allowed)
			if err != nil {
				t.Fatal(err)
			}
			if d.Model != tt.wantModel || d.Reason != tt.wantReason {
				t.Errorf("got %s (%s), want %s (%s)", d.Model, d.Reason, tt.wantModel, tt.wantReason)
			}
			if len(d.Probs) != 3 || math.Abs(d.Probs[1]-0.85) > 1e-12 {
				t.Errorf("probs %v", d.Probs)
			}
		})
	}

	t.Run("nothing allowed", func(t *testing.T) {
		t.Parallel()
		if _, err := w.Choose(x, 0.5, only("other")); !errors.Is(err, ErrNoCandidate) {
			t.Errorf("err = %v, want ErrNoCandidate", err)
		}
	})
	t.Run("wrong length", func(t *testing.T) {
		t.Parallel()
		if _, err := w.Choose(x[1:], 0.5, nil); !errors.Is(err, ErrInvalidWeights) {
			t.Errorf("err = %v, want ErrInvalidWeights", err)
		}
	})
}

func TestChooseUsesScalerAndFeatures(t *testing.T) {
	t.Parallel()
	// small's P(correct) falls with log_chars: short prompts go to small,
	// long ones to large.
	w := testWeights(0, 0, logit(0.7), logit(0.9))
	i := slices.Index(CheapFeatureNames, "log_chars")
	w.Scaler.Mean[i], w.Scaler.Std[i] = 3, 2
	w.Candidates[0].Logistic.Weights[i] = -3
	short, long := Cheap("hi"), Cheap(strings.Repeat("word ", 500))
	if d, _ := w.Choose(short, 0.8, nil); d.Model != "small" {
		t.Errorf("short prompt: %s %v", d.Model, d.Probs)
	}
	if d, _ := w.Choose(long, 0.8, nil); d.Model != "large" {
		t.Errorf("long prompt: %s %v", d.Model, d.Probs)
	}
}

type failingEmbedder struct{ err error }

func (f failingEmbedder) Model() string { return embed.FakeModel }
func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}

type wrongDimsEmbedder struct{}

func (wrongDimsEmbedder) Model() string { return embed.FakeModel }
func (wrongDimsEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}

type namedEmbedder struct{ embed.Fake }

func (*namedEmbedder) Model() string { return "other-model" }

func request(text string) *core.Request {
	return &core.Request{Model: "auto", System: "sys", Messages: []core.Message{{Role: core.RoleUser, Text: text}}}
}

func TestRoute(t *testing.T) {
	t.Parallel()
	const dims = 64
	// medium's P(correct) rises with embedding dimension 0, set to the
	// bucket the fake hashes "alpha" into, so prompts made of "alpha" go to
	// medium and others fall back to large.
	vec, err := (&embed.Fake{Dims: dims}).Embed(context.Background(), []string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	bucket := slices.Index(vec[0], 1)
	w := testWeights(dims, logit(0.1), logit(0.1), logit(0.9))
	w.Candidates[1].Logistic.Weights[len(CheapFeatureNames)+bucket] = 10

	r, err := New(w, &embed.Fake{Dims: dims}, Options{Target: -1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Target() != 0.8 || r.Weights() != w {
		t.Errorf("target %v", r.Target())
	}
	ctx := context.Background()
	d, err := r.Route(ctx, request("alpha"), nil)
	if err != nil || d.Model != "medium" || d.Reason != ReasonTarget || d.Err != nil {
		t.Errorf("alpha: %+v, %v", d, err)
	}
	const other = "beta"
	if v, _ := (&embed.Fake{Dims: dims}).Embed(ctx, []string{other}); v[0][bucket] != 0 {
		t.Fatalf("%q shares a bucket with alpha; pick other words", other)
	}
	d, err = r.Route(ctx, request(other), nil)
	if err != nil || d.Model != "large" || d.Reason != ReasonTarget {
		t.Errorf("other: %+v, %v", d, err)
	}
	if _, err := r.Route(ctx, request("alpha"), only("none")); !errors.Is(err, ErrNoCandidate) {
		t.Errorf("nothing allowed: %v", err)
	}

	down := errors.New("connection refused")
	tests := []struct {
		name      string
		e         embed.Embedder
		allowed   func(string) bool
		wantModel string
		wantErr   error
	}{
		{"embedder down", failingEmbedder{down}, nil, "large", down},
		{"embedder down, fallback not allowed", failingEmbedder{down}, only("small", "medium"), "medium", down},
		{"wrong dimensions", wrongDimsEmbedder{}, nil, "large", embed.ErrDimensions},
		{"fake with error", &embed.Fake{Dims: dims, Err: down}, nil, "large", down},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, err := New(w, tt.e, Options{Target: 0.5})
			if err != nil {
				t.Fatal(err)
			}
			d, err := r.Route(ctx, request("alpha"), tt.allowed)
			if err != nil {
				t.Fatal(err)
			}
			if d.Model != tt.wantModel || d.Reason != ReasonNoEmbedding || d.Probs != nil {
				t.Errorf("got %+v", d)
			}
			if !errors.Is(d.Err, tt.wantErr) {
				t.Errorf("Err = %v, want %v", d.Err, tt.wantErr)
			}
		})
	}
}

func TestRouteCheapOnly(t *testing.T) {
	t.Parallel()
	r, err := New(testWeights(0, logit(0.9), logit(0.95), logit(0.99)), nil, Options{Target: 0.85})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.Route(context.Background(), request("2+2?"), nil)
	if err != nil || d.Model != "small" || d.Reason != ReasonTarget {
		t.Errorf("%+v, %v", d, err)
	}
}

func TestNewErrors(t *testing.T) {
	t.Parallel()
	w := testWeights(8, 0, 0, 0)
	bad := testWeights(8, 0, 0, 0)
	bad.Version = 99
	tests := []struct {
		name string
		w    *Weights
		e    embed.Embedder
		opt  Options
	}{
		{"nil weights", nil, &embed.Fake{}, Options{}},
		{"invalid weights", bad, &embed.Fake{}, Options{}},
		{"no embedder", w, nil, Options{}},
		{"wrong embedding model", w, &namedEmbedder{}, Options{}},
		{"NaN target", w, &embed.Fake{}, Options{Target: math.NaN()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tt.w, tt.e, tt.opt); err == nil {
				t.Error("New succeeded")
			}
		})
	}
}

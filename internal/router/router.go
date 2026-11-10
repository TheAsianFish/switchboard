package router

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
)

// Reasons a Decision gives for its model.
const (
	// ReasonTarget: the cheapest allowed candidate whose P(correct)
	// reached the target.
	ReasonTarget = "target"
	// ReasonFallback: no allowed candidate reached the target, so the
	// Fallback was chosen.
	ReasonFallback = "fallback"
	// ReasonBestAllowed: no allowed candidate reached the target and the
	// Fallback is not allowed, so the allowed candidate with the highest
	// P(correct) was chosen.
	ReasonBestAllowed = "best_allowed"
	// ReasonNoEmbedding: the prompt could not be embedded, so the router
	// chose without predicting: the Fallback, or the allowed candidate
	// with the best training accuracy.
	ReasonNoEmbedding = "no_embedding"
)

// ErrNoCandidate is returned when the allowed filter rejects every
// candidate.
var ErrNoCandidate = errors.New("router: no candidate model is allowed")

// Decision is the router's choice for one request.
type Decision struct {
	Model string
	// Probs is P(correct) per candidate, in Weights.Candidates order; nil
	// when the router chose without predicting.
	Probs []float64
	// Reason is one of the Reason constants.
	Reason string
	// Err is why the router could not predict (the embedding failed) when
	// Reason is ReasonNoEmbedding, for the caller to log. The request
	// still goes ahead with Model.
	Err error
}

// Choose picks a model for the raw (unscaled) feature vector x: the
// cheapest allowed candidate whose P(correct) is at least target, else
// the Fallback, else the allowed candidate with the highest P(correct).
// So target 0 picks the cheapest allowed candidate and a target above 1
// always picks the Fallback. allowed may be nil, meaning every candidate
// is allowed.
//
// It returns ErrNoCandidate when nothing is allowed, and an
// ErrInvalidWeights error when x has the wrong length.
func (w *Weights) Choose(x []float64, target float64, allowed func(model string) bool) (Decision, error) {
	if len(x) != w.dims() {
		return Decision{}, fmt.Errorf("%w: feature vector has %d values, want %d", ErrInvalidWeights, len(x), w.dims())
	}
	if !w.anyAllowed(allowed) {
		return Decision{}, ErrNoCandidate
	}
	xs := w.Scaler.Apply(x)
	probs := make([]float64, len(w.Candidates))
	for i, c := range w.Candidates {
		probs[i] = c.Logistic.Prob(xs)
	}
	for i, c := range w.Candidates {
		if isAllowed(allowed, c.Model) && probs[i] >= target {
			return Decision{Model: c.Model, Probs: probs, Reason: ReasonTarget}, nil
		}
	}
	if isAllowed(allowed, w.Fallback) {
		return Decision{Model: w.Fallback, Probs: probs, Reason: ReasonFallback}, nil
	}
	best := w.bestAllowed(allowed, probs)
	return Decision{Model: w.Candidates[best].Model, Probs: probs, Reason: ReasonBestAllowed}, nil
}

// fallbackWithout chooses without a prediction: the Fallback if allowed,
// else the allowed candidate with the best training accuracy.
func (w *Weights) fallbackWithout(allowed func(string) bool, cause error) (Decision, error) {
	if !w.anyAllowed(allowed) {
		return Decision{}, ErrNoCandidate
	}
	model := w.Fallback
	if !isAllowed(allowed, model) {
		acc := make([]float64, len(w.Candidates))
		for i, c := range w.Candidates {
			acc[i] = c.Accuracy
		}
		model = w.Candidates[w.bestAllowed(allowed, acc)].Model
	}
	return Decision{Model: model, Reason: ReasonNoEmbedding, Err: cause}, nil
}

// bestAllowed is the index of the allowed candidate with the highest
// score; ties go to the cheaper one. At least one must be allowed.
func (w *Weights) bestAllowed(allowed func(string) bool, score []float64) int {
	best := -1
	for i, c := range w.Candidates {
		if isAllowed(allowed, c.Model) && (best < 0 || score[i] > score[best]) {
			best = i
		}
	}
	return best
}

func (w *Weights) anyAllowed(allowed func(string) bool) bool {
	for _, c := range w.Candidates {
		if isAllowed(allowed, c.Model) {
			return true
		}
	}
	return false
}

func isAllowed(allowed func(string) bool, model string) bool {
	return allowed == nil || allowed(model)
}

// DefaultTimeout bounds the embedding call when Options.Timeout is 0.
const DefaultTimeout = 2 * time.Second

// Options configures a Router.
type Options struct {
	// Target is the P(correct) a candidate must reach; negative means
	// Weights.QualityTarget.
	Target float64
	// Timeout bounds the embedding call; 0 means DefaultTimeout. The
	// router is on every auto request's critical path, so a slow embedder
	// must cost a fallback, not the request.
	Timeout time.Duration
}

// Router chooses models for requests at serving time. It is safe for
// concurrent use if its embedder is.
type Router struct {
	w       *Weights
	e       embed.Embedder
	target  float64
	timeout time.Duration
}

// New validates w and makes a router from it and the embedder it was
// trained with. e may be nil only for cheap-only weights; an
// embedder for a different model is refused, since its vectors would
// mean nothing to the regressions.
func New(w *Weights, e embed.Embedder, opt Options) (*Router, error) {
	if w == nil {
		return nil, fmt.Errorf("%w: nil weights", ErrInvalidWeights)
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	if w.Embedding != "" {
		if e == nil {
			return nil, fmt.Errorf("router: weights need embedding model %q but no embedder was given", w.Embedding)
		}
		if e.Model() != w.Embedding {
			return nil, fmt.Errorf("router: weights were trained on embedding model %q, embedder is %q", w.Embedding, e.Model())
		}
	}
	target := opt.Target
	if target < 0 {
		target = w.QualityTarget
	}
	if math.IsNaN(target) {
		return nil, errors.New("router: target is NaN")
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Router{w: w, e: e, target: target, timeout: timeout}, nil
}

// Weights returns the router's weights. Callers must not modify them.
func (r *Router) Weights() *Weights { return r.w }

// Target returns the quality target in use.
func (r *Router) Target() float64 { return r.target }

// Route chooses a model for req among the allowed candidates (nil allows
// all). It embeds PromptText(req) when the weights use an embedding. If
// the embedding fails or has the wrong length, Route does not fail the
// request: it returns a ReasonNoEmbedding decision with the cause in
// Decision.Err. The only error is ErrNoCandidate.
func (r *Router) Route(ctx context.Context, req *core.Request, allowed func(string) bool) (Decision, error) {
	text := PromptText(req)
	var emb []float32
	if r.w.Embedding != "" {
		var err error
		emb, err = r.embed(ctx, text)
		if err != nil {
			return r.w.fallbackWithout(allowed, err)
		}
	}
	return r.w.Choose(Vector(Cheap(text), emb), r.target, allowed)
}

func (r *Router) embed(ctx context.Context, text string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	vecs, err := r.e.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("router: embed prompt: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != r.w.Dimensions {
		got := 0
		if len(vecs) > 0 {
			got = len(vecs[0])
		}
		return nil, fmt.Errorf("router: embed prompt: %w: got %d vectors of length %d, want 1 of %d",
			embed.ErrDimensions, len(vecs), got, r.w.Dimensions)
	}
	for _, v := range vecs[0] {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("router: embed prompt: %w: non-finite value", embed.ErrDimensions)
		}
	}
	return vecs[0], nil
}

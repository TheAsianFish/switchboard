// Package embed turns text into vectors for the semantic cache. The
// gateway talks to Ollama's /api/embed; tests use the deterministic fake.
package embed

import (
	"context"
	"errors"
	"math"
)

// Embedder embeds texts. Implementations return one unit-length vector
// per input, in order, all of the same length.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model names the embedding model. Vectors from different models are
	// never compared.
	Model() string
}

// ErrDimensions is returned when an upstream reply does not have one
// vector per input or its vectors differ in length.
var ErrDimensions = errors.New("embed: malformed embeddings")

// ErrTooLong is returned for an input longer than the model's context.
// It says nothing about the embedder's health.
var ErrTooLong = errors.New("embed: input longer than the model's context")

// Normalise scales v to unit length in place and returns it. A zero
// vector is left as it is.
func Normalise(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := 1 / math.Sqrt(sum)
	for i := range v {
		v[i] = float32(float64(v[i]) * inv)
	}
	return v
}

// Cosine is the cosine similarity of a and b, which must have the same
// length. For unit vectors it is their dot product.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

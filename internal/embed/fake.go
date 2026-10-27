package embed

import (
	"context"
	"hash/fnv"
	"strings"
	"sync"
	"unicode"
)

// Fake is a deterministic embedder for tests: each lower-cased word is
// hashed into one of Dims buckets, so texts with the same words have
// similarity 1 and texts sharing no words are near 0. Word order is
// ignored, like the paraphrase-blind failure mode Q3 measures.
type Fake struct {
	Dims int
	// Err, when set, is returned by every call.
	Err error

	mu    sync.Mutex
	calls int
	texts int
}

var _ Embedder = (*Fake)(nil)

// FakeModel is the model name the fake reports.
const FakeModel = "fake-embed"

// Model implements Embedder.
func (f *Fake) Model() string { return FakeModel }

// Embed implements Embedder.
func (f *Fake) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	f.texts += len(texts)
	err := f.Err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dims := f.Dims
	if dims <= 0 {
		dims = 64
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dims)
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[h.Sum32()%uint32(dims)]++
		}
		out[i] = Normalise(v)
	}
	return out, nil
}

// Calls reports how many Embed calls were made and how many texts they
// carried.
func (f *Fake) Calls() (calls, texts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.texts
}

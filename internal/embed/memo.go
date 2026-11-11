package embed

import (
	"context"
	"slices"
	"sync"
)

// memo holds the vectors already computed for one request.
type memo struct {
	mu   sync.Mutex
	vecs map[string][]float32
}

type memoKey struct{}

// WithMemo returns a context in which Memoized embedders share their
// single-text results. The pipeline gives each request one, so an auto
// request whose router and semantic cache embed the same prompt with the
// same model pays for one embedding call, not two.
func WithMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoKey{}, &memo{vecs: map[string][]float32{}})
}

// Memoized wraps an embedder so that single-text calls made with a
// WithMemo context are answered from, and stored in, that context's memo.
// The memo is keyed by model and exact text, so a hit returns exactly what
// the call would have; each caller gets its own copy of the vector.
func Memoized(e Embedder) Embedder { return memoized{e} }

type memoized struct{ Embedder }

func (m memoized) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	mm, _ := ctx.Value(memoKey{}).(*memo)
	if mm == nil || len(texts) != 1 {
		return m.Embedder.Embed(ctx, texts)
	}
	key := m.Model() + "\x00" + texts[0]
	mm.mu.Lock()
	v, ok := mm.vecs[key]
	mm.mu.Unlock()
	if ok {
		return [][]float32{slices.Clone(v)}, nil
	}
	vecs, err := m.Embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) == 1 {
		mm.mu.Lock()
		mm.vecs[key] = slices.Clone(vecs[0])
		mm.mu.Unlock()
	}
	return vecs, nil
}

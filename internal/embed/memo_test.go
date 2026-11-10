package embed

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// counting counts the calls that reach the embedder.
type counting struct {
	Fake
	calls atomic.Int64
	fail  bool
}

func (c *counting) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls.Add(1)
	if c.fail {
		return nil, errors.New("down")
	}
	return c.Fake.Embed(ctx, texts)
}

func TestMemoizedSharesWithinARequest(t *testing.T) {
	t.Parallel()
	inner := &counting{Fake: Fake{Dims: 8}}
	a, b := Memoized(inner), Memoized(inner)
	ctx := WithMemo(context.Background())

	v1, err := a.Embed(ctx, []string{"what is 2 + 2?"})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := b.Embed(ctx, []string{"what is 2 + 2?"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 for the same text in one request", inner.calls.Load())
	}
	for i := range v1[0] {
		if v1[0][i] != v2[0][i] {
			t.Fatalf("memoised vector differs at %d", i)
		}
	}
	v2[0][0] = 42 // a caller changing its copy must not reach the other's
	if v1[0][0] == 42 {
		t.Fatal("callers share one slice")
	}
	if v3, _ := a.Embed(ctx, []string{"what is 2 + 2?"}); v3[0][0] == 42 {
		t.Fatal("the memo was changed through a caller's copy")
	}

	// A different text, a fresh request, a batch, or no memo at all each
	// reach the embedder.
	_, _ = a.Embed(ctx, []string{"what is 2 + 3?"})
	_, _ = a.Embed(WithMemo(context.Background()), []string{"what is 2 + 2?"})
	_, _ = a.Embed(ctx, []string{"x", "y"})
	_, _ = a.Embed(context.Background(), []string{"what is 2 + 2?"})
	if got := inner.calls.Load(); got != 5 {
		t.Fatalf("calls = %d, want 5", got)
	}
}

func TestMemoizedDoesNotStoreFailures(t *testing.T) {
	t.Parallel()
	inner := &counting{Fake: Fake{Dims: 8}, fail: true}
	m := Memoized(inner)
	ctx := WithMemo(context.Background())
	for range 2 {
		if _, err := m.Embed(ctx, []string{"hi"}); err == nil {
			t.Fatal("failure was hidden")
		}
	}
	if inner.calls.Load() != 2 {
		t.Fatalf("calls = %d, want a retry after a failure", inner.calls.Load())
	}
}

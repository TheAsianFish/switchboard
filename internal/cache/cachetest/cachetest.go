// Package cachetest is the contract every cache.Index must meet. The
// in-memory index runs it in the unit tests and the Redis index in the
// integration tests, so the two cannot drift apart.
package cachetest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/embed"
)

// Subject is one fresh, empty index under test and the means to make
// its entries expire.
type Subject struct {
	Index cache.Index
	// ShortTTL is the lifetime of the entry the expiry test adds. It must
	// be long enough for an Add and a Search to finish inside it.
	ShortTTL time.Duration
	// Advance moves the index's time on by d: an injected clock moves, a
	// real server is waited for.
	Advance func(d time.Duration)
	// Wait bounds how long an added entry may take to become visible and
	// an expired one to disappear; 0 means at once. Redis 8 inserts into
	// HNSW on background workers, so a fresh entry can be missing from a
	// search for a few milliseconds, and it drops expired keys lazily.
	Wait time.Duration
}

// Clock is a manual clock for indexes that take a now function.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock at an arbitrary fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock on by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Dims is the vector length the suite asks for.
const Dims = 8

// longTTL outlives every test.
const longTTL = time.Hour

// Index runs the contract against indexes made by newSubject, which is
// called once per subtest with Dims and must return an empty index.
func Index(t *testing.T, newSubject func(t *testing.T, dims int) Subject) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, s Subject)
	}{
		{"Empty", testEmpty},
		{"NamespaceIsolation", testNamespaceIsolation},
		{"ContextIsolation", testContextIsolation},
		{"Ordering", testOrdering},
		{"KBound", testKBound},
		{"ReplaceByID", testReplace},
		{"Expiry", testExpiry},
		{"Dimensions", testDimensions},
		{"Payload", testPayload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, newSubject(t, Dims))
		})
	}
}

var nsSeq atomic.Int64

// fresh returns a namespace no other subtest uses, so subtests sharing a
// server cannot see each other's entries even if an index were shared.
func fresh() string { return fmt.Sprintf("ns%x", nsSeq.Add(1)) }

// vec returns the unit vector cos(θ)·e0 + sin(θ)·e1, whose similarity to
// e0 is cos(θ).
func vec(theta float64) []float32 {
	v := make([]float32, Dims)
	v[0] = float32(math.Cos(theta))
	v[1] = float32(math.Sin(theta))
	return v
}

// basis returns the unit vector along axis i.
func basis(i int) []float32 {
	v := make([]float32, Dims)
	v[i] = 1
	return v
}

func add(t *testing.T, s Subject, it cache.Item) {
	t.Helper()
	if it.TTL == 0 {
		it.TTL = longTTL
	}
	if err := s.Index.Add(context.Background(), it); err != nil {
		t.Fatalf("Add(%s): %v", it.ID, err)
	}
	// Wait until the entry is visible in its own scope, so a later
	// search that does not find it proves isolation, not a lag.
	q := cache.Query{Namespace: it.Namespace, Context: it.Context, Vector: it.Vector, K: 100}
	eventually(t, s, q, func(ms []cache.Match) bool {
		for _, m := range ms {
			if m.ID == it.ID && string(m.Payload) == string(it.Payload) {
				return true
			}
		}
		return false
	}, "Add(%s) never became visible", it.ID)
}

// eventually searches with q until ok accepts the result, which it
// returns, or s.Wait has passed. Positive expectations go through it:
// besides the lag after an Add, Redis's tiered HNSW index can drop a
// fresh entry from a result now and then while it moves the entry into
// the graph. Expectations that something is absent do not, so a lag can
// never hide a leak.
func eventually(t *testing.T, s Subject, q cache.Query, ok func([]cache.Match) bool, format string, args ...any) []cache.Match {
	t.Helper()
	deadline := time.Now().Add(s.Wait)
	for {
		ms := search(t, s, q)
		if ok(ms) {
			return ms
		}
		if time.Now().After(deadline) {
			t.Fatalf(format+": last result %v", append(args, ids(ms))...)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func search(t *testing.T, s Subject, q cache.Query) []cache.Match {
	t.Helper()
	ms, err := s.Index.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return ms
}

func ids(ms []cache.Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func sameIDs(got []cache.Match, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].ID != want[i] {
			return false
		}
	}
	return true
}

func testEmpty(t *testing.T, s Subject) {
	ns := fresh()
	if ms := search(t, s, cache.Query{Namespace: ns, Context: "c", Vector: basis(0), K: 5}); len(ms) != 0 {
		t.Fatalf("empty index returned %v", ids(ms))
	}
	add(t, s, cache.Item{ID: "x", Namespace: ns, Context: "c", Vector: basis(0)})
	for _, k := range []int{0, -1} {
		if ms := search(t, s, cache.Query{Namespace: ns, Context: "c", Vector: basis(0), K: k}); len(ms) != 0 {
			t.Fatalf("K=%d returned %v", k, ids(ms))
		}
	}
}

// pairs are values that must never be confused. Several would meet in a
// naive Redis TAG index: TAG matching folds case, splits on commas and
// skips empty values, and unescaped punctuation is query syntax.
var pairs = [][2]string{
	{"aaaa", "bbbb"},
	{"abc", "ABC"},
	{"a", "a,b"},
	{"", "x"},
	{"a b", "a"},
	{"a|b", "b"},
	{"*", "a"},
	{"}", "{"},
	{"~61", "a"},   // the escaped form of "a" must not collide with a literal
	{"0x1f", "1f"}, // hex-looking values
}

// testNamespaceIsolation is the cross-tenant leak test: an entry with the
// same context and the same vector in another namespace never matches.
func testNamespaceIsolation(t *testing.T, s Subject) {
	for _, p := range pairs {
		// A fresh context per pair keeps values shared between pairs apart.
		ctx := fresh()
		checkApart(t, s, func(v string) (string, string) { return v, ctx }, p[0], p[1])
	}
}

// testContextIsolation is the cross-context leak test: an entry in the
// same namespace under another context never matches.
func testContextIsolation(t *testing.T, s Subject) {
	for _, p := range pairs {
		ns := fresh()
		checkApart(t, s, func(v string) (string, string) { return ns, v }, p[0], p[1])
	}
}

// checkApart adds one entry under a and one under b (scope maps a value
// to a namespace and context) and checks each search finds its own only.
func checkApart(t *testing.T, s Subject, scope func(string) (string, string), a, b string) {
	t.Helper()
	nsA, ctxA := scope(a)
	nsB, ctxB := scope(b)
	add(t, s, cache.Item{ID: "a-" + fmt.Sprint(nsSeq.Add(1)), Namespace: nsA, Context: ctxA, Vector: basis(0), Payload: []byte("A")})
	q := cache.Query{Namespace: nsB, Context: ctxB, Vector: basis(0), K: 10}
	if ms := search(t, s, q); len(ms) != 0 {
		t.Fatalf("search in %q/%q found %q/%q's entry: %v", nsB, ctxB, nsA, ctxA, ids(ms))
	}
	add(t, s, cache.Item{ID: "b-" + fmt.Sprint(nsSeq.Add(1)), Namespace: nsB, Context: ctxB, Vector: basis(0), Payload: []byte("B")})
	for _, c := range []struct {
		ns, ctx, want string
	}{{nsA, ctxA, "A"}, {nsB, ctxB, "B"}} {
		q := cache.Query{Namespace: c.ns, Context: c.ctx, Vector: basis(0), K: 10}
		// Wait for the entry itself, then demand nothing else is there.
		ms := eventually(t, s, q, func(ms []cache.Match) bool { return len(ms) > 0 }, "search in %q/%q found nothing", c.ns, c.ctx)
		if len(ms) != 1 || string(ms[0].Payload) != c.want {
			t.Fatalf("search in %q/%q: got %d matches %v, want only %s's entry", c.ns, c.ctx, len(ms), ids(ms), c.want)
		}
	}
}

func testOrdering(t *testing.T, s Subject) {
	ns := fresh()
	// Added out of order; similarity to e0 is cos(θ).
	angles := map[string]float64{"c": 1.2, "a": 0.1, "d": math.Pi / 2, "b": 0.5}
	for _, id := range []string{"c", "a", "d", "b"} {
		add(t, s, cache.Item{ID: id, Namespace: ns, Context: "c", Vector: vec(angles[id])})
	}
	q := cache.Query{Namespace: ns, Context: "c", Vector: basis(0), K: 4}
	ms := eventually(t, s, q, func(ms []cache.Match) bool { return len(ms) == 4 }, "want 4 matches")
	if !sameIDs(ms, "a", "b", "c", "d") {
		t.Fatalf("order = %v, want [a b c d]", ids(ms))
	}
	for _, m := range ms {
		if want := math.Cos(angles[m.ID]); math.Abs(m.Similarity-want) > 1e-4 {
			t.Errorf("%s: similarity %.6f, want %.6f", m.ID, m.Similarity, want)
		}
	}
}

func testKBound(t *testing.T, s Subject) {
	ns := fresh()
	for i := range 6 {
		add(t, s, cache.Item{ID: fmt.Sprint(i), Namespace: ns, Context: "c", Vector: vec(0.2 * float64(i))})
	}
	for _, k := range []int{1, 2, 5, 50} {
		q := cache.Query{Namespace: ns, Context: "c", Vector: basis(0), K: k}
		want := []string{"0", "1", "2", "3", "4", "5"}[:min(k, 6)]
		ms := eventually(t, s, q, func(ms []cache.Match) bool { return len(ms) >= len(want) }, "K=%d: want %d matches", k, len(want))
		if !sameIDs(ms, want...) {
			t.Errorf("K=%d: got %v, want %v", k, ids(ms), want)
		}
	}
}

func testReplace(t *testing.T, s Subject) {
	ns := fresh()
	add(t, s, cache.Item{ID: "x", Namespace: ns, Context: "c", Vector: basis(0), Payload: []byte("old")})
	add(t, s, cache.Item{ID: "x", Namespace: ns, Context: "c", Vector: basis(1), Payload: []byte("new")})
	q := cache.Query{Namespace: ns, Context: "c", Vector: basis(1), K: 10}
	ms := eventually(t, s, q, func(ms []cache.Match) bool { return len(ms) > 0 }, "replaced entry not found")
	if len(ms) != 1 || ms[0].ID != "x" || string(ms[0].Payload) != "new" || ms[0].Similarity < 0.999 {
		t.Fatalf("after replace: %+v, want one match x with the new vector and payload", ms)
	}
}

func testExpiry(t *testing.T, s Subject) {
	ns := fresh()
	add(t, s, cache.Item{ID: "short", Namespace: ns, Context: "c", Vector: basis(0), TTL: s.ShortTTL})
	add(t, s, cache.Item{ID: "long", Namespace: ns, Context: "c", Vector: vec(0.3)})
	q := cache.Query{Namespace: ns, Context: "c", Vector: basis(0), K: 10}
	eventually(t, s, q, func(ms []cache.Match) bool { return sameIDs(ms, "short", "long") }, "before expiry: want [short long]")
	s.Advance(s.ShortTTL)
	eventually(t, s, q, func(ms []cache.Match) bool { return sameIDs(ms, "long") }, "after expiry: want [long]")
}

func testDimensions(t *testing.T, s Subject) {
	ctx := context.Background()
	ns := fresh()
	for _, n := range []int{0, Dims - 1, Dims + 1} {
		err := s.Index.Add(ctx, cache.Item{ID: "x", Namespace: ns, Context: "c", Vector: make([]float32, n), TTL: longTTL})
		if !errors.Is(err, cache.ErrDimensions) {
			t.Errorf("Add with %d dims: err = %v, want ErrDimensions", n, err)
		}
		_, err = s.Index.Search(ctx, cache.Query{Namespace: ns, Context: "c", Vector: make([]float32, n), K: 1})
		if !errors.Is(err, cache.ErrDimensions) {
			t.Errorf("Search with %d dims: err = %v, want ErrDimensions", n, err)
		}
	}
}

// testPayload checks payloads round-trip byte for byte, binary included.
func testPayload(t *testing.T, s Subject) {
	ns := fresh()
	payload := []byte{0, 1, 2, 0xff, '\n', '"', 0x80}
	add(t, s, cache.Item{ID: "bin", Namespace: ns, Context: "c", Vector: embed.Normalise([]float32{1, 1, 1, 1, 1, 1, 1, 1}), Payload: payload})
	q := cache.Query{Namespace: ns, Context: "c", Vector: basis(0), K: 1}
	ms := eventually(t, s, q, func(ms []cache.Match) bool { return len(ms) > 0 }, "entry not found")
	if len(ms) != 1 || string(ms[0].Payload) != string(payload) {
		t.Fatalf("payload = %+v, want %v", ms, payload)
	}
}

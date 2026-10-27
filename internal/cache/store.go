package cache

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/TheAsianFish/switchboard/internal/embed"
)

// KV stores exact-match entries under their full key.
type KV interface {
	// Get returns the value stored under key, and false when there is none
	// or it has expired.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores val under key for ttl.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}

// Item is one semantic entry in a vector index.
type Item struct {
	// ID is unique per entry; adding an existing ID replaces it.
	ID string
	// Namespace is the tenant's hash and Context the context hash; a
	// search only ever sees items with both equal to its own.
	Namespace string
	Context   string
	// Vector is the unit-length embedding of the prompt's last user turn.
	Vector  []float32
	Payload []byte
	TTL     time.Duration
}

// Query asks for the K items nearest to Vector within one namespace and
// context.
type Query struct {
	Namespace string
	Context   string
	Vector    []float32
	K         int
}

// Match is one search result.
type Match struct {
	ID string
	// Similarity is the cosine similarity to the query vector.
	Similarity float64
	Payload    []byte
}

// Index is a vector index of semantic entries.
type Index interface {
	Add(ctx context.Context, it Item) error
	// Search returns at most q.K matches, most similar first, drawn only
	// from items whose Namespace and Context equal the query's.
	Search(ctx context.Context, q Query) ([]Match, error)
}

// ErrDimensions is returned for a vector whose length does not match the
// index.
var ErrDimensions = errors.New("cache: vector has the wrong number of dimensions")

// MemoryKV is a per-process KV with expiry and a size cap. When full it
// drops the entry closest to expiry. It is the store when no Redis is
// configured, and the unit tests' store.
type MemoryKV struct {
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[string]memEntry
}

type memEntry struct {
	val     []byte
	expires time.Time
}

// NewMemoryKV returns a KV holding at most max entries.
func NewMemoryKV(max int, now func() time.Time) *MemoryKV {
	return &MemoryKV{max: max, now: now, entries: map[string]memEntry{}}
}

var _ KV = (*MemoryKV)(nil)

// Get implements KV.
func (m *MemoryKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, false, nil
	}
	if !m.now().Before(e.expires) {
		delete(m.entries, key)
		return nil, false, nil
	}
	return e.val, true, nil
}

// Set implements KV.
func (m *MemoryKV) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; !ok && len(m.entries) >= m.max {
		evictSoonest(m.entries, m.now())
	}
	m.entries[key] = memEntry{val: val, expires: m.now().Add(ttl)}
	return nil
}

// evictSoonest drops expired entries, or failing that the one that would
// expire first. It is linear in the map's size, which only matters when
// the cache is full, and then only once per insert.
func evictSoonest(entries map[string]memEntry, now time.Time) {
	var victim string
	var soonest time.Time
	dropped := false
	for k, e := range entries {
		if !now.Before(e.expires) {
			delete(entries, k)
			dropped = true
			continue
		}
		if victim == "" || e.expires.Before(soonest) {
			victim, soonest = k, e.expires
		}
	}
	// Only evict a live entry when dropping the expired ones made no room.
	if !dropped && victim != "" {
		delete(entries, victim)
	}
}

// MemoryIndex is an exact (FLAT) vector index in process memory: a search
// compares the query with every item in its namespace and context. It is
// the index when no Redis is configured, and the reference the Redis
// index is tested against.
type MemoryIndex struct {
	dims int
	max  int
	now  func() time.Time

	mu    sync.Mutex
	size  int
	byCtx map[string]map[string]memItem // namespace+context → id → item
}

type memItem struct {
	vec     []float32
	payload []byte
	expires time.Time
}

// NewMemoryIndex returns an index of dims-dimensional vectors holding at
// most max items.
func NewMemoryIndex(dims, max int, now func() time.Time) *MemoryIndex {
	return &MemoryIndex{dims: dims, max: max, now: now, byCtx: map[string]map[string]memItem{}}
}

var _ Index = (*MemoryIndex)(nil)

func scope(namespace, context string) string { return namespace + "\x00" + context }

// Add implements Index.
func (m *MemoryIndex) Add(_ context.Context, it Item) error {
	if len(it.Vector) != m.dims {
		return ErrDimensions
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := scope(it.Namespace, it.Context)
	items := m.byCtx[s]
	if items == nil {
		items = map[string]memItem{}
		m.byCtx[s] = items
	}
	if _, ok := items[it.ID]; !ok {
		if m.size >= m.max {
			m.evict(now)
		}
		m.size++
	}
	items[it.ID] = memItem{vec: it.Vector, payload: it.Payload, expires: now.Add(it.TTL)}
	return nil
}

// evict drops expired items, or failing that the one expiring soonest.
func (m *MemoryIndex) evict(now time.Time) {
	var vs, vid string
	var soonest time.Time
	for s, items := range m.byCtx {
		for id, it := range items {
			if !now.Before(it.expires) {
				delete(items, id)
				m.size--
				continue
			}
			if vid == "" || it.expires.Before(soonest) {
				vs, vid, soonest = s, id, it.expires
			}
		}
		if len(items) == 0 {
			delete(m.byCtx, s)
		}
	}
	if m.size >= m.max && vid != "" {
		delete(m.byCtx[vs], vid)
		if len(m.byCtx[vs]) == 0 {
			delete(m.byCtx, vs)
		}
		m.size--
	}
}

// Search implements Index.
func (m *MemoryIndex) Search(_ context.Context, q Query) ([]Match, error) {
	if len(q.Vector) != m.dims {
		return nil, ErrDimensions
	}
	if q.K <= 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var out []Match
	for id, it := range m.byCtx[scope(q.Namespace, q.Context)] {
		if !now.Before(it.expires) {
			continue
		}
		out = append(out, Match{ID: id, Similarity: embed.Cosine(q.Vector, it.vec), Payload: it.payload})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > q.K {
		out = out[:q.K]
	}
	return out, nil
}

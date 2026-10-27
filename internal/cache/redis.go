package cache

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisKV is a KV in Redis, shared by every replica. Keys are stored as
// prefix+key; the caller builds the key.
type RedisKV struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisKV returns a KV storing under prefix (the gateway's global key
// prefix, for example "sb:").
func NewRedisKV(rdb *redis.Client, prefix string) *RedisKV {
	return &RedisKV{rdb: rdb, prefix: prefix}
}

var _ KV = (*RedisKV)(nil)

// Get implements KV.
func (r *RedisKV) Get(ctx context.Context, key string) ([]byte, bool, error) {
	b, err := r.rdb.Get(ctx, r.prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache: get: %w", err)
	}
	return b, true, nil
}

// Set implements KV.
func (r *RedisKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	// SET with a non-positive expiry is an error in Redis; an entry that
	// expires at once is simply not stored, as in MemoryKV.
	if ttl <= 0 {
		return nil
	}
	if err := r.rdb.Set(ctx, r.prefix+key, val, ttl).Err(); err != nil {
		return fmt.Errorf("cache: set: %w", err)
	}
	return nil
}

// Index algorithms of the Redis Query Engine.
const (
	AlgorithmFLAT = "FLAT"
	AlgorithmHNSW = "HNSW"
)

// RedisIndexOptions configures a RedisIndex.
type RedisIndexOptions struct {
	// Prefix is the gateway's global key prefix, for example "sb:".
	Prefix string
	// Name tells indexes apart, for example the embedding model and its
	// dimensions; vectors of different models must never share an index.
	Name string
	Dims int
	// Algorithm is AlgorithmFLAT (exact) or AlgorithmHNSW (approximate);
	// empty means FLAT.
	Algorithm string
	// M, EFConstruction and EFRuntime tune HNSW; 0 keeps Redis's default.
	M              int
	EFConstruction int
	EFRuntime      int
}

// RedisIndex is an Index on the Redis 8 Query Engine. Each item is a hash
// at <prefix>cache:sem:<name>:<id> with the fields ns and ctx (TAG), vec
// (FLOAT32 little-endian, cosine distance) and payload (not indexed).
//
// Commands go through rdb.Do and the replies are parsed by hand, because
// go-redis's typed FT.* commands refuse RESP3 replies (the client's
// default protocol) unless UnstableResp3 is set; the parser accepts both
// the RESP2 array and the RESP3 map shapes.
type RedisIndex struct {
	rdb       *redis.Client
	opts      RedisIndexOptions
	index     string // index name
	keyPrefix string // every item key starts with it
}

var _ Index = (*RedisIndex)(nil)

// NewRedisIndex returns the index described by opts. It does not talk to
// Redis; call Ensure at startup, or let the first Search create it.
func NewRedisIndex(rdb *redis.Client, opts RedisIndexOptions) (*RedisIndex, error) {
	if opts.Dims <= 0 {
		return nil, fmt.Errorf("cache: redis index needs positive dims, got %d", opts.Dims)
	}
	if opts.Name == "" {
		return nil, errors.New("cache: redis index needs a name")
	}
	switch opts.Algorithm {
	case "":
		opts.Algorithm = AlgorithmFLAT
	case AlgorithmFLAT, AlgorithmHNSW:
	default:
		return nil, fmt.Errorf("cache: unknown index algorithm %q", opts.Algorithm)
	}
	base := opts.Prefix + "cache:sem:" + opts.Name
	return &RedisIndex{rdb: rdb, opts: opts, index: base, keyPrefix: base + ":"}, nil
}

// IndexName is the Query Engine index name.
func (r *RedisIndex) IndexName() string { return r.index }

// Ensure creates the index unless it exists. Creating it also indexes any
// hashes already stored under the key prefix, so items added while the
// index was missing are not lost.
func (r *RedisIndex) Ensure(ctx context.Context) error {
	args := []any{"FT.CREATE", r.index, "ON", "HASH", "PREFIX", 1, r.keyPrefix,
		"SCHEMA", "ns", "TAG", "ctx", "TAG", "vec", "VECTOR", r.opts.Algorithm}
	attrs := []any{"TYPE", "FLOAT32", "DIM", r.opts.Dims, "DISTANCE_METRIC", "COSINE"}
	if r.opts.Algorithm == AlgorithmHNSW {
		for _, p := range []struct {
			name string
			v    int
		}{{"M", r.opts.M}, {"EF_CONSTRUCTION", r.opts.EFConstruction}, {"EF_RUNTIME", r.opts.EFRuntime}} {
			if p.v > 0 {
				attrs = append(attrs, p.name, p.v)
			}
		}
	}
	args = append(args, len(attrs))
	args = append(args, attrs...)
	err := r.rdb.Do(ctx, args...).Err()
	if err != nil && !isIndexExists(err) {
		return fmt.Errorf("cache: create index %s: %w", r.index, err)
	}
	return nil
}

// Drop removes the index and, with deleteItems, every item in it.
func (r *RedisIndex) Drop(ctx context.Context, deleteItems bool) error {
	args := []any{"FT.DROPINDEX", r.index}
	if deleteItems {
		args = append(args, "DD")
	}
	if err := r.rdb.Do(ctx, args...).Err(); err != nil && !isIndexMissing(err) {
		return fmt.Errorf("cache: drop index %s: %w", r.index, err)
	}
	return nil
}

// Add implements Index.
//
// A missing index cannot be noticed here, since HSET succeeds without
// it; the next Search recreates the index, which picks the item up.
func (r *RedisIndex) Add(ctx context.Context, it Item) error {
	return r.AddBatch(ctx, []Item{it})
}

// AddBatch adds items in one round trip. Each item's HSET and PEXPIRE
// run in one MULTI, so no item is ever stored without its expiry.
func (r *RedisIndex) AddBatch(ctx context.Context, items []Item) error {
	for _, it := range items {
		if len(it.Vector) != r.opts.Dims {
			return ErrDimensions
		}
	}
	_, err := r.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for _, it := range items {
			key := r.keyPrefix + it.ID
			p.HSet(ctx, key,
				"ns", tagValue(it.Namespace),
				"ctx", tagValue(it.Context),
				"vec", vectorBytes(it.Vector),
				"payload", it.Payload)
			// PEXPIRE with a non-positive TTL deletes the key: an item
			// that expires at once is gone, as in MemoryIndex.
			p.PExpire(ctx, key, it.TTL)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("cache: add: %w", err)
	}
	return nil
}

// Search implements Index.
func (r *RedisIndex) Search(ctx context.Context, q Query) ([]Match, error) {
	return r.SearchWith(ctx, q, 0)
}

// SearchWith is Search with a per-query EF_RUNTIME for an HNSW index; 0
// keeps the index's own. Benchmarks use it to trade recall for latency on
// one build.
func (r *RedisIndex) SearchWith(ctx context.Context, q Query, efRuntime int) ([]Match, error) {
	filter := "(@ns:{" + escapeTag(tagValue(q.Namespace)) + "} @ctx:{" + escapeTag(tagValue(q.Context)) + "})"
	return r.search(ctx, filter, q.Vector, q.K, efRuntime)
}

// SearchUnfiltered is a KNN over every item regardless of namespace and
// context, with an optional EF_RUNTIME as in SearchWith. It is for
// benchmarks only: the gateway must never search across tenants.
func (r *RedisIndex) SearchUnfiltered(ctx context.Context, vec []float32, k, efRuntime int) ([]Match, error) {
	return r.search(ctx, "*", vec, k, efRuntime)
}

func (r *RedisIndex) search(ctx context.Context, filter string, vec []float32, k, efRuntime int) ([]Match, error) {
	if len(vec) != r.opts.Dims {
		return nil, ErrDimensions
	}
	if k <= 0 {
		return nil, nil
	}
	knn := "KNN " + strconv.Itoa(k) + " @vec $vec"
	if efRuntime > 0 && r.opts.Algorithm == AlgorithmHNSW {
		knn += " EF_RUNTIME " + strconv.Itoa(efRuntime)
	}
	args := []any{"FT.SEARCH", r.index,
		filter + "=>[" + knn + " AS __dist]",
		"PARAMS", 2, "vec", vectorBytes(vec),
		"RETURN", 2, "__dist", "payload",
		"SORTBY", "__dist", "ASC",
		"LIMIT", 0, k,
		"DIALECT", 2}
	reply, err := r.rdb.Do(ctx, args...).Result()
	if err != nil && isIndexMissing(err) {
		// Redis lost the index (a restart without persistence, a
		// flush): recreate it and try once more.
		if err := r.Ensure(ctx); err != nil {
			return nil, err
		}
		reply, err = r.rdb.Do(ctx, args...).Result()
	}
	if err != nil {
		return nil, fmt.Errorf("cache: search %s: %w", r.index, err)
	}
	ms, err := r.parseSearch(reply)
	if err != nil {
		return nil, fmt.Errorf("cache: search %s: %w", r.index, err)
	}
	return ms, nil
}

// parseSearch reads an FT.SEARCH reply in either protocol:
//
//	RESP2: [total, key, [field, value, ...], key, [...], ...]
//	RESP3: {total_results, results: [{id, extra_attributes: {...}}, ...], ...}
func (r *RedisIndex) parseSearch(reply any) ([]Match, error) {
	var docs []searchDoc
	switch v := reply.(type) {
	case []any:
		if len(v) == 0 {
			return nil, errors.New("empty reply")
		}
		for i := 1; i+1 < len(v); i += 2 {
			key, ok := v[i].(string)
			if !ok {
				return nil, fmt.Errorf("unexpected key %T", v[i])
			}
			fields, err := toMap(v[i+1])
			if err != nil {
				return nil, err
			}
			docs = append(docs, searchDoc{key, fields})
		}
	case map[any]any:
		results, _ := v["results"].([]any)
		for _, res := range results {
			m, err := toMap(res)
			if err != nil {
				return nil, err
			}
			key, _ := m["id"].(string)
			fields, err := toMap(m["extra_attributes"])
			if err != nil {
				return nil, err
			}
			docs = append(docs, searchDoc{key, fields})
		}
	default:
		return nil, fmt.Errorf("unexpected reply %T", reply)
	}
	out := make([]Match, 0, len(docs))
	for _, d := range docs {
		if d.fields["__dist"] == nil {
			// The hash expired or was replaced between matching and
			// loading; Redis returns its key with no fields.
			continue
		}
		dist, err := toFloat(d.fields["__dist"])
		if err != nil {
			return nil, fmt.Errorf("distance of %s: %w", d.key, err)
		}
		var payload []byte
		if p, ok := d.fields["payload"].(string); ok {
			payload = []byte(p)
		}
		out = append(out, Match{
			ID:         strings.TrimPrefix(d.key, r.keyPrefix),
			Similarity: 1 - dist,
			Payload:    payload,
		})
	}
	return out, nil
}

type searchDoc struct {
	key    string
	fields map[string]any
}

// SizeMB is the vector index's memory in megabytes, from FT.INFO.
func (r *RedisIndex) SizeMB(ctx context.Context) (float64, error) {
	info, err := r.Info(ctx)
	if err != nil {
		return 0, err
	}
	mb, err := toFloat(info["vector_index_sz_mb"])
	if err != nil {
		return 0, fmt.Errorf("cache: index size: %w", err)
	}
	return mb, nil
}

// Info is the FT.INFO reply as a map, whichever protocol the client uses.
func (r *RedisIndex) Info(ctx context.Context) (map[string]any, error) {
	reply, err := r.rdb.Do(ctx, "FT.INFO", r.index).Result()
	if err != nil {
		return nil, fmt.Errorf("cache: info %s: %w", r.index, err)
	}
	m, err := toMap(reply)
	if err != nil {
		return nil, fmt.Errorf("cache: info %s: %w", r.index, err)
	}
	return m, nil
}

// toMap reads a RESP2 flat [k, v, k, v] array or a RESP3 map.
func toMap(v any) (map[string]any, error) {
	out := map[string]any{}
	switch m := v.(type) {
	case nil:
	case []any:
		for i := 0; i+1 < len(m); i += 2 {
			out[fmt.Sprint(m[i])] = m[i+1]
		}
	case map[any]any:
		for k, x := range m {
			out[fmt.Sprint(k)] = x
		}
	case map[string]any:
		return m, nil
	default:
		return nil, fmt.Errorf("unexpected reply %T", v)
	}
	return out, nil
}

// toFloat reads a number that RESP2 sends as a string and RESP3 may send
// as a double or an integer.
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(x, 64)
	default:
		return 0, fmt.Errorf("unexpected number %T", v)
	}
}

func isIndexExists(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "index already exists")
}

// isIndexMissing recognises Redis 8's SEARCH_INDEX_NOT_FOUND and the
// wording of older RediSearch versions.
func isIndexMissing(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "index not found") ||
		strings.Contains(s, "unknown index") ||
		strings.Contains(s, "no such index")
}

// tagValue is how a namespace or context is stored in its TAG field.
// TAG fields split on a separator, ignore empty values and are matched
// case-insensitively, any of which could make two different values meet.
// Lower-case alphanumerics (the hex hashes the gateway passes) are stored
// as they are, so keys stay readable; anything else is stored as "~" and
// its hex encoding, which no plain value can equal.
func tagValue(s string) string {
	plain := s != ""
	for i := 0; i < len(s) && plain; i++ {
		plain = isLowerAlnum(s[i])
	}
	if plain {
		return s
	}
	return "~" + hex.EncodeToString([]byte(s))
}

func isLowerAlnum(c byte) bool { return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') }

// escapeTag backslash-escapes everything but letters, digits and '_' in
// a TAG query value, so no value can change the query's syntax.
func escapeTag(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isLowerAlnum(c) && (c < 'A' || c > 'Z') && c != '_' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// vectorBytes is v as FLOAT32 little-endian, the layout the index reads.
func vectorBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

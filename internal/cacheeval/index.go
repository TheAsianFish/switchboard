package cacheeval

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cache"
)

// benchPrefix keeps the benchmark's keys and indexes apart from any
// gateway's ("sb:" by default), so a run against a shared Redis can never
// touch cached responses.
const benchPrefix = "sb-bench:"

// benchNamespace is the one tenant every benchmark entry belongs to.
const benchNamespace = "be7c4000be7c4000"

const (
	indexWarmups   = 20
	indexBatch     = 500
	indexLoaders   = 8
	indexEntryTTL  = 24 * time.Hour
	indexDrainPoll = 50 * time.Millisecond
)

type indexConfig struct {
	redisURL     string
	entries      int
	dims         []int
	queries      int
	contexts     int
	seed         uint64
	out          string
	m, efc       int
	efrs         []int
	drainTimeout time.Duration
}

// indexMain runs `switchboard eval cache-index`: the FLAT against HNSW
// check on the product's RedisIndex.
func indexMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cache-index", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c indexConfig
	fs.StringVar(&c.redisURL, "redis", envOr("SWITCHBOARD_TEST_REDIS_URL", "redis://127.0.0.1:6379/0"), "Redis 8 URL (database 0: the Query Engine indexes no other)")
	fs.IntVar(&c.entries, "entries", 100000, "entries in each index")
	dims := fs.String("dims", "384,768", "comma-separated vector lengths (all-minilm is 384, nomic-embed-text 768)")
	fs.IntVar(&c.queries, "queries", 1000, "timed queries per algorithm and query shape")
	fs.IntVar(&c.contexts, "contexts", 10000, "distinct context tags the entries are spread over")
	fs.Uint64Var(&c.seed, "seed", 1, "vector seed")
	fs.StringVar(&c.out, "out", "eval/results/q3-cache", "results directory")
	// Redis's documented defaults, passed explicitly so the result file
	// records what was measured.
	fs.IntVar(&c.m, "hnsw-m", 16, "HNSW M")
	fs.IntVar(&c.efc, "hnsw-ef-construction", 200, "HNSW EF_CONSTRUCTION")
	efrs := fs.String("hnsw-ef-runtime", "10,100,1000", "comma-separated HNSW EF_RUNTIME values, each measured on the same build (10 is the default)")
	fs.DurationVar(&c.drainTimeout, "drain-timeout", 30*time.Minute, "longest wait for background indexing to finish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, f := range strings.Split(*dims, ",") {
		d, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || d <= 0 {
			return fmt.Errorf("cache-index: bad --dims entry %q", f)
		}
		c.dims = append(c.dims, d)
	}
	for _, f := range strings.Split(*efrs, ",") {
		e, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || e <= 0 {
			return fmt.Errorf("cache-index: bad --hnsw-ef-runtime entry %q", f)
		}
		c.efrs = append(c.efrs, e)
	}
	if c.entries <= 0 || c.queries <= 0 || c.contexts <= 0 {
		return errors.New("cache-index: --entries, --queries and --contexts must be positive")
	}

	opts, err := redis.ParseURL(c.redisURL)
	if err != nil {
		return fmt.Errorf("cache-index: --redis: %w", err)
	}
	// Dropping an index with 100k documents takes a while; the gateway's
	// short timeouts would cut it off.
	opts.ReadTimeout = time.Minute
	opts.WriteTimeout = time.Minute
	rdb := redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()
	version, err := redisVersion(ctx, rdb)
	if err != nil {
		return fmt.Errorf("cache-index: %w", err)
	}

	info := RunInfo{
		Commit:   gitCommit(ctx),
		Date:     time.Now().UTC().Format(time.RFC3339),
		Command:  "switchboard eval cache-index " + strings.Join(args, " "),
		Hardware: bench.DetectHardware(),
	}
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return fmt.Errorf("cache-index: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "redis %s, %d entries over %d contexts, %d queries\n", version, c.entries, c.contexts, c.queries)
	for _, d := range c.dims {
		res, err := indexOne(ctx, rdb, c, d, stdout)
		if err != nil {
			return fmt.Errorf("cache-index: %d dims: %w", d, err)
		}
		res.RunInfo = info
		res.RedisVersion = version
		path := filepath.Join(c.out, IndexFileName(d))
		if err := writeJSON(path, res); err != nil {
			return fmt.Errorf("cache-index: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "wrote %s\n", path)
	}
	return nil
}

// indexOne measures FLAT and HNSW on one seeded data set of dims-long
// vectors.
//
// The entries and queries are random unit vectors (normalised Gaussians),
// uniform on the sphere. Real embeddings cluster; random ones have no
// structure for HNSW's graph to exploit, so its recall here is a worst
// case, and a random query's nearest neighbour is barely nearer than the
// rest, which is the hardest case for an approximate search.
func indexOne(ctx context.Context, rdb *redis.Client, c indexConfig, dims int, stdout io.Writer) (IndexResult, error) {
	rng := rand.New(rand.NewPCG(c.seed, uint64(dims)))
	entries := randomUnit(rng, c.entries, dims)
	queries := randomUnit(rng, c.queries, dims)
	ctxOf := make([]int, c.queries) // the context each filtered query asks in
	for i := range ctxOf {
		ctxOf[i] = rng.IntN(min(c.contexts, c.entries))
	}
	start := time.Now()
	truth := nearest(entries, queries)
	// The filtered truth: the nearest entry within the query's context,
	// where entry i belongs to context i mod contexts.
	ftruth := make([]int, len(queries))
	for q := range queries {
		best, bestSim := -1, math.Inf(-1)
		for i := ctxOf[q]; i < len(entries); i += c.contexts {
			if s := dot(queries[q], entries[i]); s > bestSim {
				best, bestSim = i, s
			}
		}
		ftruth[q] = best
	}
	_, _ = fmt.Fprintf(stdout, "%d dims: exact nearest neighbours in %.1fs\n", dims, time.Since(start).Seconds())

	res := IndexResult{
		Entries:  c.entries,
		Dims:     dims,
		Queries:  c.queries,
		Contexts: c.contexts,
		Vectors:  fmt.Sprintf("random unit (normalised Gaussian), entries and queries independent, seed %d", c.seed),
	}
	for _, alg := range []string{cache.AlgorithmFLAT, cache.AlgorithmHNSW} {
		runs, err := indexRuns(ctx, rdb, c, alg, dims, entries, queries, ctxOf, truth, ftruth)
		if err != nil {
			return res, fmt.Errorf("%s: %w", alg, err)
		}
		for _, run := range runs {
			_, _ = fmt.Fprintf(stdout, "%d dims %-4s %v: build %.1fs, unfiltered p50 %.2f p99 %.2f ms recall@1 %.3f, filtered p50 %.2f p99 %.2f ms recall@1 %.3f, %.1f MB\n",
				dims, alg, run.Params, run.BuildSeconds, run.Unfiltered.P50Ms, run.Unfiltered.P99Ms, run.RecallAt1,
				run.Filtered.P50Ms, run.Filtered.P99Ms, run.FilteredRecallAt1, run.IndexSizeMB)
		}
		res.Runs = append(res.Runs, runs...)
	}
	return res, nil
}

// indexRuns builds one index with alg and measures it: once for FLAT, and
// once per EF_RUNTIME for HNSW, which is a query-time setting.
func indexRuns(ctx context.Context, rdb *redis.Client, c indexConfig, alg string, dims int,
	entries, queries [][]float32, ctxOf, truth, ftruth []int,
) (runs []IndexRun, err error) {
	var run IndexRun
	opts := cache.RedisIndexOptions{
		Prefix:    benchPrefix,
		Name:      fmt.Sprintf("bench-%d-%s", dims, strings.ToLower(alg)),
		Dims:      dims,
		Algorithm: alg,
	}
	run.Algorithm = alg
	if alg == cache.AlgorithmHNSW {
		opts.M, opts.EFConstruction = c.m, c.efc
	}
	idx, err := cache.NewRedisIndex(rdb, opts)
	if err != nil {
		return nil, err
	}
	// Start clean, and clean up even when interrupted.
	if err := idx.Drop(ctx, true); err != nil {
		return nil, err
	}
	defer func() {
		if derr := idx.Drop(context.WithoutCancel(ctx), true); derr != nil && err == nil {
			err = derr
		}
	}()
	if err := idx.Ensure(ctx); err != nil {
		return nil, err
	}

	start := time.Now()
	if err := load(ctx, idx, entries, c.contexts); err != nil {
		return nil, err
	}
	if err := waitIndexed(ctx, idx, len(entries), c.drainTimeout); err != nil {
		return nil, err
	}
	run.BuildSeconds = time.Since(start).Seconds()
	if run.IndexSizeMB, err = idx.SizeMB(ctx); err != nil {
		return nil, err
	}

	efs := []int{0}
	if alg == cache.AlgorithmHNSW {
		efs = c.efrs
	}
	for _, ef := range efs {
		r := run
		if ef > 0 {
			r.Params = map[string]int{"M": c.m, "EF_CONSTRUCTION": c.efc, "EF_RUNTIME": ef}
		}
		hits := 0
		r.Unfiltered, err = timeQueries(len(queries), func(i int, warm bool) error {
			ms, err := idx.SearchUnfiltered(ctx, queries[i], 1, ef)
			if err != nil {
				return err
			}
			if !warm && len(ms) == 1 && isNearest(ms[0].ID, queries[i], entries, truth[i]) {
				hits++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		r.RecallAt1 = float64(hits) / float64(len(queries))
		fhits := 0
		r.Filtered, err = timeQueries(len(queries), func(i int, warm bool) error {
			ms, err := idx.SearchWith(ctx, cache.Query{Namespace: benchNamespace, Context: contextTag(ctxOf[i]), Vector: queries[i], K: 1}, ef)
			if err != nil {
				return err
			}
			if !warm && len(ms) == 1 && isNearest(ms[0].ID, queries[i], entries, ftruth[i]) {
				fhits++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		r.FilteredRecallAt1 = float64(fhits) / float64(len(queries))
		runs = append(runs, r)
	}
	return runs, nil
}

// timeQueries runs a few untimed warm-up calls, then query(0..n-1) one
// after another, and summarises the client-side latency. warm tells the
// query not to count its result.
func timeQueries(n int, query func(i int, warm bool) error) (IndexLatency, error) {
	for i := range indexWarmups {
		if err := query(i%n, true); err != nil {
			return IndexLatency{}, err
		}
	}
	samples := make([]time.Duration, n)
	for i := range n {
		start := time.Now()
		if err := query(i, false); err != nil {
			return IndexLatency{}, err
		}
		samples[i] = time.Since(start)
	}
	st := bench.Summarise(samples)
	return IndexLatency{P50Ms: st.P50, P99Ms: st.P99, MeanMs: st.Mean}, nil
}

// contextTag is entry i's context: one of n hex tags, as the gateway's
// context hashes are hex.
func contextTag(c int) string { return fmt.Sprintf("%016x", uint64(c)*0x9e3779b97f4a7c15) }

// load adds every entry through the product's AddBatch from a few
// goroutines, entry i under context i mod contexts.
func load(ctx context.Context, idx *cache.RedisIndex, entries [][]float32, contexts int) error {
	batches := make(chan int)
	errs := make(chan error, indexLoaders)
	var wg sync.WaitGroup
	for range indexLoaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items := make([]cache.Item, 0, indexBatch)
			for from := range batches {
				items = items[:0]
				for i := from; i < min(from+indexBatch, len(entries)); i++ {
					items = append(items, cache.Item{
						ID:        strconv.Itoa(i),
						Namespace: benchNamespace,
						Context:   contextTag(i % contexts),
						Vector:    entries[i],
						Payload:   []byte("p"),
						TTL:       indexEntryTTL,
					})
				}
				if err := idx.AddBatch(ctx, items); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	var err error
send:
	for from := 0; from < len(entries); from += indexBatch {
		select {
		case batches <- from:
		case err = <-errs:
			break send
		}
	}
	close(batches)
	wg.Wait()
	if err == nil {
		select {
		case err = <-errs:
		default:
		}
	}
	return err
}

// waitIndexed waits until the index holds n documents and Redis's
// background workers have moved every vector out of the HNSW index's
// flat buffer into the graph (Redis 8 inserts into HNSW asynchronously),
// so the build time covers the whole build.
func waitIndexed(ctx context.Context, idx *cache.RedisIndex, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := idx.Info(ctx)
		if err != nil {
			return err
		}
		docs, _ := infoNumber(info["num_docs"])
		indexing, _ := infoNumber(info["indexing"])
		pending := sumField(info, "flat_buffer_size")
		if int(docs) >= n && indexing == 0 && pending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("index not built after %s: %v of %d documents, %v vectors pending", timeout, docs, n, pending)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(indexDrainPoll):
		}
	}
}

// sumField adds up every value named key anywhere in an FT.INFO reply,
// which nests per-field statistics as RESP2 flat arrays or RESP3 maps.
func sumField(v any, key string) float64 {
	var sum float64
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == key {
				f, _ := infoNumber(e)
				sum += f
			} else {
				sum += sumField(e, key)
			}
		}
	case map[any]any:
		for k, e := range x {
			if fmt.Sprint(k) == key {
				f, _ := infoNumber(e)
				sum += f
			} else {
				sum += sumField(e, key)
			}
		}
	case []any:
		for i, e := range x {
			if s, ok := e.(string); ok && s == key && i+1 < len(x) {
				f, _ := infoNumber(x[i+1])
				sum += f
				continue
			}
			sum += sumField(e, key)
		}
	}
	return sum
}

// infoNumber reads a number RESP2 sends as a string and RESP3 as a
// number.
func infoNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// randomUnit returns n random unit vectors of length dims.
func randomUnit(rng *rand.Rand, n, dims int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dims)
		var sum float64
		for j := range v {
			x := rng.NormFloat64()
			v[j] = float32(x)
			sum += x * x
		}
		inv := 1 / math.Sqrt(sum)
		for j := range v {
			v[j] = float32(float64(v[j]) * inv)
		}
		out[i] = v
	}
	return out
}

// nearest is each query's exact nearest entry by cosine similarity, by
// brute force over all entries, spread over every CPU.
func nearest(entries, queries [][]float32) []int {
	out := make([]int, len(queries))
	next := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := range next {
				best, bestSim := -1, math.Inf(-1)
				for i, e := range entries {
					if s := dot(queries[q], e); s > bestSim {
						best, bestSim = i, s
					}
				}
				out[q] = best
			}
		}()
	}
	for q := range queries {
		next <- q
	}
	close(next)
	wg.Wait()
	return out
}

// isNearest reports whether the entry id is the exact nearest neighbour
// of q, or ties with it to within float32 rounding (Redis computes the
// distance in float32).
func isNearest(id string, q []float32, entries [][]float32, best int) bool {
	i, err := strconv.Atoi(id)
	if err != nil || i < 0 || i >= len(entries) {
		return false
	}
	return i == best || dot(q, entries[best])-dot(q, entries[i]) < 1e-6
}

// dot is the dot product, which for unit vectors is their cosine.
func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// redisVersion is redis_version from INFO server.
func redisVersion(ctx context.Context, rdb *redis.Client) (string, error) {
	s, err := rdb.Info(ctx, "server").Result()
	if err != nil {
		return "", fmt.Errorf("info server: %w", err)
	}
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); ok {
			return v, nil
		}
	}
	return "", errors.New("info server: no redis_version")
}

//go:build integration

package cache_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/cache/cachetest"
)

const defaultTestRedis = "redis://127.0.0.1:6379/0"

// realRedis connects to the test Redis. With SWITCHBOARD_TEST_REDIS_URL
// set, an unreachable server fails the test (CI asked for it); unset, the
// default address is tried and the test skips when nothing listens. The
// Query Engine only indexes database 0, so the default uses it and every
// test works under its own prefix.
func realRedis(t *testing.T) *redis.Client {
	t.Helper()
	url, explicit := os.LookupEnv("SWITCHBOARD_TEST_REDIS_URL")
	if !explicit {
		url = defaultTestRedis
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("SWITCHBOARD_TEST_REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	probe.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		probe.err = rdb.Ping(ctx).Err()
	})
	if err := probe.err; err != nil {
		if explicit {
			t.Fatalf("redis at SWITCHBOARD_TEST_REDIS_URL unreachable: %v", err)
		}
		t.Skipf("no redis at %s: %v", url, err)
	}
	return rdb
}

var probe struct {
	once sync.Once
	err  error
}

var prefixSeq atomic.Int64

func testPrefix() string {
	return fmt.Sprintf("sbtest:%d:%d:", os.Getpid(), prefixSeq.Add(1))
}

func newRedisIndex(t *testing.T, rdb *redis.Client, opts cache.RedisIndexOptions) *cache.RedisIndex {
	t.Helper()
	opts.Prefix = testPrefix()
	if opts.Name == "" {
		opts.Name = "test"
	}
	idx, err := cache.NewRedisIndex(rdb, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Drop(context.Background(), true) })
	return idx
}

func TestRedisIndexContract(t *testing.T) {
	t.Parallel()
	for _, alg := range []string{cache.AlgorithmFLAT, cache.AlgorithmHNSW} {
		t.Run(alg, func(t *testing.T) {
			t.Parallel()
			cachetest.Index(t, func(t *testing.T, dims int) cachetest.Subject {
				rdb := realRedis(t)
				return cachetest.Subject{
					Index:    newRedisIndex(t, rdb, cache.RedisIndexOptions{Dims: dims, Algorithm: alg}),
					ShortTTL: 500 * time.Millisecond,
					Advance:  time.Sleep,
					Wait:     3 * time.Second,
				}
			})
		})
	}
}

func TestRedisIndexEnsureIsIdempotent(t *testing.T) {
	t.Parallel()
	rdb := realRedis(t)
	idx := newRedisIndex(t, rdb, cache.RedisIndexOptions{Dims: 4, Algorithm: cache.AlgorithmHNSW, M: 8, EFConstruction: 100, EFRuntime: 20})
	if err := idx.Ensure(context.Background()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if _, err := idx.SizeMB(context.Background()); err != nil {
		t.Fatalf("SizeMB: %v", err)
	}
}

// TestRedisIndexRecreatesLostIndex drops the index, as a Redis restart
// without persistence would, and checks the next Search recreates it and
// finds an item added while it was missing.
func TestRedisIndexRecreatesLostIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := realRedis(t)
	idx := newRedisIndex(t, rdb, cache.RedisIndexOptions{Dims: 2})
	if err := idx.Drop(ctx, true); err != nil {
		t.Fatal(err)
	}
	it := cache.Item{ID: "x", Namespace: "n", Context: "c", Vector: []float32{1, 0}, Payload: []byte("p"), TTL: time.Minute}
	if err := idx.Add(ctx, it); err != nil {
		t.Fatal(err)
	}
	q := cache.Query{Namespace: "n", Context: "c", Vector: []float32{1, 0}, K: 1}
	// Creating the index backfills existing hashes in the background, so
	// the first search may not see the item yet.
	deadline := time.Now().Add(3 * time.Second)
	for {
		ms, err := idx.Search(ctx, q)
		if err != nil {
			t.Fatalf("Search after the index was lost: %v", err)
		}
		if len(ms) == 1 && ms[0].ID == "x" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("item added while the index was missing not found: %+v", ms)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRedisIndexBothProtocols runs a search over RESP2 and RESP3, whose
// FT.SEARCH replies differ in shape.
func TestRedisIndexBothProtocols(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := realRedis(t).Options()
	for _, proto := range []int{2, 3} {
		t.Run(fmt.Sprintf("RESP%d", proto), func(t *testing.T) {
			t.Parallel()
			opts := *base
			opts.Protocol = proto
			rdb := redis.NewClient(&opts)
			t.Cleanup(func() { _ = rdb.Close() })
			idx := newRedisIndex(t, rdb, cache.RedisIndexOptions{Dims: 2})
			if err := idx.Add(ctx, cache.Item{ID: "x", Namespace: "n", Context: "c", Vector: []float32{0.6, 0.8}, Payload: []byte("p"), TTL: time.Minute}); err != nil {
				t.Fatal(err)
			}
			// A freshly created index scans the keyspace in the background,
			// so on a loaded machine the item can take a moment to appear.
			var ms []cache.Match
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				var err error
				ms, err = idx.Search(ctx, cache.Query{Namespace: "n", Context: "c", Vector: []float32{1, 0}, K: 1})
				if err != nil {
					t.Fatal(err)
				}
				if len(ms) > 0 || time.Now().After(deadline) {
					break
				}
			}
			if len(ms) != 1 || ms[0].ID != "x" || string(ms[0].Payload) != "p" || ms[0].Similarity < 0.5999 || ms[0].Similarity > 0.6001 {
				t.Fatalf("got %+v", ms)
			}
			if _, err := idx.SizeMB(ctx); err != nil {
				t.Fatalf("SizeMB: %v", err)
			}
		})
	}
}

func TestRedisKV(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := realRedis(t)
	kv := cache.NewRedisKV(rdb, testPrefix())
	wantMiss(t, kv, "missing")
	mustSet(t, kv, "k", "v1", time.Minute)
	wantGet(t, kv, "k", "v1")
	mustSet(t, kv, "k", "v2", time.Minute)
	wantGet(t, kv, "k", "v2")
	bin := string([]byte{0, 0xff, '\n'})
	mustSet(t, kv, "bin", bin, time.Minute)
	wantGet(t, kv, "bin", bin)
	mustSet(t, kv, "short", "x", 100*time.Millisecond)
	wantGet(t, kv, "short", "x")
	time.Sleep(150 * time.Millisecond)
	wantMiss(t, kv, "short")
	mustSet(t, kv, "zero", "x", 0)
	wantMiss(t, kv, "zero")
	if err := kv.Set(ctx, "neg", []byte("x"), -time.Second); err != nil {
		t.Fatalf("Set with a negative TTL: %v", err)
	}
}

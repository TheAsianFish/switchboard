package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/cache/cachetest"
)

func TestMemoryIndexContract(t *testing.T) {
	t.Parallel()
	cachetest.Index(t, func(_ *testing.T, dims int) cachetest.Subject {
		clock := cachetest.NewClock()
		return cachetest.Subject{
			Index:    cache.NewMemoryIndex(dims, 1000, clock.Now),
			ShortTTL: time.Minute,
			Advance:  clock.Advance,
		}
	})
}

func TestMemoryIndexEvictsSoonestWhenFull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := cachetest.NewClock()
	idx := cache.NewMemoryIndex(2, 2, clock.Now)
	v := []float32{1, 0}
	for _, it := range []cache.Item{
		{ID: "late", TTL: 3 * time.Hour},
		{ID: "soon", TTL: time.Hour},
		{ID: "new", TTL: 2 * time.Hour},
	} {
		it.Namespace, it.Context, it.Vector = "n", "c", v
		if err := idx.Add(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := idx.Search(ctx, cache.Query{Namespace: "n", Context: "c", Vector: v, K: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range ms {
		got[m.ID] = true
	}
	if len(ms) != 2 || !got["late"] || !got["new"] {
		t.Fatalf("after overflow: %v, want late and new", got)
	}
}

func TestMemoryKV(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(t *testing.T, kv *cache.MemoryKV, clock *cachetest.Clock)
	}{
		{"Miss", func(t *testing.T, kv *cache.MemoryKV, _ *cachetest.Clock) {
			if _, ok, err := kv.Get(ctx, "nope"); ok || err != nil {
				t.Fatalf("Get(nope) = %v, %v", ok, err)
			}
		}},
		{"HitThenExpiry", func(t *testing.T, kv *cache.MemoryKV, clock *cachetest.Clock) {
			mustSet(t, kv, "k", "v", time.Minute)
			wantGet(t, kv, "k", "v")
			clock.Advance(time.Minute - time.Nanosecond)
			wantGet(t, kv, "k", "v")
			clock.Advance(time.Nanosecond)
			wantMiss(t, kv, "k")
		}},
		{"Overwrite", func(t *testing.T, kv *cache.MemoryKV, _ *cachetest.Clock) {
			mustSet(t, kv, "k", "one", time.Minute)
			mustSet(t, kv, "k", "two", time.Minute)
			wantGet(t, kv, "k", "two")
		}},
		{"EvictsSoonestWhenFull", func(t *testing.T, kv *cache.MemoryKV, _ *cachetest.Clock) {
			mustSet(t, kv, "a", "1", 3*time.Hour)
			mustSet(t, kv, "b", "2", time.Hour)
			mustSet(t, kv, "c", "3", 2*time.Hour)
			mustSet(t, kv, "d", "4", 4*time.Hour)
			wantMiss(t, kv, "b")
			wantGet(t, kv, "a", "1")
			wantGet(t, kv, "c", "3")
			wantGet(t, kv, "d", "4")
		}},
		{"FullOfExpiredKeepsLive", func(t *testing.T, kv *cache.MemoryKV, clock *cachetest.Clock) {
			// Dropping the expired entries makes room; no live entry
			// should go as well.
			mustSet(t, kv, "a", "1", time.Hour)
			mustSet(t, kv, "b", "2", time.Minute)
			mustSet(t, kv, "c", "3", 2*time.Hour)
			clock.Advance(2 * time.Minute)
			mustSet(t, kv, "d", "4", time.Hour)
			wantGet(t, kv, "a", "1")
			wantGet(t, kv, "c", "3")
			wantGet(t, kv, "d", "4")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := cachetest.NewClock()
			tt.run(t, cache.NewMemoryKV(3, clock.Now), clock)
		})
	}
}

func mustSet(t *testing.T, kv cache.KV, key, val string, ttl time.Duration) {
	t.Helper()
	if err := kv.Set(context.Background(), key, []byte(val), ttl); err != nil {
		t.Fatalf("Set(%s): %v", key, err)
	}
}

func wantGet(t *testing.T, kv cache.KV, key, want string) {
	t.Helper()
	got, ok, err := kv.Get(context.Background(), key)
	if err != nil || !ok || string(got) != want {
		t.Fatalf("Get(%s) = %q, %v, %v; want %q", key, got, ok, err, want)
	}
}

func wantMiss(t *testing.T, kv cache.KV, key string) {
	t.Helper()
	if got, ok, err := kv.Get(context.Background(), key); ok || err != nil {
		t.Fatalf("Get(%s) = %q, %v, %v; want a miss", key, got, ok, err)
	}
}

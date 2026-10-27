package ratelimit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/ratelimit/ratelimittest"
)

func newGCRA(rdb redis.Scripter, prefix string) ratelimit.Limiter {
	return ratelimit.NewGCRA(rdb, prefix)
}

func newMemoryGCRA(c *ratelimittest.Clock) ratelimit.Limiter {
	return ratelimit.NewMemoryGCRA(c.Now)
}

var gcraHarnesses = []struct {
	name string
	new  func(t *testing.T) ratelimittest.Harness
}{
	{"gcra/miniredis", miniredisHarness(newGCRA)},
	{"gcra/memory", memoryHarness(newMemoryGCRA)},
}

func TestGCRAContract(t *testing.T) {
	t.Parallel()
	for _, h := range gcraHarnesses {
		ratelimittest.RunContract(t, h.name, h.new)
	}
}

func TestGCRABound(t *testing.T) {
	t.Parallel()
	for _, h := range gcraHarnesses {
		ratelimittest.RunBound(t, h.name, h.new)
	}
}

func TestGCRAOracle(t *testing.T) {
	t.Parallel()
	for _, h := range gcraHarnesses {
		ratelimittest.RunOracle(t, h.name, h.new)
	}
}

func TestGCRAConcurrentReplicas(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	m.SetTime(ratelimittest.Start)
	var replicas []ratelimit.Limiter
	for range 3 {
		replicas = append(replicas, ratelimit.NewGCRA(newClient(t, m), "test:"))
	}
	l := ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 20}
	if got := admitConcurrently(t, replicas, "shared", l, 300); got != l.Burst {
		t.Fatalf("admitted %d across three replicas at one instant, want the burst %d", got, l.Burst)
	}
}

// TestGCRAValidatesFirst checks argument errors come before
// ErrNotImplemented, so callers see the real problem either way.
func TestGCRAValidatesFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	good := ratelimit.Limit{Rate: 1, Period: time.Second, Burst: 1}
	for _, lim := range []ratelimit.Limiter{ratelimit.NewGCRA(nil, ""), ratelimit.NewMemoryGCRA(nil)} {
		if _, err := lim.Allow(ctx, "k", good, 0); err == nil || errors.Is(err, ratelimit.ErrNotImplemented) {
			t.Errorf("%T.Allow(cost 0) err = %v, want a validation error", lim, err)
		}
		if err := lim.Adjust(ctx, "k", ratelimit.Limit{}, 1); err == nil || errors.Is(err, ratelimit.ErrNotImplemented) {
			t.Errorf("%T.Adjust(zero limit) err = %v, want a validation error", lim, err)
		}
	}
}

// The key lives exactly as long as its TAT is ahead of now, whether Allow
// or Adjust moved it, and is gone once it has drained.
func TestGCRAKeysExpire(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	m.SetTime(ratelimittest.Start)
	g := ratelimit.NewGCRA(newClient(t, m), "p:")
	l := ratelimit.Limit{Rate: 60, Period: time.Minute, Burst: 10} // T = 1 s
	ctx := context.Background()
	if _, err := g.Allow(ctx, "user", l, 5); errors.Is(err, ratelimit.ErrNotImplemented) {
		t.Skip("GCRA is not implemented yet")
	} else if err != nil {
		t.Fatal(err)
	}
	const key = "p:rl:gcra:{user}"
	if ttl := m.TTL(key); ttl != 5*time.Second {
		t.Fatalf("TTL after Allow(5) = %v, want 5s (TAT − now)", ttl)
	}
	if err := g.Adjust(ctx, "user", l, 3); err != nil {
		t.Fatal(err)
	}
	if ttl := m.TTL(key); ttl != 8*time.Second {
		t.Fatalf("TTL after Adjust(3) = %v, want 8s", ttl)
	}
	m.FastForward(8 * time.Second)
	if keys := m.Keys(); len(keys) != 0 {
		t.Fatalf("keys after the TAT drained = %v, want none", keys)
	}
}

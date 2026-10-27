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

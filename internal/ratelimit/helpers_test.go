package ratelimit_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/ratelimit/ratelimittest"
)

// newClient returns a go-redis client for m, closed with the test.
func newClient(t *testing.T, m *miniredis.Miniredis) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// miniredisHarness returns a harness factory for a Redis limiter on a
// fresh miniredis whose TIME is driven by the suites.
func miniredisHarness(newLimiter func(redis.Scripter, string) ratelimit.Limiter) func(t *testing.T) ratelimittest.Harness {
	return func(t *testing.T) ratelimittest.Harness {
		t.Helper()
		m := miniredis.RunT(t)
		return ratelimittest.Harness{Limiter: newLimiter(newClient(t, m), "test:"), Set: m.SetTime}
	}
}

// memoryHarness returns a harness factory for an in-memory limiter on a
// fake clock.
func memoryHarness(newLimiter func(*ratelimittest.Clock) ratelimit.Limiter) func(t *testing.T) ratelimittest.Harness {
	return func(t *testing.T) ratelimittest.Harness {
		t.Helper()
		c := ratelimittest.NewClock(ratelimittest.Start)
		return ratelimittest.Harness{Limiter: newLimiter(c), Set: c.Set}
	}
}

// admitConcurrently fires n one-unit requests for key from n goroutines,
// spread round-robin over limiters (one per replica), and returns how many
// were admitted. A limiter returning ErrNotImplemented skips the test.
func admitConcurrently(t *testing.T, limiters []ratelimit.Limiter, key string, l ratelimit.Limit, n int) int64 {
	t.Helper()
	var (
		admitted, notImpl atomic.Int64
		wg                sync.WaitGroup
		errMu             sync.Mutex
		firstErr          error
	)
	for i := range n {
		lim := limiters[i%len(limiters)]
		wg.Go(func() {
			d, err := lim.Allow(context.Background(), key, l, 1)
			switch {
			case err == nil:
				if d.Allowed {
					admitted.Add(1)
				}
			case errors.Is(err, ratelimit.ErrNotImplemented):
				notImpl.Add(1)
			default:
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		})
	}
	wg.Wait()
	if notImpl.Load() > 0 {
		t.Skip("limiter: not implemented yet")
	}
	if firstErr != nil {
		t.Fatalf("concurrent Allow: %v", firstErr)
	}
	return admitted.Load()
}

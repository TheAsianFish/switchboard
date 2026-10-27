package ratelimittest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

// bucketLimiter serves the oracle as a Limiter, so the suites can be
// checked against a model that is known to conform: if a suite fails
// here, the suite is wrong.
type bucketLimiter struct {
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*TokenBucket
}

func (b *bucketLimiter) bucket(key string) *TokenBucket {
	if b.buckets[key] == nil {
		b.buckets[key] = &TokenBucket{}
	}
	return b.buckets[key]
}

func (b *bucketLimiter) Allow(_ context.Context, key string, l ratelimit.Limit, cost int64) (ratelimit.Decision, error) {
	if err := l.Validate(); err != nil {
		return ratelimit.Decision{}, err
	}
	if cost < 1 {
		return ratelimit.Decision{}, fmt.Errorf("cost %d", cost)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bucket(key).Allow(b.now(), l, cost), nil
}

func (b *bucketLimiter) Adjust(_ context.Context, key string, l ratelimit.Limit, delta int64) error {
	if err := l.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bucket(key).Adjust(b.now(), l, delta)
	return nil
}

func bucketHarness(t *testing.T) Harness {
	t.Helper()
	c := NewClock(Start)
	return Harness{Limiter: &bucketLimiter{now: c.Now, buckets: map[string]*TokenBucket{}}, Set: c.Set}
}

// realClockBucketHarness runs the oracle on the wall clock, which is how
// the suites see a real Redis.
func realClockBucketHarness(t *testing.T) Harness {
	t.Helper()
	return Harness{Limiter: &bucketLimiter{now: time.Now, buckets: map[string]*TokenBucket{}}}
}

func TestSuitesAcceptTheOracle(t *testing.T) {
	t.Parallel()
	RunContract(t, "oracle", bucketHarness)
	RunBound(t, "oracle", bucketHarness)
	RunOracle(t, "oracle", bucketHarness)
	RunContract(t, "oracle_real_clock", realClockBucketHarness)
}

func TestTokenBucket(t *testing.T) {
	t.Parallel()
	l := ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 20} // T = 600 ms
	type step struct {
		at    time.Duration // since Start
		cost  int64         // 0 means Adjust by delta
		delta int64
		want  ratelimit.Decision
	}
	ms := time.Millisecond
	cases := []struct {
		name  string
		steps []step
	}{
		{"burst at one instant", []step{
			{at: 0, cost: 20, want: ratelimit.Decision{Allowed: true}},
			{at: 0, cost: 1, want: ratelimit.Decision{RetryAfter: 600 * ms}},
		}},
		{"refill one interval", []step{
			{at: 0, cost: 20, want: ratelimit.Decision{Allowed: true}},
			{at: 599 * ms, cost: 1, want: ratelimit.Decision{RetryAfter: ms}},
			{at: 600 * ms, cost: 1, want: ratelimit.Decision{Allowed: true}},
		}},
		{"retry after covers the whole cost", []step{
			{at: 0, cost: 18, want: ratelimit.Decision{Allowed: true, Remaining: 2}},
			{at: 0, cost: 5, want: ratelimit.Decision{Remaining: 2, RetryAfter: 1800 * ms}},
			{at: 1800 * ms, cost: 5, want: ratelimit.Decision{Allowed: true}},
		}},
		{"never above burst", []step{
			{at: 0, cost: 21, want: ratelimit.Decision{Remaining: 20, RetryAfter: ratelimit.Never}},
		}},
		{"capacity caps idle refill", []step{
			{at: 0, cost: 20, want: ratelimit.Decision{Allowed: true}},
			{at: time.Hour, cost: 20, want: ratelimit.Decision{Allowed: true}},
			{at: time.Hour, cost: 1, want: ratelimit.Decision{RetryAfter: 600 * ms}},
		}},
		{"charge into debt", []step{
			{at: 0, delta: 30},
			{at: 0, cost: 1, want: ratelimit.Decision{RetryAfter: 6600 * ms}},
			{at: 6600 * ms, cost: 1, want: ratelimit.Decision{Allowed: true}},
		}},
		{"refund stops at capacity", []step{
			{at: 0, cost: 5, want: ratelimit.Decision{Allowed: true, Remaining: 15}},
			{at: 0, delta: -10},
			{at: 0, cost: 20, want: ratelimit.Decision{Allowed: true}},
		}},
		{"partial token is not remaining", []step{
			{at: 0, cost: 20, want: ratelimit.Decision{Allowed: true}},
			{at: 900 * ms, cost: 1, want: ratelimit.Decision{Allowed: true}},
			{at: 900 * ms, cost: 1, want: ratelimit.Decision{RetryAfter: 300 * ms}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var b TokenBucket
			for i, s := range tc.steps {
				now := Start.Add(s.at)
				if s.cost == 0 {
					b.Adjust(now, l, s.delta)
					continue
				}
				if got := b.Allow(now, l, s.cost); got != s.want {
					t.Fatalf("step %d: Allow(%d) = %+v, want %+v", i, s.cost, got, s.want)
				}
			}
		})
	}
}

func TestTraceDeterministic(t *testing.T) {
	t.Parallel()
	l := ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 20}
	a, b := Trace(l, 7, 500, true), Trace(l, 7, 500, true)
	var adjusts, impossible int
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("step %d differs between runs of one seed", i)
		}
		if a[i].Gap%time.Microsecond != 0 {
			t.Fatalf("step %d: gap %v is not whole microseconds", i, a[i].Gap)
		}
		if a[i].Adjust {
			adjusts++
		}
		if a[i].Cost > l.Burst {
			impossible++
		}
	}
	if adjusts == 0 || impossible == 0 {
		t.Fatalf("trace lacks adjusts (%d) or impossible costs (%d)", adjusts, impossible)
	}
}

package ratelimittest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

// TokenBucket is the reference model for one key, formulated as a token
// bucket rather than a theoretical arrival time: a level of tokens and the
// time it was last refilled, capacity Burst, refilled at Rate tokens per
// Period. The two formulations are equivalent (tokens = Burst − (TAT −
// now) / T), and keeping the oracle in the other one means a shared
// misreading of GCRA cannot pass unnoticed.
//
// Arithmetic is exact: time is whole microseconds and the level is kept in
// token·Period units (tokens × Period in µs), so a refill of dt µs adds
// exactly dt × Rate. The zero value is a full bucket.
type TokenBucket struct {
	level   int64 // tokens × Period µs
	last    int64 // µs since the Unix epoch
	started bool
}

// refill brings the bucket up to now and returns the period in µs.
func (b *TokenBucket) refill(now time.Time, l ratelimit.Limit) int64 {
	p := l.Period.Microseconds()
	capacity := l.Burst * p
	t := now.UnixMicro()
	if !b.started {
		b.started, b.level, b.last = true, capacity, t
		return p
	}
	if dt := t - b.last; dt > 0 {
		// Compare before multiplying so a long idle stretch cannot
		// overflow.
		if dt >= (capacity-b.level)/l.Rate+1 {
			b.level = capacity
		} else {
			b.level = min(capacity, b.level+dt*l.Rate)
		}
		b.last = t
	}
	return p
}

// remaining is the whole tokens in the bucket, never below zero.
func (b *TokenBucket) remaining(p int64) int64 {
	return max(b.level/p, 0)
}

// Allow takes cost tokens if the bucket holds them.
func (b *TokenBucket) Allow(now time.Time, l ratelimit.Limit, cost int64) ratelimit.Decision {
	p := b.refill(now, l)
	need := cost * p
	switch {
	case cost > l.Burst:
		return ratelimit.Decision{Remaining: b.remaining(p), RetryAfter: ratelimit.Never}
	case need <= b.level:
		b.level -= need
		return ratelimit.Decision{Allowed: true, Remaining: b.remaining(p)}
	default:
		// Time for the refill to cover the shortfall, rounded up to
		// the next whole microsecond.
		wait := (need - b.level + l.Rate - 1) / l.Rate
		return ratelimit.Decision{Remaining: b.remaining(p), RetryAfter: time.Duration(wait) * time.Microsecond}
	}
}

// Adjust takes delta more tokens (a charge, which may leave the bucket
// negative) or returns -delta tokens (a refund, which never fills it past
// capacity).
func (b *TokenBucket) Adjust(now time.Time, l ratelimit.Limit, delta int64) {
	p := b.refill(now, l)
	b.level = min(b.level-delta*p, l.Burst*p)
}

// RunOracle drives the limiter and a TokenBucket per trace with the same
// seeded random traces, Adjusts included, and requires the same decision,
// Remaining and Retry-After at every step. Each trace gets a fresh
// harness.
func RunOracle(t *testing.T, name string, newHarness func(t *testing.T) Harness) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		for _, l := range boundLimits {
			t.Run(limitName(l), func(t *testing.T) {
				for seed := uint64(1); seed <= traces; seed++ {
					h := newHarness(t)
					if h.Set == nil {
						t.Skipf("%s: needs a settable clock", name)
					}
					d := newDriver(t, name, h)
					key := fmt.Sprintf("oracle-%d", seed)
					var want TokenBucket
					for i, s := range Trace(l, seed, traceSteps, true) {
						d.advance(s.Gap)
						if s.Adjust {
							d.check(h.Limiter.Adjust(context.Background(), key, l, s.Delta),
								fmt.Sprintf("seed %d step %d: Adjust(%d)", seed, i, s.Delta))
							want.Adjust(d.now, l, s.Delta)
							continue
						}
						got := d.allow(key, l, s.Cost)
						if exp := want.Allow(d.now, l, s.Cost); got != exp {
							t.Fatalf("%s: seed %d step %d at %v: Allow(cost %d) = %+v, oracle %+v",
								name, seed, i, d.now.Sub(Start), s.Cost, got, exp)
						}
					}
				}
			})
		}
	})
}

package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// GCRA is the generic cell rate algorithm over Redis: one Lua script per
// call, O(1) state per key, burst and sustained rate in one number pair,
// and an exact Retry-After. It meets the Limit.Bound conformance property
// that FixedWindow does not.
//
// An implementation must meet this contract (the ratelimittest contract,
// bound and oracle suites check it; ratelimittest.TokenBucket is the
// reference model):
//
//   - State is one theoretical arrival time (TAT) per key, an integer
//     number of microseconds on Redis's clock, stored at
//     prefix + "rl:gcra:{" + key + "}" (the hash tag keeps the key in one
//     cluster slot).
//   - Time is read with redis.call('TIME') inside the script, never passed
//     in by the replica, so replicas with skewed clocks agree.
//   - The emission interval is T = Limit.Interval() and the tolerance is
//     τ = Burst·T, so a fresh key admits Burst units at one instant and one
//     more unit every T after that.
//   - Allow computes tat' = max(TAT, now) + cost·T and admits iff
//     tat' − now ≤ τ, storing tat'. A refusal stores nothing.
//   - Remaining, after the decision, is floor((now + τ − TAT) / T) on the
//     stored TAT, and never below zero (a key in debt has none).
//   - RetryAfter of a refusal is tat' − now − τ: after exactly that long,
//     with nothing else arriving, the same request is admitted. It is Never
//     when cost > Burst, since no TAT can make room for it.
//   - Adjust sets TAT = max(TAT, now) + delta·T. A charge may push TAT past
//     now + τ (debt); a refund never moves TAT below now, so unused
//     capacity is not banked beyond Burst.
//   - The key expires when its TAT has drained: TTL = TAT − now (at least
//     1 ms), after which a missing key reads as TAT = now.
//
// Until it is implemented, Allow and Adjust validate their arguments and
// return ErrNotImplemented.
type GCRA struct {
	rdb    redis.Scripter
	prefix string
}

// NewGCRA returns a GCRA limiter storing its state under prefix in rdb.
func NewGCRA(rdb redis.Scripter, prefix string) *GCRA {
	return &GCRA{rdb: rdb, prefix: prefix}
}

var _ Limiter = (*GCRA)(nil)

// Allow implements Limiter.
func (g *GCRA) Allow(_ context.Context, _ string, l Limit, cost int64) (Decision, error) {
	if err := checkArgs(l, cost); err != nil {
		return Decision{}, err
	}
	return Decision{}, fmt.Errorf("gcra allow: %w", ErrNotImplemented)
}

// Adjust implements Limiter.
func (g *GCRA) Adjust(_ context.Context, _ string, l Limit, _ int64) error {
	if err := l.Validate(); err != nil {
		return err
	}
	return fmt.Errorf("gcra adjust: %w", ErrNotImplemented)
}

// MemoryGCRA is GCRA with the TATs in process memory, under the same
// contract with time read from now instead of Redis. Until it is
// implemented, Allow and Adjust validate their arguments and return
// ErrNotImplemented.
type MemoryGCRA struct {
	now func() time.Time
}

// NewMemoryGCRA returns an in-process GCRA limiter reading time from now
// (time.Now when nil).
func NewMemoryGCRA(now func() time.Time) *MemoryGCRA {
	if now == nil {
		now = time.Now
	}
	return &MemoryGCRA{now: now}
}

var _ Limiter = (*MemoryGCRA)(nil)

// Allow implements Limiter.
func (g *MemoryGCRA) Allow(_ context.Context, _ string, l Limit, cost int64) (Decision, error) {
	if err := checkArgs(l, cost); err != nil {
		return Decision{}, err
	}
	return Decision{}, fmt.Errorf("memory gcra allow: %w", ErrNotImplemented)
}

// Adjust implements Limiter.
func (g *MemoryGCRA) Adjust(_ context.Context, _ string, l Limit, _ int64) error {
	if err := l.Validate(); err != nil {
		return err
	}
	return fmt.Errorf("memory gcra adjust: %w", ErrNotImplemented)
}

// New returns the Redis limiter for a configured algorithm name.
func New(algorithm string, rdb redis.Scripter, prefix string) (Limiter, error) {
	switch algorithm {
	case AlgorithmFixedWindow:
		return NewFixedWindow(rdb, prefix), nil
	case AlgorithmGCRA:
		return NewGCRA(rdb, prefix), nil
	default:
		return nil, fmt.Errorf("ratelimit: unknown algorithm %q: want %q or %q",
			algorithm, AlgorithmFixedWindow, AlgorithmGCRA)
	}
}

// NewMemory returns the in-process limiter for a configured algorithm name,
// reading time from now (time.Now when nil).
func NewMemory(algorithm string, now func() time.Time) (Limiter, error) {
	switch algorithm {
	case AlgorithmFixedWindow:
		return NewMemoryFixedWindow(now), nil
	case AlgorithmGCRA:
		return NewMemoryGCRA(now), nil
	default:
		return nil, fmt.Errorf("ratelimit: unknown algorithm %q: want %q or %q",
			algorithm, AlgorithmFixedWindow, AlgorithmGCRA)
	}
}

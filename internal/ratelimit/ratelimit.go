// Package ratelimit admits or refuses units of work (requests, tokens) per
// key against a sustained rate and a burst, with the state shared by every
// replica through Redis.
//
// A Limit means: at most Burst units at one instant, refilled at Rate units
// per Period. Over any window of length W a conforming limiter admits at
// most Burst + Rate*W/Period units. GCRA meets that bound exactly; the
// fixed-window baseline does not (it can admit twice its per-window
// allowance across a window boundary), which is what Q2 measures.
//
// Time comes from the store: the Redis implementations read Redis's own
// clock inside their scripts, so replicas with skewed clocks still agree.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Limit is a sustained rate with a burst allowance.
type Limit struct {
	// Rate units are admitted per Period, sustained.
	Rate   int64
	Period time.Duration
	// Burst is the most units admitted at one instant.
	Burst int64
}

// Validate reports whether l is usable.
func (l Limit) Validate() error {
	switch {
	case l.Rate <= 0:
		return fmt.Errorf("ratelimit: rate must be positive, got %d", l.Rate)
	case l.Period <= 0:
		return fmt.Errorf("ratelimit: period must be positive, got %v", l.Period)
	case l.Burst <= 0:
		return fmt.Errorf("ratelimit: burst must be positive, got %d", l.Burst)
	}
	return nil
}

// Interval is the time one unit of the sustained rate takes to refill
// (GCRA's emission interval), Period / Rate.
func (l Limit) Interval() time.Duration {
	return l.Period / time.Duration(l.Rate)
}

// Bound is the most units a conforming limiter admits in any window of
// length w: Burst + Rate*w/Period, rounded down.
func (l Limit) Bound(w time.Duration) int64 {
	return l.Burst + int64(float64(l.Rate)*float64(w)/float64(l.Period))
}

// Never is the RetryAfter of a request that can never be admitted under
// the limit, because its cost alone is more than the limit allows at once.
const Never time.Duration = -1

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed bool
	// Remaining is how many more units could be admitted right now, after
	// this decision.
	Remaining int64
	// RetryAfter, when the request was refused, is how long until the same
	// request would be admitted if nothing else arrived in between; Never
	// when it never would. Zero when the request was allowed.
	RetryAfter time.Duration
}

// Limiter is the interface every algorithm implements. Implementations are
// safe for concurrent use, and every replica sharing a store sees one
// limit per key.
type Limiter interface {
	// Allow admits cost units for key under l, or refuses them without
	// consuming anything. cost is at least 1.
	Allow(ctx context.Context, key string, l Limit, cost int64) (Decision, error)
	// Adjust corrects an earlier admission once the true cost is known:
	// a positive delta charges more units (it may push the key into debt,
	// so later requests wait longer), a negative delta refunds units.
	// Token limits are admitted on an estimate and adjusted after usage
	// arrives.
	Adjust(ctx context.Context, key string, l Limit, delta int64) error
}

// ErrNotImplemented is returned by an algorithm that has an interface but
// no implementation yet.
var ErrNotImplemented = errors.New("ratelimit: not implemented")

// Algorithms the configuration can name.
const (
	AlgorithmFixedWindow = "fixed_window"
	AlgorithmGCRA        = "gcra"
)

// Package ratelimittest is the conformance kit for ratelimit.Limiter
// implementations: a contract every limiter meets (RunContract), the
// sliding-window bound only GCRA meets (RunBound), and a reference token
// bucket the GCRA implementations must agree with decision for decision
// (TokenBucket, RunOracle).
//
// The suites drive the store's clock through Harness.Set and work in whole
// microseconds, the precision of Redis's TIME. Every limit they use has an
// Interval (Period / Rate) that is a whole number of microseconds, for
// example 100 per minute = 600 ms, so the exact arithmetic of the oracle
// and of a TAT in microseconds coincide.
//
// A limiter that returns ratelimit.ErrNotImplemented makes the suites skip
// rather than fail; any other error fails them.
package ratelimittest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

// Harness is one limiter under test with control of its store's clock.
type Harness struct {
	Limiter ratelimit.Limiter
	// Set sets the store's clock (miniredis SetTime, a Clock for the
	// in-memory limiters). Nil means the store runs on the real clock, as
	// a real Redis does: the suites then run only the properties that
	// need no time travel, on a limit long enough that the real clock
	// moving does not refill anything.
	Set func(time.Time)
}

// Clock is a settable clock for in-memory limiters, safe for concurrent
// use.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock reading t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// Start is the time the suites set the clock to before each property. It
// is deliberately not on a round second, so no window boundary of the
// limits used falls on it by accident.
var Start = time.Date(2026, 3, 14, 15, 9, 26, 535_897_000, time.UTC)

// tick is the clock resolution the suites probe Retry-After with.
const tick = time.Microsecond

// longLimit refills nothing over the duration of a test on a real clock.
var longLimit = ratelimit.Limit{Rate: 10, Period: 1000 * time.Hour, Burst: 4}

// contractLimits are the limits RunContract checks with a settable clock.
var contractLimits = []ratelimit.Limit{
	longLimit,
	{Rate: 100, Period: time.Minute, Burst: 20},
	{Rate: 10, Period: time.Second, Burst: 10},
}

// limitName names a limit in subtest names.
func limitName(l ratelimit.Limit) string {
	return fmt.Sprintf("%dper%v_burst%d", l.Rate, l.Period, l.Burst)
}

// driver runs one property against one harness.
type driver struct {
	t    *testing.T
	name string
	h    Harness
	now  time.Time
}

func newDriver(t *testing.T, name string, h Harness) *driver {
	t.Helper()
	if h.Limiter == nil {
		t.Fatalf("%s: harness has no limiter", name)
	}
	d := &driver{t: t, name: name, h: h, now: Start}
	if h.Set != nil {
		h.Set(Start)
	}
	return d
}

// needClock skips a property that needs time travel on a real clock.
func (d *driver) needClock() {
	d.t.Helper()
	if d.h.Set == nil {
		d.t.Skipf("%s: needs a settable clock", d.name)
	}
}

// check fails on an error, or skips on ErrNotImplemented.
func (d *driver) check(err error, what string) {
	d.t.Helper()
	if errors.Is(err, ratelimit.ErrNotImplemented) {
		d.t.Skipf("%s: not implemented yet", d.name)
	}
	if err != nil {
		d.t.Fatalf("%s: %s: %v", d.name, what, err)
	}
}

func (d *driver) allow(key string, l ratelimit.Limit, cost int64) ratelimit.Decision {
	d.t.Helper()
	dec, err := d.h.Limiter.Allow(context.Background(), key, l, cost)
	d.check(err, fmt.Sprintf("Allow(%q, %v, %d)", key, l, cost))
	return dec
}

func (d *driver) adjust(key string, l ratelimit.Limit, delta int64) {
	d.t.Helper()
	d.check(d.h.Limiter.Adjust(context.Background(), key, l, delta),
		fmt.Sprintf("Adjust(%q, %v, %d)", key, l, delta))
}

// advance moves the store's clock forward by dt.
func (d *driver) advance(dt time.Duration) {
	d.now = d.now.Add(dt)
	d.h.Set(d.now)
}

// mustAllow asserts the request is admitted with the given Remaining.
func (d *driver) mustAllow(key string, l ratelimit.Limit, cost, remaining int64) {
	d.t.Helper()
	dec := d.allow(key, l, cost)
	if !dec.Allowed || dec.Remaining != remaining || dec.RetryAfter != 0 {
		d.t.Fatalf("%s: Allow(%q, cost %d) = %+v, want allowed with remaining %d", d.name, key, cost, dec, remaining)
	}
}

// mustRefuse asserts the request is refused with a finite Retry-After and
// returns the decision.
func (d *driver) mustRefuse(key string, l ratelimit.Limit, cost int64) ratelimit.Decision {
	d.t.Helper()
	dec := d.allow(key, l, cost)
	if dec.Allowed || dec.RetryAfter < tick {
		d.t.Fatalf("%s: Allow(%q, cost %d) = %+v, want refused with a Retry-After of at least %v", d.name, key, cost, dec, tick)
	}
	return dec
}

// capacity is what a fresh key admits at one instant, measured on a
// scratch key with one unit (Remaining after it, plus the unit itself).
func (d *driver) capacity(l ratelimit.Limit) int64 {
	d.t.Helper()
	dec := d.allow("capacity-probe", l, 1)
	if !dec.Allowed {
		d.t.Fatalf("%s: a fresh key refused one unit: %+v", d.name, dec)
	}
	c := dec.Remaining + 1
	if c > l.Rate+l.Burst {
		d.t.Fatalf("%s: capacity %d exceeds rate %d + burst %d", d.name, c, l.Rate, l.Burst)
	}
	return c
}

// exhaust admits units on key one at a time until it refuses, checking
// Remaining counts down by one each time, and returns how many it took.
func (d *driver) exhaust(key string, l ratelimit.Limit) int64 {
	d.t.Helper()
	var n, last int64
	for {
		dec := d.allow(key, l, 1)
		if !dec.Allowed {
			if dec.Remaining != 0 {
				d.t.Fatalf("%s: refused one unit with %d remaining", d.name, dec.Remaining)
			}
			if n > 0 && last != 0 {
				d.t.Fatalf("%s: refused one unit after reporting %d remaining", d.name, last)
			}
			return n
		}
		n++
		if n > l.Rate+l.Burst {
			d.t.Fatalf("%s: admitted more than rate + burst at one instant", d.name)
		}
		if n > 1 && dec.Remaining != last-1 {
			d.t.Fatalf("%s: unit %d left %d remaining, want %d", d.name, n, dec.Remaining, last-1)
		}
		last = dec.Remaining
	}
}

// RunContract checks the properties every Limiter meets, each on a fresh
// harness from newHarness: argument validation, Remaining counting down
// honestly, independent keys, refusals consuming nothing, an honest
// Retry-After, Never for impossible costs, and Adjust refunds and charges.
func RunContract(t *testing.T, name string, newHarness func(t *testing.T) Harness) {
	t.Helper()
	props := []struct {
		name string
		run  func(d *driver, l ratelimit.Limit)
	}{
		{"validation", contractValidation},
		{"remaining", contractRemaining},
		{"independent_keys", contractIndependentKeys},
		{"refusal_consumes_nothing", contractRefusalConsumesNothing},
		{"retry_after_honest", contractRetryAfter},
		{"never", contractNever},
		{"adjust_refund", contractRefund},
		{"adjust_charge", contractCharge},
	}
	t.Run(name, func(t *testing.T) {
		for _, p := range props {
			t.Run(p.name, func(t *testing.T) {
				for _, l := range contractLimits {
					t.Run(limitName(l), func(t *testing.T) {
						h := newHarness(t)
						if h.Set == nil && l != longLimit {
							t.Skipf("%s: needs a settable clock", name)
						}
						p.run(newDriver(t, name, h), l)
					})
				}
			})
		}
	})
}

func contractValidation(d *driver, l ratelimit.Limit) {
	d.t.Helper()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		l    ratelimit.Limit
		cost int64
	}{
		{"zero cost", l, 0},
		{"negative cost", l, -1},
		{"zero rate", ratelimit.Limit{Period: l.Period, Burst: l.Burst}, 1},
		{"zero period", ratelimit.Limit{Rate: l.Rate, Burst: l.Burst}, 1},
		{"zero burst", ratelimit.Limit{Rate: l.Rate, Period: l.Period}, 1},
	} {
		_, err := d.h.Limiter.Allow(ctx, "validation", tc.l, tc.cost)
		if err == nil || errors.Is(err, ratelimit.ErrNotImplemented) {
			d.t.Errorf("%s: Allow with %s: err = %v, want a validation error", d.name, tc.name, err)
		}
		if tc.cost == 1 {
			err := d.h.Limiter.Adjust(ctx, "validation", tc.l, 1)
			if err == nil || errors.Is(err, ratelimit.ErrNotImplemented) {
				d.t.Errorf("%s: Adjust with %s: err = %v, want a validation error", d.name, tc.name, err)
			}
		}
	}
}

func contractRemaining(d *driver, l ratelimit.Limit) {
	c := d.capacity(l)
	if n := d.exhaust("k", l); n != c {
		d.t.Fatalf("%s: admitted %d units one at a time, capacity is %d", d.name, n, c)
	}
	// The whole capacity in one request, and nothing after it.
	d.mustAllow("whole", l, c, 0)
	d.mustRefuse("whole", l, 1)
}

func contractIndependentKeys(d *driver, l ratelimit.Limit) {
	c := d.capacity(l)
	d.exhaust("a", l)
	d.mustRefuse("a", l, 1)
	d.mustAllow("b", l, 1, c-1)
	d.exhaust("b", l)
	d.mustAllow("c", l, c, 0)
}

func contractRefusalConsumesNothing(d *driver, l ratelimit.Limit) {
	c := d.capacity(l)
	// More than remains is refused, and what remains is still there.
	d.mustAllow("k", l, 1, c-1)
	for range 20 {
		if dec := d.mustRefuse("k", l, c); dec.Remaining != c-1 {
			d.t.Fatalf("%s: refusal reports %d remaining, want %d", d.name, dec.Remaining, c-1)
		}
	}
	d.mustAllow("k", l, c-1, 0)
	// Hammering an exhausted key does not push its Retry-After out.
	first := d.mustRefuse("k", l, 1)
	for range 20 {
		dec := d.mustRefuse("k", l, 1)
		if dec.Remaining != 0 || dec.RetryAfter > first.RetryAfter ||
			(d.h.Set != nil && dec.RetryAfter != first.RetryAfter) {
			d.t.Fatalf("%s: repeated refusal = %+v after %+v", d.name, dec, first)
		}
	}
}

func contractRetryAfter(d *driver, l ratelimit.Limit) {
	d.needClock()
	c := d.capacity(l)
	for _, tc := range []struct {
		name  string
		setup func(key string)
		cost  int64
	}{
		{"exhausted, one unit", func(k string) { d.exhaust(k, l) }, 1},
		{"exhausted, full capacity", func(k string) { d.exhaust(k, l) }, c},
		{"partly used, more than remains", func(k string) { d.mustAllow(k, l, 1, c-1) }, c},
		{"in debt", func(k string) { d.adjust(k, l, 2*c) }, 1},
		{"in deep debt, full capacity", func(k string) { d.adjust(k, l, 5*c) }, c},
	} {
		key := "retry-" + tc.name
		tc.setup(key)
		ra := d.mustRefuse(key, l, tc.cost).RetryAfter
		d.advance(ra - tick)
		if dec := d.mustRefuse(key, l, tc.cost); dec.RetryAfter != tick {
			d.t.Fatalf("%s: %s: %v before the Retry-After of %v: %+v, want refused with %v left",
				d.name, tc.name, tick, ra, dec, tick)
		}
		d.advance(tick)
		if dec := d.allow(key, l, tc.cost); !dec.Allowed {
			d.t.Fatalf("%s: %s: refused at its Retry-After of %v: %+v", d.name, tc.name, ra, dec)
		}
	}
}

func contractNever(d *driver, l ratelimit.Limit) {
	c := d.capacity(l)
	for _, cost := range []int64{c + 1, l.Rate + l.Burst + 1, 1 << 40} {
		dec := d.allow("k", l, cost)
		if dec.Allowed || dec.RetryAfter != ratelimit.Never || dec.Remaining != c {
			d.t.Fatalf("%s: cost %d with capacity %d = %+v, want refused, Never, %d remaining", d.name, cost, c, dec, c)
		}
	}
	if d.h.Set != nil {
		d.advance(10 * l.Period)
		if dec := d.allow("k", l, c+1); dec.RetryAfter != ratelimit.Never {
			d.t.Fatalf("%s: cost %d became possible with time: %+v", d.name, c+1, dec)
		}
	}
	d.mustAllow("k", l, c, 0)
}

func contractRefund(d *driver, l ratelimit.Limit) {
	c := d.capacity(l)
	d.exhaust("k", l)
	d.adjust("k", l, -2)
	d.mustAllow("k", l, 1, 1)
	d.mustAllow("k", l, 1, 0)
	d.mustRefuse("k", l, 1)
	// A refund on a fresh key banks nothing beyond capacity.
	d.adjust("fresh", l, -c)
	d.mustAllow("fresh", l, c, 0)
	d.mustRefuse("fresh", l, 1)
	// A zero adjustment is a no-op.
	d.adjust("zero", l, 0)
	d.mustAllow("zero", l, c, 0)
}

func contractCharge(d *driver, l ratelimit.Limit) {
	c := d.capacity(l)
	d.adjust("k", l, c)
	if dec := d.mustRefuse("k", l, 1); dec.Remaining != 0 {
		d.t.Fatalf("%s: charged key reports %d remaining", d.name, dec.Remaining)
	}
	// A partial charge leaves exactly the rest.
	d.adjust("partial", l, 1)
	d.mustAllow("partial", l, c-1, 0)
	d.mustRefuse("partial", l, 1)
}

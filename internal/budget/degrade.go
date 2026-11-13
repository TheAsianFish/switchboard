package budget

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"sync"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// ErrUnavailable is returned by Degrading.Balance while the primary ledger
// is unreachable: the grace allowance knows nothing about the real balance.
var ErrUnavailable = errors.New("budget store unavailable")

// DefaultGraceRefill is how long a key's grace allowance takes to refill
// from empty when DegradeOptions.GraceRefill is not set.
const DefaultGraceRefill = time.Hour

// DegradeOptions configures a Degrading ledger.
type DegradeOptions struct {
	// Grace is the capacity of each key's grace allowance on this replica:
	// the most the key may have reserved at once while the primary is
	// unreachable. The allowance refills over GraceRefill and is not reset
	// when an outage ends. Zero refuses everything during an outage.
	Grace cost.Nanos
	// GraceRefill is how long the allowance takes to refill from empty, at
	// a steady Grace per GraceRefill; DefaultGraceRefill if not positive.
	GraceRefill time.Duration
	// RetryInterval is how long to stay on the grace allowance after a
	// primary failure before probing again; default 1s.
	RetryInterval time.Duration
	// Timeout bounds every primary call; default 100ms.
	Timeout time.Duration
	// OnState is called on every transition, with true on entering
	// degraded mode and false on recovery. It is called with an internal
	// lock held, so transitions arrive in order; it must not call back
	// into the ledger.
	OnState func(degraded bool)
	// Now is the clock; time.Now if nil.
	Now func() time.Time
}

// Degrading is a Ledger over a shared primary (Redis) that keeps serving
// when the primary is unreachable, following the degrade policy: a
// gateway that refuses everything when its store blinks is a bigger outage
// than the store, but unlimited spend during an outage is not acceptable
// either.
//
// While degraded, a key's reservations are admitted against a grace
// allowance held in this process's memory: a token bucket per key with
// capacity Grace that refills at Grace per GraceRefill. Admitting a
// reservation takes its amount from the bucket, and settling it gives back
// what the hold over-estimated (or takes what it under-estimated). The
// bucket is not reset when an outage ends, so however often the primary
// flaps, one replica process admits at most Grace × (1 + T / GraceRefill)
// without the primary over a stretch of length T that starts with no
// grace hold open (an outage after the primary was reachable), plus
// whatever requests cost above their estimates. A stretch that starts
// while grace holds are open can gain up to one more Grace, since settling
// a hold admitted before the stretch gives back what it over-estimated.
// A fleet multiplies that by the number of
// replica processes, not replicas: a restarted replica starts with a full
// allowance.
//
// Costs settled while degraded are kept as pending charges per key and
// window and are charged to the primary on the first successful primary
// call after recovery. Pending charges live only in this process's memory:
// a replica killed while degraded takes its unreported spend with it,
// which is at most what its allowance admitted plus the cost of primary
// holds it settled during the outage. That includes requests admitted on
// grace and still in flight when the process dies: their cost was never
// pending and never held on the primary, so only this process knew it.
// A lost reply on a reserve gets there without an outage, since the
// failed call sends the request to grace while the primary keeps an
// orphaned hold that expires on its own.
//
// A reservation held on the primary and settled while degraded cannot be
// released there; its cost goes to pending and the hold expires by TTL, so
// until then the key is over-reserved by it, which errs on refusing. A
// primary hold whose replica died before settling is also released by
// TTL, and its cost never reaches the primary's spent.
//
// A primary call that times out after the primary applied it is
// indistinguishable from one that never arrived; a settle in that state is
// charged again from pending, so an outage can over-count spend.
type Degrading struct {
	primary Ledger
	grace   cost.Nanos
	refill  time.Duration
	retry   time.Duration
	timeout time.Duration
	onState func(bool)
	now     func() time.Time

	mu       sync.Mutex
	degraded bool
	retryAt  time.Time
	// buckets holds the grace allowance of every key below full; a key
	// with no bucket has a full allowance. They are keyed by key alone,
	// not by budget window: the allowance bounds what this process admits
	// without the primary over time, and a key whose daily window rolls
	// over mid-outage must not get a second allowance.
	buckets  map[string]*graceBucket
	pending  map[accountKey]pendingCharge
	flushing bool
	// failures counts primary failures; a success ends degraded mode only
	// if none was recorded after its call started.
	failures uint64
}

// graceBucket is one key's grace allowance.
type graceBucket struct {
	// tokens is what the key may still reserve. It goes below zero when a
	// grace hold settles above its estimate; refill pays that back first.
	tokens cost.Nanos
	// last is when tokens was last refilled.
	last time.Time
	// rem is the remainder of the last refill's division by the refill
	// period, so frequent refills lose nothing to rounding.
	rem uint64
}

// pendingCharge is cost settled while degraded, not yet on the primary.
type pendingCharge struct {
	budget Budget
	amount cost.Nanos
}

var _ Ledger = (*Degrading)(nil)

// NewDegrading wraps primary with the degrade policy.
func NewDegrading(primary Ledger, opts DegradeOptions) *Degrading {
	d := &Degrading{
		primary: primary,
		grace:   max(opts.Grace, 0),
		refill:  opts.GraceRefill,
		retry:   opts.RetryInterval,
		timeout: opts.Timeout,
		onState: opts.OnState,
		now:     opts.Now,
		buckets: make(map[string]*graceBucket),
		pending: make(map[accountKey]pendingCharge),
	}
	if d.refill <= 0 {
		d.refill = DefaultGraceRefill
	}
	if d.retry <= 0 {
		d.retry = time.Second
	}
	if d.timeout <= 0 {
		d.timeout = 100 * time.Millisecond
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.onState == nil {
		d.onState = func(bool) {}
	}
	return d
}

// Degraded reports whether the ledger is currently on its grace allowance.
func (d *Degrading) Degraded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.degraded
}

// GraceRemaining reports what key may still reserve on this replica while
// the primary is unreachable. It is negative while the key pays back a
// grace hold that settled above its estimate.
func (d *Degrading) GraceRemaining(key string) cost.Nanos {
	d.mu.Lock()
	defer d.mu.Unlock()
	g := d.bucket(key, d.now())
	tokens := g.tokens
	d.tidy(key, g)
	return tokens
}

// bucket returns key's grace allowance refilled up to now, creating a full
// one if the key has none. Callers hold mu and call tidy when done.
func (d *Degrading) bucket(key string, now time.Time) *graceBucket {
	g := d.buckets[key]
	if g == nil {
		g = &graceBucket{tokens: d.grace, last: now}
		d.buckets[key] = g
		return g
	}
	g.refill(now, d.grace, d.refill)
	return g
}

// tidy forgets key's bucket once it is full, since a missing bucket is a
// full one; the map then holds only keys that spent grace recently.
// Callers hold mu.
func (d *Degrading) tidy(key string, g *graceBucket) {
	if g.tokens >= d.grace {
		delete(d.buckets, key)
	}
}

// refill adds grace per period for the time since g.last, up to grace. A
// clock that went backwards adds nothing.
func (g *graceBucket) refill(now time.Time, grace cost.Nanos, period time.Duration) {
	if !now.After(g.last) {
		return
	}
	elapsed := now.Sub(g.last)
	g.last = now
	if grace <= 0 || g.tokens >= grace {
		g.tokens, g.rem = min(g.tokens, grace), 0
		return
	}
	// Each whole period adds grace. Stop as soon as that fills the bucket,
	// so a long gap cannot overflow.
	if elapsed >= period {
		n := int64(elapsed / period)
		elapsed %= period
		deficit := int64(grace - g.tokens)
		need := deficit / int64(grace)
		if deficit%int64(grace) != 0 {
			need++
		}
		if n >= need {
			g.tokens, g.rem = grace, 0
			return
		}
		g.tokens += cost.Nanos(n) * grace
	}
	// The rest of a period adds grace × elapsed / period, worked out in 128
	// bits. With elapsed < period and rem < period the dividend is below
	// (grace + 1) × period, so the quotient fits in 64 bits and Div64
	// cannot panic.
	hi, lo := bits.Mul64(uint64(grace), uint64(elapsed))
	lo, carry := bits.Add64(lo, g.rem, 0)
	q, r := bits.Div64(hi+carry, lo, uint64(period))
	g.tokens += cost.Nanos(q)
	g.rem = r
	if g.tokens >= grace {
		g.tokens, g.rem = grace, 0
	}
}

// tryPrimary reports whether a call should go to the primary, and the
// failure count it started at. While degraded, one caller per
// RetryInterval is let through as a probe.
func (d *Degrading) tryPrimary() (bool, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.degraded {
		return true, d.failures
	}
	now := d.now()
	if now.Before(d.retryAt) {
		return false, 0
	}
	d.retryAt = now.Add(d.retry)
	return true, d.failures
}

// failed classifies err from a primary call made under ctx. Exhaustion and
// the caller's own cancellation say nothing about the store; anything else
// (including the per-call timeout) puts the ledger in degraded mode.
func (d *Degrading) failed(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, ErrExhausted) || ctx.Err() != nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.retryAt = d.now().Add(d.retry)
	d.failures++
	if !d.degraded {
		d.degraded = true
		telemetry.Logger(ctx).Warn("budget store unreachable, using per-replica grace",
			"error", err, "grace", d.grace.String(), "grace_refill", d.refill.String())
		d.onState(true)
	}
	return true
}

// succeeded runs after a primary call that reached the store: it charges
// pending costs and, once none are left, leaves degraded mode. Recovery
// does not touch the grace allowance, which keeps refilling at its own
// pace. A call that started before the latest failure (seen is older)
// proves nothing about the store now and changes nothing.
func (d *Degrading) succeeded(ctx context.Context, seen uint64) {
	d.mu.Lock()
	if (!d.degraded && len(d.pending) == 0) || d.flushing || d.failures != seen {
		d.mu.Unlock()
		return
	}
	d.flushing = true
	d.mu.Unlock()

	flushed, err := d.flush(ctx)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushing = false
	if err != nil {
		d.retryAt = d.now().Add(d.retry)
		telemetry.Logger(ctx).Warn("budget pending charges not flushed",
			"error", err, "flushed", flushed.String())
		return
	}
	if d.degraded && len(d.pending) == 0 {
		d.degraded = false
		// Warn like the failure: a fleet logging at warn records both
		// ends of every outage.
		telemetry.Logger(ctx).Warn("budget store recovered", "flushed", flushed.String())
		d.onState(false)
	}
}

// flush charges pending costs to the primary one entry at a time. An entry
// is reduced by what was charged only after the primary accepted it, so a
// failure part way keeps exactly what is still owed, and costs added while
// the flush runs are kept for the next one.
func (d *Degrading) flush(ctx context.Context) (cost.Nanos, error) {
	var total cost.Nanos
	// The caller may give up on its request; the charge is still owed.
	ctx = context.WithoutCancel(ctx)
	for {
		d.mu.Lock()
		var (
			k     accountKey
			p     pendingCharge
			found bool
		)
		for k, p = range d.pending {
			found = true
			break
		}
		d.mu.Unlock()
		if !found {
			return total, nil
		}
		cctx, cancel := context.WithTimeout(ctx, d.timeout)
		err := d.primary.Settle(cctx, k.key, p.budget, Reservation{Window: k.window}, p.amount)
		cancel()
		if err != nil {
			return total, fmt.Errorf("budget flush %s window %s: %w", k.key, k.window, err)
		}
		total += p.amount
		d.mu.Lock()
		left := d.pending[k]
		left.amount -= p.amount
		if left.amount == 0 {
			delete(d.pending, k)
		} else {
			d.pending[k] = left
		}
		d.mu.Unlock()
	}
}

// Reserve implements Ledger. While the primary is unreachable it admits
// against key's grace allowance, with Memory's rule for zero amounts, and
// fails with an error wrapping ErrExhausted once the allowance is short.
func (d *Degrading) Reserve(ctx context.Context, key string, b Budget, id string, amount cost.Nanos) (Reservation, error) {
	// Reject bad input here so it never reaches the primary and is never
	// mistaken for a store failure.
	if err := checkReserve(b, id, amount); err != nil {
		return Reservation{}, err
	}
	if try, seen := d.tryPrimary(); try {
		cctx, cancel := context.WithTimeout(ctx, d.timeout)
		r, err := d.primary.Reserve(cctx, key, b, id, amount)
		cancel()
		if !d.failed(ctx, err) {
			if err == nil || errors.Is(err, ErrExhausted) {
				d.succeeded(ctx, seen)
			}
			return r, err
		}
	}
	now := d.now()
	window := b.Window(now)
	d.mu.Lock()
	defer d.mu.Unlock()
	g := d.bucket(key, now)
	if amount > g.tokens || (amount == 0 && g.tokens <= 0) {
		err := exhausted(key, window, g.tokens, amount)
		d.tidy(key, g)
		return Reservation{}, fmt.Errorf("budget store unreachable, grace: %w", err)
	}
	g.tokens -= amount
	d.tidy(key, g)
	return Reservation{ID: id, Window: window, Amount: amount, grace: true}, nil
}

// Settle implements Ledger. A grace hold adjusts key's grace allowance by
// hold − actual whenever it settles, during the outage or after it.
func (d *Degrading) Settle(ctx context.Context, key string, b Budget, r Reservation, actual cost.Nanos) error {
	if err := checkSettle(b, actual); err != nil {
		return err
	}
	if r.Window == "" {
		r.Window = b.Window(d.now())
	}
	// The money was spent whether or not the caller is still waiting, so
	// its cancellation must not drop the charge.
	ctx = context.WithoutCancel(ctx)
	if r.grace {
		// The cost was admitted without the primary's consent, so it
		// counts against the allowance: give back what the hold
		// over-estimated, or take what it under-estimated. Tokens may go
		// negative, which is honest about a low estimate; refill pays it
		// back before the key is admitted again.
		d.mu.Lock()
		g := d.bucket(key, d.now())
		g.tokens += r.Amount - actual
		d.tidy(key, g)
		d.mu.Unlock()
	}
	if try, seen := d.tryPrimary(); try {
		pr := r
		if r.grace {
			// The primary never saw this hold; charge the cost plainly.
			pr = Reservation{Window: r.Window}
		}
		cctx, cancel := context.WithTimeout(ctx, d.timeout)
		err := d.primary.Settle(cctx, key, b, pr, actual)
		cancel()
		if err == nil {
			d.succeeded(ctx, seen)
			return nil
		}
		if !d.failed(ctx, err) {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if actual > 0 {
		k := accountKey{key, r.Window}
		p := d.pending[k]
		p.budget = b
		p.amount += actual
		d.pending[k] = p
	}
	return nil
}

// Balance implements Ledger. It reports the primary's view; while the
// primary is unreachable it fails with an error wrapping ErrUnavailable.
// Pending charges are not included until they are flushed.
func (d *Degrading) Balance(ctx context.Context, key string, b Budget) (Balance, error) {
	if err := b.Validate(); err != nil {
		return Balance{}, err
	}
	try, seen := d.tryPrimary()
	if !try {
		return Balance{}, fmt.Errorf("budget balance %s: %w", key, ErrUnavailable)
	}
	cctx, cancel := context.WithTimeout(ctx, d.timeout)
	bal, err := d.primary.Balance(cctx, key, b)
	cancel()
	if err == nil {
		d.succeeded(ctx, seen)
		return bal, nil
	}
	if d.failed(ctx, err) {
		return Balance{}, fmt.Errorf("budget balance %s: %w: %w", key, ErrUnavailable, err)
	}
	return Balance{}, err
}

package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// ErrUnavailable is returned by Degrading.Balance while the primary ledger
// is unreachable: the grace ledger knows nothing about the real balance.
var ErrUnavailable = errors.New("budget store unavailable")

// DegradeOptions configures a Degrading ledger.
type DegradeOptions struct {
	// Grace is how much each key may reserve on this replica while the
	// primary is unreachable, per outage and per window. Zero refuses
	// everything during an outage.
	Grace cost.Nanos
	// RetryInterval is how long to stay on the grace ledger after a
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
// While degraded, each key may reserve up to Grace on this replica, after
// which reservations are refused. Costs settled while degraded are kept as
// pending charges per key and window and are charged to the primary on the
// first successful primary call after recovery; once they are all charged
// the grace ledger is reset, so each outage gets fresh grace. With N
// replicas an outage can overspend a key by at most N × Grace plus
// whatever the primary had already admitted.
//
// A reservation held on the primary and settled while degraded cannot be
// released there; its cost goes to pending and the hold expires by TTL, so
// until then the key is over-reserved by it, which errs on refusing.
//
// A primary call that times out after the primary applied it is
// indistinguishable from one that never arrived; a settle in that state is
// charged again from pending, so an outage can over-count spend (never
// under-count it).
type Degrading struct {
	primary Ledger
	grace   cost.Nanos
	retry   time.Duration
	timeout time.Duration
	onState func(bool)
	now     func() time.Time

	mu       sync.Mutex
	degraded bool
	retryAt  time.Time
	local    *Memory
	gen      uint64
	pending  map[accountKey]pendingCharge
	flushing bool
	// failures counts primary failures; a success ends degraded mode only
	// if none was recorded after its call started.
	failures uint64
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
		grace:   opts.Grace,
		retry:   opts.RetryInterval,
		timeout: opts.Timeout,
		onState: opts.OnState,
		now:     opts.Now,
		pending: make(map[accountKey]pendingCharge),
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
	d.gen = 1
	d.local = NewMemory(DefaultReservationTTL, d.now)
	return d
}

// Degraded reports whether the ledger is currently on its grace ledger.
func (d *Degrading) Degraded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.degraded
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
			"error", err, "grace", d.grace.String())
		d.onState(true)
	}
	return true
}

// succeeded runs after a primary call that reached the store: it charges
// pending costs and, once none are left, leaves degraded mode. A call
// that started before the latest failure (seen is older) proves nothing
// about the store now and changes nothing.
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
		d.gen++
		d.local = NewMemory(DefaultReservationTTL, d.now)
		telemetry.Logger(ctx).Info("budget store recovered", "flushed", flushed.String())
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

// Reserve implements Ledger.
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
	d.mu.Lock()
	local, gen := d.local, d.gen
	d.mu.Unlock()
	r, err := local.Reserve(ctx, key, Budget{Limit: d.grace, Period: b.Period}, id, amount)
	if err != nil {
		return Reservation{}, fmt.Errorf("budget store unreachable, grace: %w", err)
	}
	r.grace = gen
	return r, nil
}

// Settle implements Ledger.
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
	if try, seen := d.tryPrimary(); try {
		pr := r
		if r.grace != 0 {
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
	if r.grace == d.gen {
		// Release the grace hold and count the cost against grace. An
		// error here can only be a validation error, already ruled out.
		_ = d.local.Settle(ctx, key, Budget{Limit: d.grace, Period: b.Period}, r, actual)
	}
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

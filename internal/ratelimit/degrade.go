package ratelimit

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Degrading is the degrade policy for limits: it uses Primary (the shared
// Redis limiter) and, when Primary fails, falls back to Local (a
// per-replica in-memory limiter) at the limit divided by the replica
// count, so the fleet as a whole still admits roughly the configured rate
// while the store is down. Refusing everything when Redis blinks would be
// a bigger outage than Redis; admitting everything would be unlimited
// spend.
//
// After a failure it stays on Local for RetryInterval, then lets one call
// probe Primary again; a probe that succeeds ends the degraded state. An
// error caused by the caller's own context, ErrNotImplemented and argument
// errors are returned to the caller rather than treated as a store
// failure.
//
// Units admitted on one side are not known to the other: an Adjust that
// lands on the other side of a transition corrects the wrong counter. That
// error is bounded by one RetryInterval of traffic and accepted.
//
// Set the exported fields before first use; Degrading is safe for
// concurrent use after that.
type Degrading struct {
	Primary Limiter
	Local   Limiter
	// Replicas divides the limit for Local; values below 1 mean 1.
	Replicas int
	// RetryInterval is how long to stay on Local after a Primary failure
	// before probing Primary again; zero means one second.
	RetryInterval time.Duration
	// Timeout is the deadline applied to each Primary call; zero means
	// 100 ms.
	Timeout time.Duration
	// OnState, if set, is called on each transition with the new state
	// (true when entering degraded mode). It is called with an internal
	// lock held, so transitions are reported in order; it must be quick
	// and must not call back into the Degrading.
	OnState func(degraded bool)
	// Now is the clock for RetryInterval; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	degraded bool
	probeAt  time.Time // when degraded, the earliest next Primary call
}

// Defaults for the zero values of the Degrading fields.
const (
	DefaultRetryInterval = time.Second
	DefaultTimeout       = 100 * time.Millisecond
)

// NewDegrading returns a Degrading over primary and local with the default
// RetryInterval and Timeout.
func NewDegrading(primary, local Limiter, replicas int) *Degrading {
	return &Degrading{
		Primary:       primary,
		Local:         local,
		Replicas:      replicas,
		RetryInterval: DefaultRetryInterval,
		Timeout:       DefaultTimeout,
	}
}

var _ Limiter = (*Degrading)(nil)

// LocalLimit is the per-replica limit Local enforces in place of l: Rate
// and Burst divided by the replica count, rounded down, at least 1.
func (d *Degrading) LocalLimit(l Limit) Limit {
	n := int64(max(d.Replicas, 1))
	return Limit{Rate: max(l.Rate/n, 1), Period: l.Period, Burst: max(l.Burst/n, 1)}
}

// Degraded reports whether calls are currently going to Local.
func (d *Degrading) Degraded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.degraded
}

// Allow implements Limiter.
func (d *Degrading) Allow(ctx context.Context, key string, l Limit, cost int64) (Decision, error) {
	if err := checkArgs(l, cost); err != nil {
		return Decision{}, err
	}
	var dec Decision
	local, err := d.tryPrimary(ctx, func(ctx context.Context) error {
		var err error
		dec, err = d.Primary.Allow(ctx, key, l, cost)
		return err
	})
	if !local {
		return dec, err
	}
	dec, err = d.Local.Allow(ctx, key, d.LocalLimit(l), cost)
	if err == nil && dec.RetryAfter == Never && cost <= max(l.Rate, l.Burst) {
		// Too big for this replica's share, though the full limit may
		// take it: that is the outage's fault, not the request's, so it
		// is refused as rate limited until Redis can be tried again. A
		// cost above both Rate and Burst fits no algorithm's limit and
		// stays Never.
		dec.RetryAfter = d.retryInterval()
	}
	return dec, err
}

// Adjust implements Limiter.
func (d *Degrading) Adjust(ctx context.Context, key string, l Limit, delta int64) error {
	if err := l.Validate(); err != nil {
		return err
	}
	local, err := d.tryPrimary(ctx, func(ctx context.Context) error {
		return d.Primary.Adjust(ctx, key, l, delta)
	})
	if !local {
		return err
	}
	return d.Local.Adjust(ctx, key, d.LocalLimit(l), delta)
}

// tryPrimary runs call against Primary under Timeout unless the limiter is
// degraded and not yet due a probe. It reports whether the caller should
// use Local instead; when it should not, err is Primary's result.
func (d *Degrading) tryPrimary(ctx context.Context, call func(context.Context) error) (local bool, err error) {
	if !d.shouldTry() {
		return true, nil
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	err = call(pctx)
	cancel()
	switch {
	case err == nil:
		d.recover(ctx)
		return false, nil
	case ctx.Err() != nil, errors.Is(err, ErrNotImplemented):
		// The caller gave up, or the algorithm does not exist: neither
		// says anything about the store.
		return false, err
	}
	d.fail(ctx, err)
	return true, nil
}

// shouldTry reports whether this call goes to Primary. While degraded,
// one call per RetryInterval is let through as a probe.
func (d *Degrading) shouldTry() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.degraded {
		return true
	}
	now := d.now()
	if now.Before(d.probeAt) {
		return false
	}
	d.probeAt = now.Add(d.retryInterval())
	return true
}

func (d *Degrading) fail(ctx context.Context, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probeAt = d.now().Add(d.retryInterval())
	if d.degraded {
		return
	}
	d.degraded = true
	telemetry.Logger(ctx).Warn("rate limit store unavailable, using per-replica limits",
		"error", err, "replicas", max(d.Replicas, 1), "retry_interval", d.retryInterval())
	if d.OnState != nil {
		d.OnState(true)
	}
}

func (d *Degrading) recover(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.degraded {
		return
	}
	d.degraded = false
	telemetry.Logger(ctx).Info("rate limit store recovered, using shared limits")
	if d.OnState != nil {
		d.OnState(false)
	}
}

func (d *Degrading) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Degrading) retryInterval() time.Duration {
	if d.RetryInterval > 0 {
		return d.RetryInterval
	}
	return DefaultRetryInterval
}

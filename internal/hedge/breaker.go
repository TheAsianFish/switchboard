package hedge

import (
	"sync"
	"time"
)

// State is a circuit breaker's state.
type State int

// Breaker states. The numeric values are what the
// switchboard_provider_breaker_state gauge reports.
const (
	// Closed: requests flow; consecutive failures are counted.
	Closed State = iota
	// HalfOpen: the cooldown has passed; one probe request at a time is
	// let through to find out whether the provider has recovered.
	HalfOpen
	// Open: requests fail fast without reaching the provider.
	Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half_open"
	case Open:
		return "open"
	default:
		return "unknown"
	}
}

// BreakerConfig sets when a breaker opens and for how long.
type BreakerConfig struct {
	// Failures is the number of consecutive failed attempts that opens
	// the breaker; 0 disables it.
	Failures int
	// Cooldown is how long an open breaker refuses before it lets a probe
	// through, and how long a probe has to report before another is let
	// through.
	Cooldown time.Duration
}

// Breaker is a per-provider circuit breaker. It counts consecutive failed
// attempts (overloaded, timed out, upstream errors; never the caller's own
// mistakes or cancellations), opens after cfg.Failures of them, and after
// the cooldown lets one probe through at a time until one succeeds.
//
// It is per replica: each replica learns a provider is down from its own
// traffic, which takes cfg.Failures requests per replica. Sharing the state
// through Redis would make every request pay a round trip to save a handful
// of failures.
type Breaker struct {
	cfg      BreakerConfig
	now      func() time.Time
	onChange func(State)

	mu         sync.Mutex
	state      State
	failures   int
	openedAt   time.Time
	probeUntil time.Time
}

// NewBreaker returns a closed breaker. onChange, if not nil, is called
// with the new state on every transition, outside the breaker's lock.
func NewBreaker(cfg BreakerConfig, now func() time.Time, onChange func(State)) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{cfg: cfg, now: now, onChange: onChange}
}

// Allow reports whether a request may go to the provider now. In the
// half-open state it admits one probe per cooldown.
func (b *Breaker) Allow() bool {
	if b == nil || b.cfg.Failures <= 0 {
		return true
	}
	b.mu.Lock()
	now := b.now()
	changed := b.advance(now)
	allow := false
	switch b.state {
	case Closed:
		allow = true
	case HalfOpen:
		if !now.Before(b.probeUntil) {
			b.probeUntil = now.Add(b.cfg.Cooldown)
			allow = true
		}
	}
	st := b.state
	b.mu.Unlock()
	if changed {
		b.notify(st)
	}
	return allow
}

// State returns the breaker's current state.
func (b *Breaker) State() State {
	if b == nil || b.cfg.Failures <= 0 {
		return Closed
	}
	b.mu.Lock()
	changed := b.advance(b.now())
	st := b.state
	b.mu.Unlock()
	if changed {
		b.notify(st)
	}
	return st
}

// RetryAfter is how long an open breaker will keep refusing.
func (b *Breaker) RetryAfter() time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != Open {
		return 0
	}
	return max(0, b.openedAt.Add(b.cfg.Cooldown).Sub(b.now()))
}

// advance moves an open breaker whose cooldown has passed to half-open.
// The caller holds mu.
func (b *Breaker) advance(now time.Time) bool {
	if b.state == Open && !now.Before(b.openedAt.Add(b.cfg.Cooldown)) {
		b.state = HalfOpen
		b.probeUntil = time.Time{}
		return true
	}
	return false
}

// Success records a successful attempt: the breaker closes.
func (b *Breaker) Success() {
	if b == nil || b.cfg.Failures <= 0 {
		return
	}
	b.mu.Lock()
	changed := b.state != Closed
	b.state, b.failures = Closed, 0
	b.mu.Unlock()
	if changed {
		b.notify(Closed)
	}
}

// Failure records a failed attempt. A failed probe reopens the breaker at
// once; in the closed state it opens after cfg.Failures in a row.
func (b *Breaker) Failure() {
	if b == nil || b.cfg.Failures <= 0 {
		return
	}
	b.mu.Lock()
	now := b.now()
	b.advance(now)
	opened := false
	switch b.state {
	case HalfOpen:
		b.state, b.openedAt, opened = Open, now, true
	case Closed:
		b.failures++
		if b.failures >= b.cfg.Failures {
			b.state, b.openedAt, opened = Open, now, true
		}
	}
	b.mu.Unlock()
	if opened {
		b.notify(Open)
	}
}

func (b *Breaker) notify(s State) {
	if b.onChange != nil {
		b.onChange(s)
	}
}

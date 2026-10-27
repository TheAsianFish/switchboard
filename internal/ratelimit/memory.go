package ratelimit

import (
	"context"
	"sync"
	"time"
)

// MemoryFixedWindow is FixedWindow with the counters in process memory:
// the same windows, decisions and Retry-After, but per replica. It is the
// fallback Degrading uses while Redis is unreachable, and it is held to
// the same tests as the Redis script.
type MemoryFixedWindow struct {
	now func() time.Time

	mu      sync.Mutex
	windows map[string]fwWindow
	// sweepAt is the map size that triggers the next sweep of expired
	// windows; doubling it after each sweep keeps the cost amortised O(1).
	sweepAt int
}

// fwWindow is one key's current window.
type fwWindow struct {
	period int64 // µs; a different period starts a fresh window
	index  int64
	count  int64
}

const minSweep = 1024

// NewMemoryFixedWindow returns an in-process fixed-window limiter reading
// time from now (time.Now when nil).
func NewMemoryFixedWindow(now func() time.Time) *MemoryFixedWindow {
	if now == nil {
		now = time.Now
	}
	return &MemoryFixedWindow{now: now, windows: make(map[string]fwWindow), sweepAt: minSweep}
}

var _ Limiter = (*MemoryFixedWindow)(nil)

// current returns key's window at nowUS, fresh if the stored one is from an
// earlier window or a different period. Callers hold m.mu.
func (m *MemoryFixedWindow) current(key string, p, nowUS int64) fwWindow {
	idx := nowUS / p
	w, ok := m.windows[key]
	if !ok || w.period != p || w.index != idx {
		return fwWindow{period: p, index: idx}
	}
	return w
}

// store saves w, dropping it when empty, and sweeps expired windows when
// the map has grown. Callers hold m.mu.
func (m *MemoryFixedWindow) store(key string, w fwWindow, nowUS int64) {
	if w.count == 0 {
		delete(m.windows, key)
		return
	}
	m.windows[key] = w
	if len(m.windows) < m.sweepAt {
		return
	}
	for k, v := range m.windows {
		if v.index < nowUS/v.period {
			delete(m.windows, k)
		}
	}
	m.sweepAt = max(minSweep, 2*len(m.windows))
}

// Allow implements Limiter.
func (m *MemoryFixedWindow) Allow(_ context.Context, key string, l Limit, cost int64) (Decision, error) {
	if err := checkArgs(l, cost); err != nil {
		return Decision{}, err
	}
	p := periodMicros(l)
	m.mu.Lock()
	defer m.mu.Unlock()
	nowUS := m.now().UnixMicro()
	w := m.current(key, p, nowUS)
	switch {
	case cost > l.Rate:
		return Decision{Remaining: max(l.Rate-w.count, 0), RetryAfter: Never}, nil
	case w.count+cost <= l.Rate:
		w.count += cost
		m.store(key, w, nowUS)
		return Decision{Allowed: true, Remaining: max(l.Rate-w.count, 0)}, nil
	default:
		retry := (w.index+1)*p - nowUS
		return Decision{Remaining: max(l.Rate-w.count, 0), RetryAfter: time.Duration(retry) * time.Microsecond}, nil
	}
}

// Adjust implements Limiter.
func (m *MemoryFixedWindow) Adjust(_ context.Context, key string, l Limit, delta int64) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if delta == 0 {
		return nil
	}
	p := periodMicros(l)
	m.mu.Lock()
	defer m.mu.Unlock()
	nowUS := m.now().UnixMicro()
	w := m.current(key, p, nowUS)
	w.count = max(w.count+delta, 0)
	m.store(key, w, nowUS)
	return nil
}

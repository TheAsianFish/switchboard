package hedge

import (
	"math"
	"slices"
	"sync"
	"time"
)

// Window keeps the last n latency samples and answers a quantile of them.
// The quantile is recomputed after every n/20 new samples rather than per
// request: sorting a thousand durations costs tens of microseconds, and
// the delay it sets does not need to move faster than that.
type Window struct {
	mu      sync.Mutex
	buf     []time.Duration
	next    int
	full    bool
	q       float64
	cached  time.Duration
	stale   int
	refresh int
	scratch []time.Duration
}

// NewWindow keeps the last n samples and answers their q quantile.
func NewWindow(n int, q float64) *Window {
	if n < 1 {
		n = 1
	}
	return &Window{buf: make([]time.Duration, n), q: q, refresh: max(1, n/20)}
}

// Add records a sample.
func (w *Window) Add(d time.Duration) {
	w.mu.Lock()
	w.buf[w.next] = d
	w.next++
	if w.next == len(w.buf) {
		w.next, w.full = 0, true
	}
	w.stale++
	w.mu.Unlock()
}

// Len is the number of samples held.
func (w *Window) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.len()
}

func (w *Window) len() int {
	if w.full {
		return len(w.buf)
	}
	return w.next
}

// Quantile returns the window's q quantile (nearest rank) and the number
// of samples it was computed from.
func (w *Window) Quantile() (time.Duration, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.len()
	if n == 0 {
		return 0, 0
	}
	if w.stale >= w.refresh || w.cached == 0 {
		w.scratch = append(w.scratch[:0], w.buf[:n]...)
		slices.Sort(w.scratch)
		w.cached = w.scratch[rank(n, w.q)]
		w.stale = 0
	}
	return w.cached, n
}

// rank is the nearest-rank index of quantile q in n sorted samples.
func rank(n int, q float64) int {
	// The epsilon keeps n·q from landing just above an integer through
	// floating-point error, as bench.Percentile does.
	i := int(math.Ceil(float64(n)*q-1e-9)) - 1
	return min(max(i, 0), n-1)
}

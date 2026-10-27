// Package limitseval analyses the Q2 runs of `make test-limits`: how many
// requests got past a shared rate limit, read from the fake provider's
// ledger of what actually reached it, and whether the gateway's per-key
// spend matches that ledger.
package limitseval

import (
	"slices"
	"time"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

// Window is the busiest stretch of one length: the most admitted requests
// in any window of that length and how far that is past the limit's bound.
type Window struct {
	Seconds float64 `json:"window_seconds"`
	// StartSeconds is where the busiest window starts, relative to the
	// run's start.
	StartSeconds float64 `json:"start_seconds"`
	Admitted     int     `json:"admitted"`
	Bound        int64   `json:"bound"`
	// Over is Admitted - Bound, or 0 when the bound held.
	Over int64 `json:"over"`
}

// Busiest returns the most entries of sorted times that fall in any
// closed window [t, t+w] with t and t+w inside [from, to].
func Busiest(times []time.Time, w time.Duration, from, to time.Time) (int, time.Time) {
	best, at := 0, from
	j := 0
	for i := range times {
		if times[i].Before(from) {
			continue
		}
		end := times[i].Add(w)
		if end.After(to) {
			// Windows that run past the end are clipped to it; a later
			// start would only see fewer entries.
			end = to
		}
		if j < i {
			j = i
		}
		for j < len(times) && !times[j].After(end) {
			j++
		}
		if n := j - i; n > best {
			best, at = n, times[i]
		}
	}
	return best, at
}

// Worst is the window, among lengths 1 s to max in whole seconds, where
// admissions exceed the limit's bound by the most, between from and to.
// Looking at every length matters: at a steady overload a fixed-window
// counter spends its whole allowance at the start of each window, so it
// can stay under the bound over 60 s and still be far over it at 20 s.
func Worst(times []time.Time, l ratelimit.Limit, max time.Duration, from, to time.Time) Window {
	sorted := slices.Clone(times)
	slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })
	var worst Window
	first := true
	for w := time.Second; w <= max && w <= to.Sub(from); w += time.Second {
		got := At(sorted, l, w, from, to)
		if first || got.Over > worst.Over || (got.Over == worst.Over && got.Admitted-int(got.Bound) > worst.Admitted-int(worst.Bound)) {
			worst, first = got, false
		}
	}
	return worst
}

// At is the busiest window of length w between from and to. times must
// be sorted.
func At(sorted []time.Time, l ratelimit.Limit, w time.Duration, from, to time.Time) Window {
	n, at := Busiest(sorted, w, from, to)
	b := l.Bound(w)
	return Window{
		Seconds:      w.Seconds(),
		StartSeconds: at.Sub(from).Seconds(),
		Admitted:     n,
		Bound:        b,
		Over:         max(int64(n)-b, 0),
	}
}

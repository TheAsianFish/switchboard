package bench

import (
	"math"
	"slices"
	"time"
)

// Stats summarises a set of durations in milliseconds. Every field is
// computed exactly from all samples; nothing is estimated from a histogram.
//
// Percentiles use the nearest-rank method: the p-th percentile of n sorted
// samples is the sample at 1-based rank ceil(p/100 * n). It is always one of
// the observed values, never an interpolation between two of them.
type Stats struct {
	Count int     `json:"count"`
	Mean  float64 `json:"mean_ms"`
	P50   float64 `json:"p50_ms"`
	P90   float64 `json:"p90_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	P999  float64 `json:"p999_ms"`
	Max   float64 `json:"max_ms"`
}

// Summarise computes Stats over samples. It sorts a copy, so the caller's
// slice is left untouched. An empty input gives a zero Stats.
func Summarise(samples []time.Duration) Stats {
	if len(samples) == 0 {
		return Stats{}
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	var sum float64
	for _, d := range s {
		sum += float64(d)
	}
	return Stats{
		Count: len(s),
		Mean:  ms(time.Duration(sum / float64(len(s)))),
		P50:   ms(Percentile(s, 50)),
		P90:   ms(Percentile(s, 90)),
		P95:   ms(Percentile(s, 95)),
		P99:   ms(Percentile(s, 99)),
		P999:  ms(Percentile(s, 99.9)),
		Max:   ms(s[len(s)-1]),
	}
}

// Percentile returns the nearest-rank p-th percentile (0 < p <= 100) of
// sorted, which must be in ascending order. It returns 0 for an empty slice.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	// The small epsilon keeps p/100*n from landing just above an integer
	// through floating-point error (99.9/100*1000 is 999.0000000000001).
	rank := int(math.Ceil(p/100*float64(n) - 1e-9))
	rank = min(max(rank, 1), n)
	return sorted[rank-1]
}

// ms converts d to milliseconds rounded to three decimals (microseconds).
func ms(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}

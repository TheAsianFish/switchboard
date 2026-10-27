package limitseval

import (
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func at(secs ...float64) []time.Time {
	out := make([]time.Time, len(secs))
	for i, s := range secs {
		out[i] = t0.Add(time.Duration(s * float64(time.Second)))
	}
	return out
}

func TestBusiest(t *testing.T) {
	t.Parallel()
	times := at(0, 0.5, 1, 1, 3, 10, 10.2)
	tests := []struct {
		w     time.Duration
		want  int
		start float64
	}{
		{time.Second, 4, 0}, // [0, 1] closed holds 0, 0.5, 1, 1
		{500 * time.Millisecond, 3, 0.5},
		{2 * time.Second, 4, 0},
		{20 * time.Second, 7, 0},
	}
	for _, tt := range tests {
		n, start := Busiest(times, tt.w, t0, t0.Add(time.Minute))
		if n != tt.want || start.Sub(t0).Seconds() != tt.start {
			t.Errorf("Busiest(%v) = %d at %v, want %d at %v", tt.w, n, start.Sub(t0), tt.want, tt.start)
		}
	}
	// Windows stay inside [from, to].
	if n, _ := Busiest(times, 20*time.Second, t0.Add(time.Second), t0.Add(5*time.Second)); n != 3 {
		t.Errorf("clipped Busiest = %d, want 3 (1, 1, 3)", n)
	}
}

// A fixed-window counter at 3× its allowance spends 100 requests in the
// first 20 s of each minute: under the 60 s bound (120), far over the 20 s
// bound (20 + 33 = 53).
func TestWorstFindsFixedWindowBurst(t *testing.T) {
	t.Parallel()
	var secs []float64
	for m := range 3 {
		for i := range 100 {
			secs = append(secs, float64(m*60)+float64(i)*0.2)
		}
	}
	l := ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 20}
	from, to := t0, t0.Add(3*time.Minute)
	if w := At(at(secs...), l, time.Minute, from, to); w.Over != 0 || w.Admitted != 101 {
		t.Errorf("60 s window = %+v, want 101 (the closed window reaches the next minute's first request) and nothing over", w)
	}
	w := Worst(at(secs...), l, time.Minute, from, to)
	if w.Over != 47 || w.Seconds != 20 {
		t.Errorf("Worst = %+v, want 47 over at 20 s", w)
	}
}

// A conforming schedule (burst, then one per interval) is never over.
func TestWorstConforming(t *testing.T) {
	t.Parallel()
	l := ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 20}
	var secs []float64
	for range 20 {
		secs = append(secs, 0)
	}
	for i := 1; i <= 280; i++ {
		secs = append(secs, float64(i)*0.6)
	}
	if w := Worst(at(secs...), l, time.Minute, t0, t0.Add(3*time.Minute)); w.Over != 0 {
		t.Errorf("Worst = %+v, want nothing over", w)
	}
}

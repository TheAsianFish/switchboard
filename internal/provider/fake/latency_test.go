package fake

import (
	"slices"
	"testing"
	"time"
)

func TestLatencyDistribution(t *testing.T) {
	t.Parallel()
	p := New(Config{Latency: 100 * time.Millisecond, LatencySigma: 0.25, SlowFraction: 0.05, SlowFactor: 10, Seed: 7})
	const n = 20000
	ds := make([]time.Duration, n)
	slow := 0
	for i := range ds {
		ds[i] = p.latency()
		// exp(0.25·4) < 3: only the slow tail gets past 5× the median.
		if ds[i] > 500*time.Millisecond {
			slow++
		}
	}
	slices.Sort(ds)
	if med := ds[n/2]; med < 95*time.Millisecond || med > 110*time.Millisecond {
		t.Errorf("median %v, want about 100ms (slightly above: the slow 5%% shift it)", med)
	}
	if share := float64(slow) / n; share < 0.045 || share > 0.055 {
		t.Errorf("slow share %.4f, want about 0.05", share)
	}
	a, b := New(p.cfg), New(p.cfg)
	for i := range 100 {
		if x, y := a.latency(), b.latency(); x != y {
			t.Fatalf("draw %d differs across providers with one seed: %v, %v", i, x, y)
		}
	}
	if d := New(Config{Latency: time.Second}).latency(); d != time.Second {
		t.Errorf("fixed latency drew %v", d)
	}
}

func TestModelTokenInterval(t *testing.T) {
	t.Parallel()
	c := Config{TokenInterval: 5 * time.Millisecond, ModelTokenInterval: map[string]time.Duration{"slow": 70 * time.Millisecond}}
	tests := []struct {
		model string
		want  time.Duration
	}{
		{"slow", 70 * time.Millisecond},
		{"fast", 5 * time.Millisecond},
		{"", 5 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := c.tokenInterval(tt.model); got != tt.want {
			t.Errorf("tokenInterval(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

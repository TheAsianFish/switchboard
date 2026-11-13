package results

import (
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/chaos"
)

func TestM13Rule(t *testing.T) {
	t.Parallel()
	arm := func(iv string, timedOut int, after ...float64) *M13Retry {
		r := &M13Retry{Interval: iv, afterReady: after, timedOut: timedOut}
		r.AfterReady = m13Range(after, "%.2f")
		return r
	}
	tests := []struct {
		name  string
		retry []*M13Retry
		want  string
	}{
		{"separate and no more timeouts", []*M13Retry{arm("1s", 30, 1.1, 1.2, 1.3), arm("250ms", 30, 0.5, 0.6, 0.7)}, "250ms"},
		{"overlapping ranges", []*M13Retry{arm("1s", 30, 1.1, 1.2, 1.3), arm("250ms", 10, 0.5, 0.6, 1.15)}, "1s"},
		{"more timeouts", []*M13Retry{arm("1s", 30, 1.1, 1.2, 1.3), arm("250ms", 31, 0.5, 0.6, 0.7)}, "1s"},
		{"too few runs", []*M13Retry{arm("1s", 30, 1.1, 1.2), arm("250ms", 3, 0.5, 0.6, 0.7)}, "1s"},
		{"one arm", []*M13Retry{arm("1s", 30, 1.1, 1.2, 1.3)}, "1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			text, got := m13Rule(tt.retry)
			if got != tt.want || text == "" {
				t.Fatalf("m13Rule = %q, %q; want default %q", text, got, tt.want)
			}
		})
	}
}

// The breaker reading carries a replica's failed dials from an outage
// that did not trip it into the next one.
func TestM13Breaker(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	s := &chaos.OutageSummary{Hardware: bench.Hardware{GOMAXPROCS: 4}, Outages: []chaos.RedisOutage{
		{Replicas: []chaos.ReplicaDials{{Replica: "a", DialsBefore: 45, FirstFastSeconds: f(3)}, {Replica: "b", DialsBefore: 30}}},
		{Replicas: []chaos.ReplicaDials{{Replica: "a", DialsBefore: 20}, {Replica: "b", DialsBefore: 12, FirstFastSeconds: f(1.5)}}},
		{Replicas: []chaos.ReplicaDials{{Replica: "a", DialsBefore: 25}}},
	}}
	b := m13Breaker(s)
	if b.PoolSize != 40 || b.ReplicaOutages != 5 || b.Tripped != 2 || b.Carried != 1 || b.MinAtTrip != 42 || b.MaxAtTrip != 45 {
		t.Fatalf("breaker = %+v", b)
	}
	if len(b.Untripped) != 1 || !strings.Contains(b.Untripped[0], "a in outage 3 (20 carried + 25)") {
		t.Fatalf("untripped = %v", b.Untripped)
	}
	if b.FirstFast != "1.50–3.00 s" {
		t.Fatalf("first fast = %q", b.FirstFast)
	}
}

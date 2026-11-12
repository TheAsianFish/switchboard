package results

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
)

// M9BenchRepeatDir is the subdirectory of M9Dir holding a second
// `make bench-persist` pass (OUT_ROOT=eval/results/m9-persist/bench-repeat).
// It sets each mode's gateway run against the same mode's gateway run in
// another pass, which the direct runs the pre-stated check uses as its
// drift cannot.
const M9BenchRepeatDir = "bench-repeat"

// M9Second is the second bench pass beside the first.
type M9Second struct {
	// Runs are the second pass's bench runs, for their meta lines.
	Runs []*M9Run
	// Rows: per mode, "pass 1 / pass 2" for every added latency and cost.
	Rows []M9Row
	// Moves: per metric and percentile, the largest change of a mode's
	// added latency between the passes ("gateway drift"), and which mode.
	Moves []M9Move
	// Checks re-reads the latency hypotheses with both passes.
	Checks []M9SecondCheck
}

// M9Move is how far one metric and percentile moved between passes.
type M9Move struct {
	Metric, Largest, Mode string
	// PerMode is "none 0.04, everysec 0.03, always 0.02".
	PerMode string

	v float64
}

// M9SecondCheck is one cost of one mode in both passes, against the
// threshold and the gateway drift for that metric and percentile.
type M9SecondCheck struct {
	Mode, Metric, Pass1, Pass2, Drift, Threshold, Reading string
}

var m9Metrics = []struct {
	name   string
	metric int
	pct    int // 0 p50, 1 p99
}{
	{"total p50", m9Total, 0},
	{"total p99", m9Total, 1},
	{"TTFT p50", m9TTFT, 0},
	{"TTFT p99", m9TTFT, 1},
}

func (d diff) point(pct int) (float64, bool) {
	if !d.ok {
		return 0, false
	}
	if pct == 0 {
		return d.v50, true
	}
	return d.v99, true
}

// loadM9Second reads the second pass under dir and sets it against the
// first (m.Runs' benches, costs already computed). Nil without a second
// pass.
func loadM9Second(dir string, m *M9) (*M9Second, error) {
	s := &M9Second{}
	found := false
	byMode := map[string]*M9Bench{}
	for _, mode := range M9Modes {
		b, err := loadM9Bench(filepath.Join(dir, M9BenchRepeatDir, mode), mode)
		if err != nil {
			return nil, err
		}
		if b != nil {
			found = true
			byMode[mode] = b
			s.Runs = append(s.Runs, &M9Run{Mode: mode, Bench: b})
		}
	}
	if !found {
		return nil, nil
	}
	none := byMode["none"]
	for _, b := range byMode {
		if b == none || none == nil {
			continue
		}
		for k := range 2 {
			b.cost[k] = b.added[k].minus(none.added[k])
		}
		b.CostTotalP50, b.CostTotalP99 = b.cost[m9Total].ests()
		b.CostTTFTP50, b.CostTTFTP99 = b.cost[m9TTFT].ests()
	}

	first := map[string]*M9Bench{}
	for _, r := range m.Runs {
		if r.Bench != nil {
			first[r.Mode] = r.Bench
		}
	}
	pair := func(get func(b *M9Bench) Est) []string {
		var out []string
		for _, mode := range M9Modes {
			a, b := "–", "–"
			if f := first[mode]; f != nil {
				a = get(f).Point()
			}
			if r := byMode[mode]; r != nil {
				b = get(r).Point()
			}
			out = append(out, a+" / "+b)
		}
		return out
	}
	s.Rows = []M9Row{
		{"Added p50, non-streaming total (ms)", pair(func(b *M9Bench) Est { return b.TotalP50 })},
		{"Added p99, non-streaming total (ms)", pair(func(b *M9Bench) Est { return b.TotalP99 })},
		{"Added p50, streaming TTFT (ms)", pair(func(b *M9Bench) Est { return b.TTFTP50 })},
		{"Added p99, streaming TTFT (ms)", pair(func(b *M9Bench) Est { return b.TTFTP99 })},
		{"Cost against `none`, p50 total (ms)", pair(func(b *M9Bench) Est { return b.CostTotalP50 })},
		{"Cost against `none`, p99 total (ms)", pair(func(b *M9Bench) Est { return b.CostTotalP99 })},
		{"Cost against `none`, p50 TTFT (ms)", pair(func(b *M9Bench) Est { return b.CostTTFTP50 })},
		{"Cost against `none`, p99 TTFT (ms)", pair(func(b *M9Bench) Est { return b.CostTTFTP99 })},
	}

	// The gateway drift: how far a mode's added latency moved between
	// the passes, the largest over the modes measured in both.
	moves := make([]float64, len(m9Metrics))
	movesOK := make([]bool, len(m9Metrics))
	for i, mt := range m9Metrics {
		mv := M9Move{Metric: mt.name, Largest: "n/a"}
		var parts []string
		for _, mode := range M9Modes {
			f, r := first[mode], byMode[mode]
			if f == nil || r == nil {
				continue
			}
			a, okA := f.added[mt.metric].point(mt.pct)
			b, okB := r.added[mt.metric].point(mt.pct)
			if !okA || !okB {
				continue
			}
			d := math.Abs(a - b)
			parts = append(parts, fmt.Sprintf("`%s` %s", mode, msf(d)))
			if !movesOK[i] || d > moves[i] {
				moves[i], movesOK[i], mv.Mode = d, true, mode
			}
		}
		if movesOK[i] {
			mv.Largest, mv.v = msf(moves[i]), moves[i]
		}
		mv.PerMode = strings.Join(parts, ", ")
		s.Moves = append(s.Moves, mv)
	}

	// The latency hypotheses with both passes: everysec under 0.1 ms on
	// all four, always under 0.2 ms at p99.
	for _, h := range []struct {
		mode      string
		threshold float64
		pcts      []int // indexes into m9Metrics
	}{
		{"everysec", 0.1, []int{0, 1, 2, 3}},
		{"always", 0.2, []int{1, 3}},
	} {
		f, r := first[h.mode], byMode[h.mode]
		if f == nil || r == nil {
			continue
		}
		for _, i := range h.pcts {
			mt := m9Metrics[i]
			c1, ok1 := f.cost[mt.metric].point(mt.pct)
			c2, ok2 := r.cost[mt.metric].point(mt.pct)
			if !ok1 || !ok2 || !movesOK[i] {
				continue
			}
			s.Checks = append(s.Checks, M9SecondCheck{
				Mode: h.mode, Metric: mt.name,
				Pass1: added(c1), Pass2: added(c2), Drift: msf(moves[i]),
				Threshold: msf(h.threshold),
				Reading:   m9SecondReading(c1, c2, moves[i], h.threshold),
			})
		}
	}
	return s, nil
}

// m9SecondReading judges one cost measured in two passes against a
// threshold, given how far the gateway runs moved between the passes. A
// cost is resolved only if both passes are outside that movement and on
// the same side of the threshold.
func m9SecondReading(c1, c2, drift, threshold float64) string {
	switch {
	case math.Min(math.Abs(c1), math.Abs(c2)) <= drift:
		return "unresolved: within the gateway drift"
	case c1 < threshold && c2 < threshold:
		return "under in both passes"
	case c1 >= threshold && c2 >= threshold:
		return fmt.Sprintf("over in both passes, by at least %s ms", msf(math.Min(c1, c2)-threshold))
	default:
		return "unresolved: the passes disagree"
	}
}

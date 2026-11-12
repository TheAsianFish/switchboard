package results

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/chaos"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

func TestM9Verdict(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                   string
		cost, drift, threshold float64
		want                   string
		excess                 float64
	}{
		{"inside the drift", 0.05, 0.09, 0.1, "unresolved", 0},
		{"equal to the drift", 0.09, 0.09, 0.1, "unresolved", 0},
		{"negative inside the drift", -0.08, 0.09, 0.1, "unresolved", 0},
		{"above the drift, under the threshold", 0.095, 0.09, 0.1, "yes", 0},
		{"negative beyond the drift", -0.2, 0.09, 0.1, "yes", 0},
		{"over the threshold", 0.35, 0.12, 0.2, "no", 0.15},
		{"at the threshold", 0.2, 0.12, 0.2, "no", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, excess := m9Verdict(c.cost, c.drift, c.threshold)
			if got != c.want || fmt.Sprintf("%.3f", excess) != fmt.Sprintf("%.3f", c.excess) {
				t.Errorf("m9Verdict(%v, %v, %v) = %s, %v; want %s, %v", c.cost, c.drift, c.threshold, got, excess, c.want, c.excess)
			}
		})
	}
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"yes", "yes"}, "yes"},
		{[]string{"yes", "unresolved"}, "unresolved"},
		{[]string{"unresolved", "no"}, "no"},
	} {
		if got := m9Combine(c.in...); got != c.want {
			t.Errorf("m9Combine(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

// m9In builds judging inputs: overshoots in microdollars per mode (-1 for
// a missing chaos run), p99 costs per mode as [total, TTFT] (nil for a
// missing bench run), and the p99 drift of both metrics.
func m9In(over [3]int64, p99 [3]*[2]float64, drift float64) m9Inputs {
	const grace = cost.Nanos(20_000_000)
	const bound = cost.Nanos(24_650_000)
	in := m9Inputs{driftOK: [2]bool{true, true}}
	in.drift[0] = [2]float64{drift, drift}
	in.drift[1] = [2]float64{drift, drift}
	for i := range 3 {
		var c *M9Chaos
		if over[i] >= 0 {
			o := cost.Nanos(over[i] * 1000)
			c = &M9Chaos{overshoot: o, Overshoot: m8USD(o), bound: bound, graceBound: grace, boundOK: true}
		}
		in.chaos = append(in.chaos, c)
		var cs [2][2]*float64
		if p := p99[i]; p != nil {
			for k := range 2 {
				v50, v99 := 0.0, p[k]
				cs[k] = [2]*float64{&v50, &v99}
			}
		}
		in.cost = append(in.cost, cs)
	}
	return in
}

func TestM9Rule(t *testing.T) {
	t.Parallel()
	small := &[2]float64{0.03, 0.04}
	big := &[2]float64{0.4, 0.9}
	tiny := &[2]float64{0.01, -0.02}
	for _, c := range []struct {
		name     string
		over     [3]int64
		p99      [3]*[2]float64
		drift    float64
		want     string
		decided  bool
		noPass   bool
		passes   [3]string
		inReason string
	}{
		{"none within the bound", [3]int64{20_000, 15_000, 10_000}, [3]*[2]float64{nil, small, big}, 0.1, "none", true, false,
			[3]string{"yes", "yes", "no"}, "`none` is the first"},
		{"everysec after none overshoots", [3]int64{198_000, 24_000, 18_000}, [3]*[2]float64{nil, small, big}, 0.01, "everysec", true, false,
			[3]string{"no", "yes", "no"}, "every mode before it failed"},
		{"everysec over the bound, always passes", [3]int64{198_000, 30_000, 18_000}, [3]*[2]float64{nil, small, small}, 0.01, "always", true, false,
			[3]string{"no", "no", "yes"}, "`always`"},
		{"unresolved cost passes with a small drift", [3]int64{198_000, 24_000, 18_000}, [3]*[2]float64{nil, tiny, big}, 0.12, "everysec", true, false,
			[3]string{"no", "yes", "no"}, "`everysec`"},
		{"unresolved cost fails with a large drift", [3]int64{198_000, 24_000, 18_000}, [3]*[2]float64{nil, tiny, tiny}, 0.6, "none", true, true,
			[3]string{"no", "no", "no"}, "no mode passes"},
		{"no mode passes", [3]int64{198_000, 24_000, 18_000}, [3]*[2]float64{nil, big, big}, 0.01, "none", true, true,
			[3]string{"no", "no", "no"}, "stays `none`"},
		{"a missing mode before any pass", [3]int64{198_000, -1, 18_000}, [3]*[2]float64{nil, small, small}, 0.01, "", false, false,
			[3]string{"no", "not measured", "yes"}, "undecided: `everysec`"},
		{"a missing bench before any pass", [3]int64{198_000, 24_000, 18_000}, [3]*[2]float64{nil, nil, small}, 0.01, "", false, false,
			[3]string{"no", "not measured", "yes"}, "undecided: `everysec`"},
		{"a missing mode after a pass", [3]int64{198_000, 24_000, -1}, [3]*[2]float64{nil, small, nil}, 0.01, "everysec", true, false,
			[3]string{"no", "yes", "not measured"}, "`everysec`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rows, def, reason, decided, noPass := m9Rule(m9In(c.over, c.p99, c.drift))
			if def != c.want || decided != c.decided || noPass != c.noPass {
				t.Errorf("default %q decided %v noPass %v; want %q %v %v (%s)", def, decided, noPass, c.want, c.decided, c.noPass, reason)
			}
			if !strings.Contains(reason, c.inReason) {
				t.Errorf("reason %q lacks %q", reason, c.inReason)
			}
			for i, r := range rows {
				if r.Passes != c.passes[i] {
					t.Errorf("%s passes %q, want %q (%+v)", r.Mode, r.Passes, c.passes[i], r)
				}
			}
		})
	}
}

func TestM9Checks(t *testing.T) {
	t.Parallel()
	held := func(checks []Q6Check, prefix string) string {
		for _, c := range checks {
			if strings.HasPrefix(c.Hypothesis, prefix) {
				return c.Held
			}
		}
		return "absent"
	}
	restart := func(mainErrs, other int, repeat float64) *chaos.PhaseResult {
		p := &chaos.PhaseResult{Name: M9RestartPhase, RedisUp: true}
		p.Main.Errors, p.Main.RepeatHitRate, p.Limited.OtherErrors = mainErrs, repeat, other
		return p
	}
	for _, c := range []struct {
		name                          string
		overEvery, overAlways         int64
		logs                          bool
		loading, mainErrs, other      int
		repeat                        float64
		everyCost, alwaysCost         *[2]float64
		drift                         float64
		wantOver, wantAlways          string
		wantLoading, wantHit          string
		wantEveryCost, wantAlwaysCost string
	}{
		{"all held", 24_650, 20_000, true, 3, 0, 0, 0.97, &[2]float64{0.05, 0.06}, &[2]float64{0.15, 0.1}, 0.02,
			"yes", "yes", "yes", "yes", "yes", "yes"},
		{"over the bounds, LOADING with errors", 24_651, 20_001, true, 3, 2, 0, 0.94, &[2]float64{0.3, 0.06}, &[2]float64{0.5, 0.1}, 0.02,
			"no", "no", "no", "no", "no", "no"},
		{"not exercised, cost unresolved", 10_000, 10_000, true, 0, 0, 0, 0.95, &[2]float64{0.05, 0.06}, &[2]float64{0.05, 0.1}, 0.12,
			"yes", "yes", "not exercised", "yes", "unresolved", "unresolved"},
		{"no logs", 10_000, 10_000, false, 0, 0, 1, 0.95, nil, nil, 0.1,
			"yes", "yes", "unresolved", "yes", "absent", "absent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := m9In([3]int64{198_000, c.overEvery, c.overAlways}, [3]*[2]float64{nil, c.everyCost, c.alwaysCost}, c.drift)
			if c.everyCost != nil {
				// p50 costs equal to the p99 ones, so the everysec check
				// reads both percentiles.
				for k := range 2 {
					v := c.everyCost[k]
					in.cost[1][k][0] = &v
				}
			}
			ev := in.chaos[1]
			ev.restart, ev.logs, ev.LoadingLines = restart(c.mainErrs, c.other, c.repeat), c.logs, c.loading
			ev.Summary = &chaos.Summary{Streams: []chaos.StreamInfo{{Name: "budget", Rate: 10}},
				Phases: []chaos.PhaseResult{{Budget: chaos.BudgetPhase{Admitted: 2, Spent: 930_000}}}}
			checks := m9Checks(in)
			for _, w := range []struct{ prefix, want string }{
				{"`everysec`: overshoot", c.wantOver},
				{"`always`: overshoot", c.wantAlways},
				{"`everysec`: no client-visible", c.wantLoading},
				{"`everysec`: hit rate", c.wantHit},
				{"`everysec`: adds", c.wantEveryCost},
				{"`always`: adds", c.wantAlwaysCost},
			} {
				if got := held(checks, w.prefix); got != w.want {
					t.Errorf("%s: held %q, want %q (%+v)", w.prefix, got, w.want, checks)
				}
			}
		})
	}
}

// m9Summary is a Q6-shaped run: $0.20 budget, $0.01 grace, two replicas
// up at the Redis kill, a budget stream at 10 req/s and $0.000465 a reply.
func m9Summary(mode string, overshoot cost.Nanos, loading int) chaos.Summary {
	var s chaos.Summary
	s.Meta.Commit, s.Meta.Date, s.Meta.Command = "abc1234", "2026-11-12T10:00:00Z", "REDIS_PERSIST="+mode+" make chaos"
	s.Meta.Replicas, s.Meta.DurationSeconds, s.Meta.TimeScale, s.Meta.Schedule = 3, 120, 1, chaos.ScheduleQ6
	s.Meta.RedisPersist = mode
	s.Meta.Redis = &chaos.RedisMeta{AppendOnly: "yes", AppendFsync: mode, DataFS: "overlayfs", AOFBytes: 12_345_678}
	if mode == "none" {
		s.Meta.Redis = &chaos.RedisMeta{AppendOnly: "no", DataFS: "overlayfs"}
	}
	s.Meta.Budget.USD, s.Meta.Budget.GraceUSD = "0.20", "0.01"
	s.Streams = []chaos.StreamInfo{{Name: "main", Rate: 200}, {Name: "budget", Rate: 10}}
	steady := chaos.PhaseResult{Name: "steady", ReplicasUp: 3, RedisUp: true}
	steady.Budget.Admitted, steady.Budget.Spent = 100, 46_500_000
	steady.Main.RepeatHitRate = 0.98
	down := chaos.PhaseResult{Name: "replica and Redis down", ReplicasUp: 2}
	down.Budget.Spent, down.Budget.GraceBound = 18_600_000, 20_000_000
	back := chaos.PhaseResult{Name: M9RestartPhase, ReplicasUp: 2, RedisUp: true}
	back.Main.RepeatHitRate, back.Main.ErrorRate = 0.96, 0
	back.Budget.Reconcile = &chaos.Reconcile{Unreconciled: 930_000}
	last := chaos.PhaseResult{Name: "all back", ReplicasUp: 3, RedisUp: true}
	last.Budget.Overshoot = overshoot
	s.Phases = []chaos.PhaseResult{steady, {Name: "replica down", ReplicasUp: 2, RedisUp: true}, down, back, last}
	ready, loaded, degraded, rec := 0.4, 0.25, 1.5, 1.25
	s.Store = &chaos.StoreLog{
		Outages: []chaos.OutageErrors{{Event: "kill redis", AtSeconds: 60, ToSeconds: 75, Classes: []chaos.ErrorClassCount{
			{Class: "timeout", Lines: 119, PoolLines: 4, FirstSeconds: 0.2, LastSeconds: 2.1, Example: "cache: get: context deadline exceeded"},
			{Class: "dns", Lines: 5, FirstSeconds: 1.2, LastSeconds: 14.5, Example: "dial tcp: lookup redis"},
		}}},
		Restarts: []chaos.RedisRestart{{AtSeconds: 75, ReadySeconds: &ready, LoadedSeconds: &loaded, LoadingErrors: loading,
			DegradedSeconds: &degraded,
			Recoveries:      []chaos.Recovery{{Replica: "switchboard-1", Component: "budget", Seconds: &degraded}, {Replica: "switchboard-1", Component: "cache", Seconds: &rec}},
		}},
	}
	return s
}

// m9WriteBench writes a bench-persist run whose gateway adds addMS to
// every sample over direct runs of 10 ms (+0.01 ms in the second run).
func m9WriteBench(t *testing.T, dir, mode string, addMS float64) {
	t.Helper()
	bdir := filepath.Join(dir, Q1BreakdownDir)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "meta.json"), Meta{Commit: "abc1234", Date: "2026-11-12T11:00:00Z", Command: "MODES=" + mode + " make bench-persist",
		Breakdown: &BreakdownMeta{Rate: 200, RedisPersist: mode, RedisArgs: "--appendonly x", RedisDataFS: "overlayfs"}})
	samples := func(base float64) []float64 {
		xs := make([]float64, 200)
		for i := range xs {
			xs[i] = base + float64(i%50)*0.01
		}
		return xs
	}
	res := func(stream bool, xs []float64) bench.Result {
		r := bench.Result{Requests: len(xs), Outcomes: map[string]int{bench.OutcomeOK: len(xs)}}
		r.Config.Stream, r.Config.Rate, r.Config.DurationSec, r.Config.WarmupSec = stream, 200, 30, 2
		r.Samples = &bench.Samples{LatencyMS: xs, TTFTMS: xs}
		return r
	}
	for _, mname := range []string{"nonstream", "stream"} {
		st := mname == "stream"
		writeJSON(t, filepath.Join(bdir, "direct-"+mname+"-200-1.json"), res(st, samples(10)))
		writeJSON(t, filepath.Join(bdir, "direct-"+mname+"-200-2.json"), res(st, samples(10.01)))
		writeJSON(t, filepath.Join(bdir, "budget-"+mname+"-200.json"), res(st, samples(10+addMS)))
		info := "# Persistence\naof_enabled:0\n"
		if mode != "none" {
			info = "# Persistence\naof_enabled:1\naof_current_size:2000000\naof_delayed_fsync:3\n"
		}
		if err := os.WriteFile(filepath.Join(bdir, "budget-"+mname+"-200.redis-persistence.txt"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadM9(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, M9Dir)
	for mode, over := range map[string]cost.Nanos{"none": 198_000_000, "everysec": 22_000_000} {
		d := filepath.Join(root, "chaos-"+mode)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(d, chaos.SummaryFile), m9Summary(mode, over, 0))
	}
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "none"), "none", 1)
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "everysec"), "everysec", 1.05)

	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := d.M9
	if m == nil {
		t.Fatal("no M9 section")
	}
	ev := m.Runs[1]
	if ev.Chaos == nil || ev.Bench == nil || m.Runs[2].Chaos != nil || m.Runs[2].Bench != nil {
		t.Fatalf("runs %+v", m.Runs)
	}
	c := ev.Chaos
	if c.Overshoot != "$0.022000" || c.Lost != "$0.000930" || c.RepeatHitRate != "96.0%" || c.AOFSize != "12.35 MB" ||
		c.Ready != "0.400 s" || c.Degraded != "1.500 s" || *c.FirstDNSSeconds != 1.2 || *c.LastTimeoutSeconds != 2.1 {
		t.Errorf("everysec chaos %+v", c)
	}
	if m.EverysecBound != "$0.024650" || m.GraceBound != "$0.020000" || m.PricePerReply != "$0.000465" || m.ReplicasAtKill != "2" {
		t.Errorf("bounds: everysec %s grace %s reply %s replicas %s", m.EverysecBound, m.GraceBound, m.PricePerReply, m.ReplicasAtKill)
	}
	b := ev.Bench
	if b.CostTotalP50.Point() != "+0.05" || b.CostTTFTP99.Point() != "+0.05" || b.DelayedFsync != "3 / 3" || b.AOFSize != "2.00 MB / 2.00 MB" {
		t.Errorf("everysec bench: cost %s %s, fsync %s, aof %s", b.CostTotalP50, b.CostTTFTP99, b.DelayedFsync, b.AOFSize)
	}
	if m.DriftTotalP50 != "0.01" || m.Runs[0].Bench.AOFSize != "no file / no file" {
		t.Errorf("drift %s, none aof %s", m.DriftTotalP50, m.Runs[0].Bench.AOFSize)
	}
	if m.Default != "everysec" || !m.Decided || len(m.Slow) != 2 || m.Slow[0].PoolLines != "4" {
		t.Errorf("default %q decided %v slow %+v", m.Default, m.Decided, m.Slow)
	}
	if len(m.ToMeasure) != 2 || !strings.Contains(m.ToMeasure[0], "MODES=always make chaos-persist") {
		t.Errorf("to measure %v", m.ToMeasure)
	}

	var first, second bytes.Buffer
	if err := Render(dir, tmplPath, &first); err != nil {
		t.Fatal(err)
	}
	if err := Render(dir, tmplPath, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("two renders of the same inputs differ")
	}
	out := first.String()
	for _, want := range []string{
		"| Overshoot at the end | $0.198000 | $0.022000 | – |",
		"| timeout | 119 | 4 | 0.200 | 2.100 |",
		"not exercised",
		"The rule picks `everysec`",
		"- M9 `always` latency: `MODES=always make bench-persist`, then `make results`.",
		"- `everysec` bench: commit `abc1234`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	readme, err := Execute(readmeTmplPath, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "overshoot $0.198000 with no append-only file, $0.022000 with `everysec`, – with `always`") ||
		!strings.Contains(string(readme), "compose default by the pre-stated rule: `everysec`") {
		t.Errorf("README row:\n%s", readme)
	}
}

func TestLoadM9ModeMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := filepath.Join(dir, "chaos-always")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(d, chaos.SummaryFile), m9Summary("everysec", 0, 0))
	if _, err := loadM9(dir, nil); err == nil || !strings.Contains(err.Error(), "redis_persist") {
		t.Errorf("err %v, want a redis_persist mismatch", err)
	}
	if m, err := loadM9(t.TempDir(), nil); m != nil || err != nil {
		t.Errorf("empty dir: %v, %v", m, err)
	}
}

func TestM9SecondReading(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		c1, c2, drift, thresh float64
		want                  string
	}{
		{"both under, outside the drift", -0.14, -0.19, 0.03, 0.1, "under in both passes"},
		{"one pass inside the drift", 0.07, -1.06, 1.04, 0.1, "unresolved: within the gateway drift"},
		{"either side of the threshold", 0.12, 0.07, 0.03, 0.1, "unresolved: the passes disagree"},
		{"both over", 6.18, 5.83, 1.04, 0.2, "over in both passes, by at least 5.63 ms"},
		{"exactly the drift is inside it", 0.05, 0.05, 0.05, 0.1, "unresolved: within the gateway drift"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := m9SecondReading(tt.c1, tt.c2, tt.drift, tt.thresh); got != tt.want {
				t.Errorf("m9SecondReading(%v, %v, %v, %v) = %q, want %q", tt.c1, tt.c2, tt.drift, tt.thresh, got, tt.want)
			}
		})
	}
}

// A second bench pass is set against the first: the gateway drift is the
// largest move of a mode's added latency between passes, and a cost inside
// it is unresolved even when one pass alone looks resolved.
func TestLoadM9Second(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, M9Dir)
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "none"), "none", 1)
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "everysec"), "everysec", 1.05)
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "always"), "always", 3)
	// The second pass: none's gateway run 0.5 ms slower, everysec's the
	// same, always's 0.1 ms faster.
	m9WriteBench(t, filepath.Join(root, M9BenchRepeatDir, "none"), "none", 1.5)
	m9WriteBench(t, filepath.Join(root, M9BenchRepeatDir, "everysec"), "everysec", 1.05)
	m9WriteBench(t, filepath.Join(root, M9BenchRepeatDir, "always"), "always", 2.9)

	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := d.M9
	if m == nil || m.Second == nil {
		t.Fatal("no second pass")
	}
	s := m.Second
	if mv := s.Moves[0]; mv.Largest != "0.50" || mv.Mode != "none" || mv.PerMode != "`none` 0.50, `everysec` 0.00, `always` 0.10" {
		t.Errorf("total p50 move %+v", mv)
	}
	if s.Runs[0].Bench.CostTotalP50.ok {
		t.Error("none has a cost against itself")
	}
	if got := s.Runs[1].Bench.CostTotalP50.Point(); got != "-0.45" {
		t.Errorf("second-pass everysec cost = %s, want -0.45 (against the second pass's none)", got)
	}
	want := map[string]string{
		"everysec total p50": "unresolved: within the gateway drift",
		"always total p99":   "over in both passes, by at least 1.20 ms",
	}
	for _, c := range s.Checks {
		if w, ok := want[c.Mode+" "+c.Metric]; ok {
			if c.Reading != w {
				t.Errorf("%s %s: %q, want %q", c.Mode, c.Metric, c.Reading, w)
			}
			delete(want, c.Mode+" "+c.Metric)
		}
	}
	if len(want) > 0 {
		t.Errorf("checks missing %v in %+v", want, s.Checks)
	}
	if len(s.Checks) != 6 {
		t.Errorf("%d checks, want 4 for everysec and 2 for always", len(s.Checks))
	}
	if !strings.Contains(m.Headline, "`everysec` adds +0.05 ms at p99 (non-streaming total, against none; -0.45 ms in a second pass)") {
		t.Errorf("headline %q", m.Headline)
	}

	var out bytes.Buffer
	if err := Render(dir, tmplPath, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"**A second bench pass.**",
		"| Cost against `none`, p50 total (ms) | n/a / n/a | +0.05 / -0.45 | +2.00 / +1.40 |",
		"| `everysec` total p50 | +0.05 ms | -0.45 ms | 0.50 ms | 0.10 ms | unresolved: within the gateway drift |",
		"- `everysec` bench, second pass: commit `abc1234`",
		"- M9's `everysec` cost resolved at 0.1 ms",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("render lacks %q", want)
		}
	}
	if strings.Contains(out.String(), "OUT_ROOT=eval/results/m9-persist/bench-repeat make bench-persist") {
		t.Error("render still lists the second pass under To measure")
	}
}

// Redis counts delayed fsyncs only under everysec; under always the
// fsync is inline, so the row must not read as "0 delays".
func TestM9DelayedFsyncOnlyUnderEverysec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, M9Dir)
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "none"), "none", 1)
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "everysec"), "everysec", 1.05)
	m9WriteBench(t, filepath.Join(root, M9BenchDir, "always"), "always", 3)
	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"n/a / n/a", "3 / 3", "n/a / n/a"} {
		if got := d.M9.Runs[i].Bench.DelayedFsync; got != want {
			t.Errorf("%s delayed fsyncs = %q, want %q", d.M9.Runs[i].Mode, got, want)
		}
	}
	if d.M9.DriftMax != "0.01" {
		t.Errorf("largest drift = %q, want 0.01", d.M9.DriftMax)
	}
}

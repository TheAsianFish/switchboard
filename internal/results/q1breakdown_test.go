package results

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
)

// rampSamples is n made-up latencies in ms: base plus a ramp of width ms, with
// every 50th request slowed by tail ms, in a scrambled but fixed order.
func rampSamples(n int, base, width, tail float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		j := (i * 7919) % n
		v := base + width*float64(j)/float64(n)
		if j%50 == 49 {
			v += tail
		}
		out[i] = math.Round(v*1000) / 1000
	}
	return out
}

// withSamples is a synthetic breakdown run whose summary matches its
// samples.
func withSamples(stream bool, rate float64, xs []float64) bench.Result {
	r := synthetic(stream, rate, 0, 0, map[string]int{"ok": len(xs)})
	r.Config.DurationSec, r.Config.WarmupSec = 30, 2
	d := make([]time.Duration, len(xs))
	for i, x := range xs {
		d[i] = time.Duration(x * float64(time.Millisecond))
	}
	s := bench.Summarise(d)
	r.Latency, r.TTFT = s, s
	r.Samples = &bench.Samples{LatencyMS: xs, TTFTMS: xs}
	return r
}

func ptr[T any](v T) *T { return &v }

// writeBreakdown writes a small breakdown: none, limits, exact and
// tracing at 200 req/s, semantic at 5 req/s, budget not selected and
// router skipped.
func writeBreakdown(t *testing.T, q1 string) {
	t.Helper()
	bdir := filepath.Join(q1, Q1BreakdownDir)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := func(name, status, reason string, rate float64, model string, fs ...string) ConfigMeta {
		return ConfigMeta{Name: name, Features: fs, Rate: rate, Model: model, Status: status, Reason: reason}
	}
	writeJSON(t, filepath.Join(q1, "meta.json"), Meta{
		Commit: "abc1234", Date: "2026-11-10T12:00:00Z",
		FakeProvider: FakeSettings{Latency: "20ms", Tokens: 30, TokenInterval: "5ms"},
		Command:      "RATES='500' FEATURES='none limits exact tracing semantic router' eval/bench/q1.sh",
		Breakdown: &BreakdownMeta{
			Rate: 200, EmbedRate: 5, Duration: "30s", Warmup: "2s", CacheDirective: "semantic",
			Redis: "redis:8", Jaeger: "jaegertracing/jaeger:2.23.0",
			Ollama: &OllamaMeta{Source: "docker ollama/ollama:0.40.11", Version: "0.40.11", EmbedModel: "nomic-embed-text",
				CapacityRPS: 21.5, CapacityRequests: 120, CapacityConcurrency: 16},
			Configs: []ConfigMeta{
				cfg("none", "measured", "", 200, "fake"),
				cfg("limits", "measured", "", 200, "fake", "limits"),
				cfg("budget", "not selected", "not in FEATURES", 200, "fake", "limits", "budget"),
				cfg("exact", "measured", "", 200, "fake", "limits", "budget", "exact"),
				cfg("tracing", "measured", "", 200, "fake", "limits", "budget", "exact", "tracing"),
				cfg("semantic", "measured", "", 5, "fake", "limits", "budget", "exact", "tracing", "semantic"),
				cfg("router", "skipped", "Ollama not available: ollama pull nomic-embed-text failed", 5, "auto", "limits", "budget", "exact", "tracing", "semantic", "router"),
			},
		},
	})
	run := func(name string, stream bool, rate float64, xs []float64) {
		writeJSON(t, filepath.Join(bdir, name+".json"), withSamples(stream, rate, xs))
	}
	gateway := func(name string, stream bool, rate float64, xs []float64, gw GatewayRun, metrics string) {
		run(name, stream, rate, xs)
		writeJSON(t, filepath.Join(bdir, name+".gateway.json"), gw)
		if err := os.WriteFile(filepath.Join(bdir, name+".metrics.txt"), []byte(metrics), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, stream := range []bool{false, true} {
		mode := modeName(stream)
		base := 21.0
		if stream {
			base = 21.3
		}
		run("direct-"+mode+"-200-1", stream, 200, rampSamples(1200, base, 1, 2))
		run("direct-"+mode+"-200-2", stream, 200, rampSamples(1200, base+0.05, 1, 2.5))
		run("direct-"+mode+"-5-1", stream, 5, rampSamples(150, base, 1, 2))
		run("direct-"+mode+"-5-2", stream, 5, rampSamples(150, base+0.1, 1, 2))
		ok := `switchboard_requests_total{key="bench",model="fake",outcome="ok"} 1601` + "\n"
		gateway("none-"+mode+"-200", stream, 200, rampSamples(1200, base+0.4, 1, 2),
			GatewayRun{Config: "none", WindowSeconds: 30, CPUSeconds: 2.4, PeakRSSKB: 32768, Probe: Probe{HTTPStatus: 200, Model: "fake"}}, ok)
		gateway("limits-"+mode+"-200", stream, 200, rampSamples(1200, base+0.7, 1, 2),
			GatewayRun{Config: "limits", WindowSeconds: 30, CPUSeconds: 3.6, PeakRSSKB: 34000, RedisCPUSeconds: ptr(1.2),
				Probe: Probe{HTTPStatus: 200, Model: "fake"}},
			ok+`switchboard_unpriced_requests_total{key="bench",model="fake"} 1601`+"\n")
		gateway("tracing-"+mode+"-200", stream, 200, rampSamples(1200, base+1.03, 1, 3),
			GatewayRun{Config: "tracing", WindowSeconds: 30, CPUSeconds: 4.8, PeakRSSKB: 36000, RedisCPUSeconds: ptr(2.1),
				Probe: Probe{HTTPStatus: 200, CacheStatus: "miss", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", TraceInJaeger: ptr(true), Model: "fake"}},
			ok+`switchboard_cache_lookups_total{result="miss"} 1601`+"\n")
		gateway("exact-"+mode+"-200", stream, 200, rampSamples(1200, base+1.0, 1, 3),
			GatewayRun{Config: "exact", WindowSeconds: 30, CPUSeconds: 4.5, PeakRSSKB: 35000, RedisCPUSeconds: ptr(2.1),
				Probe: Probe{HTTPStatus: 200, CacheStatus: "miss", Model: "fake"}},
			ok+`switchboard_cache_lookups_total{result="miss"} 1601`+"\n"+
				`switchboard_cache_lookup_seconds_sum{layer="exact"} 0.2`+"\n"+
				`switchboard_cache_lookup_seconds_count{layer="exact"} 1601`+"\n")
		gateway("semantic-"+mode+"-5", stream, 5, rampSamples(150, base+40, 10, 20),
			GatewayRun{Config: "semantic", WindowSeconds: 30, CPUSeconds: 0.6, PeakRSSKB: 33000, RedisCPUSeconds: ptr(0.3), OllamaCPUSeconds: ptr(12.0),
				Probe: Probe{HTTPStatus: 200, CacheStatus: "miss", Model: "fake"}},
			`switchboard_requests_total{key="bench",model="fake",outcome="ok"} 161`+"\n"+
				`switchboard_cache_lookups_total{result="miss"} 161`+"\n"+
				`switchboard_cache_errors_total{op="embed"} 2`+"\n"+
				`switchboard_cache_lookup_seconds_sum{layer="exact"} 0.02`+"\n"+
				`switchboard_cache_lookup_seconds_count{layer="exact"} 161`+"\n"+
				`switchboard_cache_lookup_seconds_sum{layer="semantic"} 6.3`+"\n"+
				`switchboard_cache_lookup_seconds_count{layer="semantic"} 150`+"\n")
	}
}

func TestRenderQ1Breakdown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q1 := filepath.Join(dir, Q1Dir)
	if err := os.Mkdir(q1, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBreakdown(t, q1)
	// A passthrough pair with the gateway's process figures.
	writeJSON(t, filepath.Join(q1, "direct-nonstream-500.json"), synthetic(false, 500, 21.05, 22.16, map[string]int{"ok": 30000}))
	writeJSON(t, filepath.Join(q1, "gateway-nonstream-500.json"), synthetic(false, 500, 21.42, 22.52, map[string]int{"ok": 30000}))
	writeJSON(t, filepath.Join(q1, "gateway-nonstream-500.gateway.json"), GatewayRun{Config: "none", WindowSeconds: 60.01, CPUSeconds: 30.5, PeakRSSKB: 51200})

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
		"| non-streaming | 500 | total | 21.05 | 22.16 | 21.42 | 22.52 | +0.37 | +0.36 | 30000 / 30000 | 0 / 0 | 0.51 | 50.0 |",
		"| exact | limits, budget, exact | 200 | `fake` |",
		"| + semantic (across: tracing at 200 req/s) | 5 |",
		"| + tracing | 200 |",
		"Ollama CPU per request (container CPU over the window divided by rate times window): `semantic` nonstream 80 ms, `semantic` stream 80 ms.",
		"| exact | non-streaming | 200 | 0.15 | 34.2 | 0.07 | n/a | 1200 | 0 | ok 1601 | miss 1601 | exact 0.12 ms (1601) | - | `miss` | `fake` | - |",
		"miss 161; errors: embed 2 | exact 0.12 ms (161), semantic 42.00 ms (150) |",
		"Cache lookups other than clean misses: `semantic` nonstream: embed errors 2, semantic layer ran for 150 of 161 lookups; `semantic` stream:",
		"- Q1 breakdown config `router` (skipped: Ollama not available: ollama pull nomic-embed-text failed): `FEATURES='router' eval/bench/q1.sh` with Docker or `OLLAMA_URL` reachable, then `make results`.",
		"- Q1 breakdown config `budget` (not selected: not in FEATURES)",
		"| tracing | non-streaming | 200 | 0.16 | 35.2 | 0.07 | n/a | 1200 | 0 | ok 1601 | miss 1601 | - | - | `miss` | `fake` | in Jaeger |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	if strings.Contains(out, "- Q1 per-feature breakdown") {
		t.Error("render lists the whole breakdown under To measure")
	}
	golden(t, "q1-breakdown.golden", first.Bytes())
}

func TestBreakdownRejectsBadInputs(t *testing.T) {
	t.Parallel()
	tests := map[string]func(t *testing.T, bdir string){
		"bad name": func(t *testing.T, bdir string) {
			writeJSON(t, filepath.Join(bdir, "proxy-nonstream-200.json"), withSamples(false, 200, []float64{1}))
		},
		"direct without sequence": func(t *testing.T, bdir string) {
			writeJSON(t, filepath.Join(bdir, "direct-nonstream-200.json"), withSamples(false, 200, []float64{1}))
		},
		"name disagrees with config": func(t *testing.T, bdir string) {
			writeJSON(t, filepath.Join(bdir, "none-stream-200.json"), withSamples(false, 200, []float64{1}))
		},
		"no samples": func(t *testing.T, bdir string) {
			r := withSamples(false, 200, []float64{1})
			r.Samples = nil
			writeJSON(t, filepath.Join(bdir, "none-nonstream-200.json"), r)
		},
		"bad metrics": func(t *testing.T, bdir string) {
			writeJSON(t, filepath.Join(bdir, "none-nonstream-200.json"), withSamples(false, 200, []float64{1}))
			if err := os.WriteFile(filepath.Join(bdir, "none-nonstream-200.metrics.txt"), []byte("switchboard_x one\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q1 := filepath.Join(dir, Q1Dir)
			bdir := filepath.Join(q1, Q1BreakdownDir)
			if err := os.MkdirAll(bdir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, filepath.Join(q1, "meta.json"), Meta{Breakdown: &BreakdownMeta{Rate: 200, EmbedRate: 5}})
			setup(t, bdir)
			var buf bytes.Buffer
			if err := Render(dir, tmplPath, &buf); err == nil {
				t.Error("Render accepted bad inputs")
			}
		})
	}
	t.Run("runs without breakdown meta", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		bdir := filepath.Join(dir, Q1BreakdownDir)
		if err := os.MkdirAll(bdir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(bdir, "direct-nonstream-200-1.json"), withSamples(false, 200, []float64{1}))
		if _, err := loadBreakdown(bdir, nil); err == nil {
			t.Error("loadBreakdown accepted runs without meta")
		}
	})
}

func TestBootPercentiles(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 99, 100, 1001} {
		xs := rampSamples(n, 20, 5, 10)
		b := newBoot("x", xs)
		d := make([]time.Duration, n)
		for i, x := range xs {
			d[i] = time.Duration(math.Round(x * float64(time.Millisecond)))
		}
		s := bench.Summarise(d)
		if b.p50 != s.P50 || b.p99 != s.P99 {
			t.Errorf("n=%d: boot p50 %v p99 %v, bench %v %v", n, b.p50, b.p99, s.P50, s.P99)
		}
		if len(b.b50) != q1Resamples || len(b.b99) != q1Resamples {
			t.Fatalf("n=%d: %d and %d replicates", n, len(b.b50), len(b.b99))
		}
		sorted := slices.Sorted(slices.Values(xs))
		for i := range q1Resamples {
			// A replicate is always one of the observed values.
			if _, found := slices.BinarySearch(sorted, b.b50[i]); !found {
				t.Fatalf("n=%d: replicate %v is not a sample", n, b.b50[i])
			}
		}
		again := newBoot("x", xs)
		if !slices.Equal(again.b99, b.b99) {
			t.Errorf("n=%d: replicates are not reproducible", n)
		}
	}
}

func TestBootIntervalCoversShift(t *testing.T) {
	t.Parallel()
	// The same distribution shifted by 0.5 ms: the interval on the p50
	// difference holds 0.5, and the same set against itself holds 0.
	a, b := rampSamples(2000, 20, 2, 5), rampSamples(2000, 20.5, 2, 5)
	p50, _ := sub(newBoot("b", b), newBoot("a", a)).ests()
	if p50.lo > 0.5 || p50.hi < 0.5 || math.Abs(p50.v-0.5) > 1e-9 {
		t.Errorf("shift interval %v", p50)
	}
	self, _ := sub(newBoot("a1", a), newBoot("a2", a)).ests()
	if self.lo > 0 || self.hi < 0 {
		t.Errorf("self interval %v", self)
	}
	// A step between two configs against one baseline does not depend on it.
	base := newBoot("base", a)
	x, y := newBoot("x", b), newBoot("y", rampSamples(2000, 21, 2, 5))
	step := sub(y, base).minus(sub(x, base))
	direct := sub(y, x)
	for i := range q1Resamples {
		if math.Abs(step.r50[i]-direct.r50[i]) > 1e-9 {
			t.Fatalf("replicate %d: step %v, direct difference %v", i, step.r50[i], direct.r50[i])
		}
	}
	if (Est{}).String() != "n/a" {
		t.Errorf("empty Est = %q", Est{}.String())
	}
}

func TestLoadMetrics(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.txt")
	text := `# HELP x
switchboard_requests_total{key="bench",model="qwen2.5:1.5b",outcome="ok"} 26
switchboard_requests_total{key="bench",model="qwen2.5:3b",outcome="ok"} 8
switchboard_router_decisions_total{model="qwen2.5:1.5b",reason="target"} 26
switchboard_router_decisions_total{model="qwen2.5:3b",reason="fallback"} 8
switchboard_store_degraded{component="a,b"} 0
switchboard_up 1
`
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadMetrics(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(m.by("switchboard_requests_total", "outcome")); got != "ok 34" {
		t.Errorf("requests by outcome = %q", got)
	}
	if got := counts(m.by("switchboard_router_decisions_total", "model", "reason")); got != "qwen2.5:1.5b target 26, qwen2.5:3b fallback 8" {
		t.Errorf("router decisions = %q", got)
	}
	if got := m.by("switchboard_store_degraded", "component"); got["a,b"] != 0 || len(got) != 1 {
		t.Errorf("quoted comma: %v", got)
	}
	if m["switchboard_up"][""] != 1 {
		t.Errorf("unlabelled series: %v", m["switchboard_up"])
	}
	if none, err := loadMetrics(filepath.Join(t.TempDir(), "missing")); none != nil || err != nil {
		t.Errorf("missing file = %v, %v", none, err)
	}
}

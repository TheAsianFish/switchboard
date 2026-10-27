package results

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cacheeval"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

const tmplPath = "../../eval/RESULTS.md.tmpl"

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// synthetic is a made-up bench result for renderer tests only.
func synthetic(stream bool, rate float64, p50, p99 float64, outcomes map[string]int) bench.Result {
	n := 0
	for _, v := range outcomes {
		n += v
	}
	lat := bench.Stats{Count: outcomes["ok"], Mean: p50, P50: p50, P90: p99, P99: p99, P999: p99, Max: p99}
	r := bench.Result{
		Config:    bench.ConfigEcho{URL: "http://127.0.0.1:9101", Format: "openai", Model: "fake", Stream: stream, Rate: rate, DurationSec: 60, WarmupSec: 5, TimeoutSec: 30, MaxInFlight: 1024},
		Requests:  n,
		Outcomes:  outcomes,
		Latency:   lat,
		TTFT:      lat,
		SendDelay: bench.Stats{Count: n, P99: 0.125},
		Hardware:  bench.Hardware{NumCPU: 8, GOMAXPROCS: 8, GOOS: "linux", GOARCH: "amd64", GoVersion: "go1.27.2", CPUModel: "Test CPU @ 3.00GHz", Kernel: "6.17.0"},
	}
	if stream {
		r.Latency.P50 += 150
		r.Latency.P99 += 150
	}
	return r
}

func TestRenderQ1(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q1 := filepath.Join(dir, Q1Dir)
	if err := os.Mkdir(q1, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(q1, "meta.json"), Meta{
		Commit: "abc1234-dirty", Date: "2026-10-26T12:00:00Z",
		FakeProvider: FakeSettings{Latency: "20ms", Tokens: 30, TokenInterval: "5ms"},
		Command:      "RATES='200 50' DURATION=60s WARMUP=5s eval/bench/q1.sh",
	})
	writeJSON(t, filepath.Join(q1, "direct-nonstream-200.json"), synthetic(false, 200, 20.5, 22.75, map[string]int{"ok": 12000}))
	writeJSON(t, filepath.Join(q1, "gateway-nonstream-200.json"), synthetic(false, 200, 21.0, 22.5, map[string]int{"ok": 11998, "http_502": 2}))
	writeJSON(t, filepath.Join(q1, "direct-nonstream-50.json"), synthetic(false, 50, 20.25, 21.004, map[string]int{"ok": 3000}))
	writeJSON(t, filepath.Join(q1, "gateway-nonstream-50.json"), synthetic(false, 50, 20.75, 21.001, map[string]int{"ok": 3000}))
	writeJSON(t, filepath.Join(q1, "direct-stream-50.json"), synthetic(true, 50, 20.1, 20.9, map[string]int{"ok": 2999, "dropped": 1}))
	writeJSON(t, filepath.Join(q1, "direct-stream-200.json"), synthetic(true, 200, 20.2, 21.5, map[string]int{"ok": 12000}))
	failed := synthetic(true, 200, 0, 0, map[string]int{"http_401": 12000})
	failed.Latency, failed.TTFT = bench.Stats{}, bench.Stats{}
	writeJSON(t, filepath.Join(q1, "gateway-stream-200.json"), failed)

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
	golden(t, "q1.golden", first.Bytes())
}

func TestRenderNotMeasured(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing", "empty"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "results")
			if name == "empty" {
				if err := os.MkdirAll(filepath.Join(dir, Q1Dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var buf bytes.Buffer
			if err := Render(dir, tmplPath, &buf); err != nil {
				t.Fatal(err)
			}
			golden(t, "not-measured.golden", buf.Bytes())
		})
	}
}

func TestRenderRejectsBadInputs(t *testing.T) {
	t.Parallel()
	tests := map[string]func(t *testing.T, q1 string){
		"no meta": func(t *testing.T, q1 string) {
			writeJSON(t, filepath.Join(q1, "direct-nonstream-50.json"), synthetic(false, 50, 1, 2, map[string]int{"ok": 1}))
		},
		"bad name": func(t *testing.T, q1 string) {
			writeJSON(t, filepath.Join(q1, "meta.json"), Meta{})
			writeJSON(t, filepath.Join(q1, "proxy-nonstream-50.json"), synthetic(false, 50, 1, 2, map[string]int{"ok": 1}))
		},
		"name disagrees with config": func(t *testing.T, q1 string) {
			writeJSON(t, filepath.Join(q1, "meta.json"), Meta{})
			writeJSON(t, filepath.Join(q1, "direct-stream-50.json"), synthetic(false, 50, 1, 2, map[string]int{"ok": 1}))
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q1 := filepath.Join(dir, Q1Dir)
			if err := os.Mkdir(q1, 0o755); err != nil {
				t.Fatal(err)
			}
			setup(t, q1)
			var buf bytes.Buffer
			if err := Render(dir, tmplPath, &buf); err == nil {
				t.Error("Render accepted bad inputs")
			}
			if buf.Len() != 0 {
				t.Error("Render wrote output despite failing")
			}
		})
	}
}

func TestMainWritesFile(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "RESULTS.md")
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--results", t.TempDir(), "--template", tmplPath, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("Main = %d: %s", code, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Not measured yet. Run `make bench`") {
		t.Errorf("unexpected output:\n%s", b)
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (go test -update to accept):\n%s", path, got)
	}
}

func TestLoadSDK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := map[string]string{
		"openai--m.json":    `{"sdk":"openai-python","sdk_version":"3.0","model":"m","backend":"b","passed":true,"recorded_at":"2026-10-26T10:00:00Z","stream":{"text":"a|b","chunks":3,"stop_reason":"stop","ttft_ms":81.4,"usage":{"completion_tokens":4}}}`,
		"anthropic--m.json": `{"sdk":"anthropic-python","sdk_version":"2.0","model":"m","backend":"b","passed":false,"error":"TypeError: x","recorded_at":"2026-10-27T10:00:00Z"}`,
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := loadSDK(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || strings.Join(got.Dates, ",") != "2026-10-26,2026-10-27" {
		t.Fatalf("got %+v", got)
	}
	a, o := got.Rows[0], got.Rows[1]
	if a.Passed != "no" || a.Error != "TypeError: x" || a.SDK != "anthropic-python 2.0" {
		t.Errorf("failed case = %+v", a)
	}
	if o.Passed != "yes" || o.Chunks != 3 || o.OutputTokens != 4 || o.TTFT != "81" || o.Text != `a\|b` {
		t.Errorf("passed case = %+v", o)
	}
	if none, err := loadSDK(filepath.Join(dir, "missing")); none != nil || err != nil {
		t.Errorf("missing dir = %v, %v", none, err)
	}
}

func TestRenderQ2(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q2 := filepath.Join(dir, Q2Dir)
	if err := os.Mkdir(q2, 0o755); err != nil {
		t.Fatal(err)
	}
	// A copy of a real run's raw files.
	for _, name := range []string{"fixed-window.json", "fixed-window-redis-outage.json", "no-shared-store.json", "budget.json"} {
		b, err := os.ReadFile(filepath.Join("testdata", "q2", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(q2, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := Render(dir, tmplPath, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"| fixed-window | redis | fixed_window | 900 | 366 | 534 | 0 | 100 (100) | 20 | 100 | 53 | 47 | 167 | 120 | 47 | 22.4 / 24.2 |",
		"| Redis down | 60.003 | 120.329 | 178 | 120 | 58 |",
		"- `fixed-window`: 100* · 100 · 100 · 66*",
		"total budget of $0.10",
		"| $0.099045000 | $0.099045000 | $0.000000000 |",
		"Not in this run: `gcra`, `gcra-redis-outage`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	golden(t, "q2.golden", buf.Bytes())
}

// Result files written before the per-window counts and latency were
// recorded still render, with those cells marked as missing.
func TestRenderQ2WithoutLimiterWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q2 := filepath.Join(dir, Q2Dir)
	if err := os.Mkdir(q2, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("testdata", "q2", "fixed-window.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"limiter_windows", "most_in_limiter_window", "latency_ok", "admitted_at_seconds"} {
		delete(r, k)
	}
	if b, err = json.Marshal(r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q2, "fixed-window.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Render(dir, tmplPath, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| 0 | — (100) | 20 |", "| — / — |", "- `fixed-window`: not recorded"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("render lacks %q", want)
		}
	}
}

// q3Sims is a made-up similarity file for renderer tests only: positives
// at 0.95 and 0.85, negatives at neg (numbers equal) and two at 0.99 whose
// numbers differ.
func q3Sims(model, set string, dims int, neg float64) cacheeval.Sims {
	var ps []cacheeval.SimPair
	for i := range 20 {
		cos := 0.95
		if i%2 == 1 {
			cos = 0.85
		}
		ps = append(ps, cacheeval.SimPair{ID: fmt.Sprint("p", i), Same: true, Cos: cos, NumbersEqual: true, Exact: i == 0, Norm: i < 2})
	}
	for i := range 400 {
		ps = append(ps, cacheeval.SimPair{ID: fmt.Sprint("n", i), Cos: neg, NumbersEqual: true})
	}
	ps = append(ps, cacheeval.SimPair{ID: "x1", Cos: 0.99}, cacheeval.SimPair{ID: "x2", Cos: 0.99})
	return cacheeval.Sims{Model: model, Set: set, Dims: dims, Pairs: ps}
}

func TestRenderQ3(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q3 := filepath.Join(dir, Q3Dir)
	if err := os.Mkdir(q3, 0o755); err != nil {
		t.Fatal(err)
	}
	hw := bench.Hardware{NumCPU: 8, GOMAXPROCS: 8, GOOS: "linux", GOARCH: "amd64", GoVersion: "go1.27.2", CPUModel: "Test CPU @ 3.00GHz", Kernel: "6.17.0"}
	info := cacheeval.RunInfo{Commit: "abc1234", Date: "2026-10-27T12:00:00Z", Command: "switchboard eval cache", Hardware: hw}
	sims := []cacheeval.Sims{
		q3Sims("all-minilm", cacheeval.SetPAWS, 384, 0.9),
		q3Sims("all-minilm", cacheeval.SetGSM, 384, 0.8),
		q3Sims("nomic-embed-text", cacheeval.SetPAWS, 768, 0.8),
		q3Sims("nomic-embed-text", cacheeval.SetGSM, 768, 0.8),
	}
	data := cacheeval.DataMeta{Model: "qwen2.5:3b", Seed: 1, GSMEligible: 1200, GSMSampled: 500, NumbersOK: 450, AnswerOK: 400, Positives: 400, Negatives: 500, PAWSRows: 8000}
	lat := []cacheeval.Latency{{Model: "all-minilm", Calls: 200, Warmups: 10, Dims: 384, P50Ms: 4.5, P99Ms: 9.25, MeanMs: 5}}
	s, err := cacheeval.Summarise(sims, []string{"all-minilm", "nomic-embed-text"}, []string{cacheeval.SetPAWS, cacheeval.SetGSM}, 0.01, lat, data, info)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(q3, cacheeval.SummaryFile), s)
	writeJSON(t, filepath.Join(q3, "index-384.json"), cacheeval.IndexResult{
		RunInfo: info, RedisVersion: "8.2.1", Entries: 100000, Dims: 384, Queries: 1000, Contexts: 100, Vectors: "random unit",
		Runs: []cacheeval.IndexRun{
			{Algorithm: "FLAT", BuildSeconds: 3.25, Unfiltered: cacheeval.IndexLatency{P50Ms: 12.5, P99Ms: 20}, Filtered: cacheeval.IndexLatency{P50Ms: 0.5, P99Ms: 1}, RecallAt1: 1, IndexSizeMB: 160.4},
			{Algorithm: "HNSW", Params: map[string]int{"M": 16, "EF_CONSTRUCTION": 200, "EF_RUNTIME": 10}, BuildSeconds: 40, Unfiltered: cacheeval.IndexLatency{P50Ms: 0.75, P99Ms: 2}, Filtered: cacheeval.IndexLatency{P50Ms: 1.5, P99Ms: 3}, RecallAt1: 0.987, FilteredRecallAt1: 1, IndexSizeMB: 210},
		},
	})

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
		"| `paws-wiki` | 20 | 402 |",
		// Unguarded: the 0.99 negatives keep false-hitting until 0.9901.
		"| `all-minilm` | `paws-wiki` | off | 0.9025 | 50.0% (29.9–70.1) | 0.5% (0.1–1.8) | 0.9901 | 0.0% (0.0–16.1) | 0.0% (0.0–0.9) |",
		// Guarded: they never hit.
		"| `all-minilm` | `paws-wiki` | on | 0.9025 | 50.0% (29.9–70.1) | 0.0% (0.0–0.9) | 0.9025 | 50.0% (29.9–70.1) | 0.0% (0.0–0.9) |",
		"| `paws-wiki` | 5.0% (0.9–23.6) | 0.0% (0.0–0.9) | 10.0% (2.8–30.1) | 0.0% (0.0–0.9) |",
		"| `nomic-embed-text` | on | 0.8025 | `paws-wiki` 100.0% (83.9–100.0); `gsm8k-perturb` 100.0% (83.9–100.0) | 100.0% |",
		"The gateway ships with `nomic-embed-text` at threshold 0.8025 with the number guard on.",
		"| `all-minilm` | 384 | 200 (after 10) | 4.50 | 9.25 | 5.00 |",
		"| HNSW | EF_CONSTRUCTION 200, EF_RUNTIME 10, M 16 | 100000 | 384 | 100 | 40.0 | 0.75 | 2.00 | 0.987 | 1.50 | 3.00 | 1.000 | 210.0 |",
		"500 GSM8K test questions sampled (seed 1)",
		"at least 381 negatives",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	if strings.Contains(out, "Q3 semantic cache threshold sweep: `make eval-cache`") {
		t.Error("Q3 still listed under To measure")
	}
	golden(t, "q3.golden", first.Bytes())
}

func TestLoadQ3Partial(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if q, err := loadQ3(filepath.Join(dir, "missing")); q != nil || err != nil {
		t.Errorf("missing dir = %v, %v", q, err)
	}
	writeJSON(t, filepath.Join(dir, "index.json"), cacheeval.IndexResult{Entries: 10, Runs: []cacheeval.IndexRun{{Algorithm: "FLAT"}}})
	q, err := loadQ3(dir)
	if err != nil || q == nil || q.Sweep != nil || q.Index == nil || len(q.Index.Rows) != 1 {
		t.Fatalf("index only = %+v, %v", q, err)
	}
	writeJSON(t, filepath.Join(dir, "index-768.json"), cacheeval.IndexResult{Entries: 10})
	if _, err := loadQ3(dir); err == nil {
		t.Error("index file without runs accepted")
	}
}

func TestWrongServed(t *testing.T) {
	t.Parallel()
	points := []cacheeval.SweepPoint{
		{HitRate: cacheeval.Rate{K: 197, N: 3536}, FalseHitRate: cacheeval.Rate{K: 28, N: 4464}},
		{HitRate: cacheeval.Rate{K: 0, N: 329}, FalseHitRate: cacheeval.Rate{K: 0, N: 500}},
	}
	got := wrongServed([]string{"paws-wiki", "gsm8k-perturb"}, points)
	want := "`paws-wiki` 12.4% (28 of 225); `gsm8k-perturb` — (0 of 0)"
	if got != want {
		t.Errorf("wrongServed = %q, want %q", got, want)
	}
}

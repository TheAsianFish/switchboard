package results

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/bench"
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

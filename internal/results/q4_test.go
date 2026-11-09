package results

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/bench"
)

// q4Arm is a made-up arm for renderer tests only: n samples rising
// linearly to tail.
func q4Arm(n int, tail float64) bench.Result {
	r := synthetic(true, 200, 50, tail, map[string]int{"ok": n})
	r.WarmupRequests = n / 10
	r.Samples = &bench.Samples{}
	for i := range n {
		v := 50 + (tail-50)*float64(i)/float64(n-1)
		r.Samples.TTFTMS = append(r.Samples.TTFTMS, v)
		r.Samples.LatencyMS = append(r.Samples.LatencyMS, v+150)
	}
	return r
}

func TestRenderQ4(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q4 := filepath.Join(dir, Q4Dir)
	if err := os.Mkdir(q4, 0o755); err != nil {
		t.Fatal(err)
	}
	var meta Q4Meta
	meta.Commit, meta.Date, meta.Command = "abc1234", "2026-11-09T12:00:00Z", "eval/hedge/run.sh"
	meta.FakeProvider.Latency, meta.FakeProvider.LatencySigma = "50ms", 0.3
	meta.FakeProvider.SlowFraction, meta.FakeProvider.SlowFactor = 0.05, 10
	meta.FakeProvider.Tokens, meta.FakeProvider.TokenInterval, meta.FakeProvider.Seed = 30, "5ms", 1
	meta.Rate, meta.Duration, meta.Warmup = 200, "60s", "5s"
	writeJSON(t, filepath.Join(q4, "meta.json"), meta)
	writeJSON(t, filepath.Join(q4, "off.json"), q4Arm(1000, 600))
	writeJSON(t, filepath.Join(q4, "on.json"), q4Arm(1000, 200))
	writeJSON(t, filepath.Join(q4, "off-ledger.json"), LedgerSummary{Requests: 1100, Completed: 1100, InputTokens: 5500, OutputTokens: 33000})
	writeJSON(t, filepath.Join(q4, "on-ledger.json"), LedgerSummary{Requests: 1166, Completed: 1100, InputTokens: 5830, OutputTokens: 33000})
	if err := os.WriteFile(filepath.Join(q4, "on-metrics.prom"), []byte(
		"switchboard_provider_hedges_total{event=\"sent\",provider=\"fake\"} 66\n"+
			"switchboard_provider_hedges_total{event=\"won\",provider=\"fake\"} 60\n"+
			"switchboard_provider_hedges_total{event=\"lost\",provider=\"fake\"} 6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(q4, "off-providers.json"), map[string]any{"providers": []any{map[string]any{"provider": "fake"}}})
	writeJSON(t, filepath.Join(q4, "on-providers.json"), map[string]any{"providers": []any{map[string]any{"provider": "fake", "hedge_delay_ms": map[string]float64{"fake/stream": 101.25}}}})

	var first, second bytes.Buffer
	if err := Render(dir, tmplPath, &first); err != nil {
		t.Fatal(err)
	}
	if err := Render(dir, tmplPath, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("two renders of the same inputs differ (the bootstrap must be seeded)")
	}
	out := first.String()
	for _, want := range []string{
		"| off | 1000 | 0 |",
		"| 1100 | 0.0% | 0 | 0 / 0 / 0 | n/a | 5500 / 33000 |",
		"| 1166 | 6.0% | 66 | 66 / 60 / 0 | 101.2 | 5830 / 33000 |",
		"for +6.0% upstream requests",
		"5% of requests are 10× slower",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	golden(t, "q4.golden", first.Bytes())
}

func TestSummarizeLedger(t *testing.T) {
	t.Parallel()
	s, err := SummarizeLedger(strings.NewReader(`{"entries":[
		{"input_tokens":5,"output_tokens":30,"completed":true},
		{"input_tokens":5,"output_tokens":0,"completed":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s != (LedgerSummary{Requests: 2, Completed: 1, InputTokens: 10, OutputTokens: 30}) {
		t.Fatalf("summary = %+v", s)
	}
	if _, err := SummarizeLedger(strings.NewReader("not json")); err == nil {
		t.Fatal("bad input accepted")
	}
}

func TestRenderQ4Ollama(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := filepath.Join(dir, Q4OllamaDir)
	if err := os.Mkdir(q, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(q, "meta.json"), map[string]any{
		"commit": "abc1234", "date": "2026-11-09T12:00:00Z", "model": "qwen2.5:0.5b", "ollama_version": "0.40.15",
		"rate": 1, "duration": "180s", "warmup": "30s", "max_tokens": 16, "command": "eval/hedge/ollama.sh",
	})
	writeJSON(t, filepath.Join(q, "off.json"), q4Arm(180, 400))
	writeJSON(t, filepath.Join(q, "on.json"), q4Arm(180, 450))
	if err := os.WriteFile(filepath.Join(q, "on-metrics.prom"), []byte("switchboard_provider_hedges_total{event=\"sent\",provider=\"ollama\"} 18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Render(dir, tmplPath, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"`qwen2.5:0.5b` on Ollama 0.40.15",
		"| 18 / 0 | 9.1% |",
		"changed the TTFT p99 by +50.0 ms (+12%)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
}

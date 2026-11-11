package results

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderQ6 renders testdata/q6-chaos/summary.json, a chaos run's
// summary with its buckets dropped and the replica restart made to never
// recover, so both outcomes of a check are rendered.
func TestRenderQ6(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, Q6Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("testdata", Q6Dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Q6Dir, "summary.json"), b, 0o644); err != nil {
		t.Fatal(err)
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
		"| main | `fake-chaos` | 200 | 24000 | streaming, 30.0% repeats of 50 prompts |",
		"| replica down | 30.0 | 60.0 | 2 of 3 | up | 6002 | 0.20% | malformed_stream 10, http_502 2 |",
		"| replica and Redis down | 450 | 353 | 98 | 13 | 6 | 3 |",
		"| kill switchboard-2 | 30.00 | 1.57 | 0.996 s (from 31 s) | 12 |",
		"| start switchboard-2 | 90.01 | 0.48 | not recovered |",
		"$0.0186 spent while Redis was down, bound $0.0200 | yes |",
		"steady after 0.996 s (from 31 s) | yes |",
		"0 errors in `all back`; not recovered | no |",
		"first byte after 20 ms, then 30 tokens 5 ms apart",
		"- Commit: `abc1234`, run 2026-11-09T12:00:00Z.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	if strings.Contains(out, "Q6 replica and Redis kills under load: `make chaos`") {
		t.Error("Q6 still listed under To measure")
	}
	golden(t, "q6.golden", first.Bytes())
}

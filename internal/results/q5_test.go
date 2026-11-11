package results

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// q5Fixture copies the made-up Q5 files in testdata/q5-router into a fresh
// results directory and returns it and the section's directory.
func q5Fixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	q5 := filepath.Join(dir, Q5Dir)
	if err := os.Mkdir(q5, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"summary.json", "meta.json"} {
		b, err := os.ReadFile(filepath.Join("testdata", "q5-router", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(q5, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, q5
}

func TestRenderQ5(t *testing.T) {
	t.Parallel()
	dir, _ := q5Fixture(t)
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
		"The eval set is 600 committed questions (300 GSM8K and 300 MMLU;",
		"(parameters: `qwen2.5:0.5b` 0.49 B; `qwen2.5:1.5b` 1.54 B; `qwen2.5:3b` 3.09 B)",
		"plus its embedding from nomic-embed-text (768 dimensions).",
		"95.7% of `qwen2.5:3b`'s accuracy at 82.2% of its cost; gain over the line +3.7 points (95% CI +0.8 to +6.2)",
		"per dataset: GSM8K 7% / 10% / 83%; MMLU 10% / 30% / 60%.",
		"| `qwen2.5:0.5b` | 32.0% (28.4–35.8) | 24.3% | 39.7% | 264,600 | 1.84 | 4.21 | 117.6 | 4 | $0.00 |",
		"| always the best (`qwen2.5:3b`) | 66.0% (62.1–69.7) | 1,730,400 | 100.0% | 100.0% |",
		"| oracle (cheapest correct candidate) | 76.2% (72.6–79.4) | 701,200 | 115.4% | 40.5% |",
		"| `router` at target 0.70 (operating point, held out) | 63.2% (59.2–66.9) | 1,421,700 | 95.7% | 82.2% |",
		"| knows the dataset: GSM8K to `qwen2.5:3b`, MMLU to `qwen2.5:1.5b` | 63.3% (59.4–67.1) |",
		"**Frontier of `router`** (embedding plus cheap features), held out.",
		"across the line's cost range: +5.4 points over the line (95% CI +3.4 to +7.4),\n+4.4 over the hull (+2.4 to +6.4)",
		"| 0.70 (operating point) | 63.2% (59.2–66.9) | 1,421,700 | 59.5% | +3.7 | 60.5% | +2.7 | 8% / 20% / 72% |",
		"| 0.30 | 39.5% (35.7–43.5) | 640,000 | 40.7% | -1.2 | 41.7% | -2.2 | 50% / 30% / 20% |",
		"| `router-cheap` | `qwen2.5:3b` | 0.612 | 0.219 | 0.3 |",
		"(Ollama 0.40.15 on the same machine)",
		"- Labels: 2026-11-09 to 2026-11-10, 7 h 26 min of summed request time.",
		"- Cost: $0.00 (local models only).",
		"- 5 replies were cut off by the output cap (`qwen2.5:0.5b` 4, `qwen2.5:1.5b` 1)",
		"![Held-out router frontier against the random-mix line](eval/results/q5-router/frontier.svg)",
		"(estimated $2.89: `claude-haiku-5-5` $0.05, `claude-sonnet-5-5` $0.95, `claude-opus-5-5` $1.90;",
		"estimated from `qwen2.5:3b`'s token counts at list prices; Claude's own\n  replies will differ).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	for _, unwanted := range []string{
		"Not measured yet. Run `make eval-router`",
		"- Q5 router frontier: `make eval-router`.",
		"No target reached",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("render has %q", unwanted)
		}
	}
	golden(t, "q5.golden", first.Bytes())
}

func TestRenderQ5NotMeasured(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Render(t.TempDir(), tmplPath, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"## Q5: evaluated router\n\nCan a router",
		"`eval/results/q5-router/`.\n\nNot measured yet. Run `make eval-router`, then `make results`.\n\n## Q6: chaos",
		"- Q5 router frontier: `make eval-router`.\n- Q5 with the Claude candidates:",
		"then the same `MODELS=... make eval-router`.\n- Q6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
}

func TestLoadQ5(t *testing.T) {
	t.Parallel()
	if q, err := loadQ5(filepath.Join(t.TempDir(), "missing")); q != nil || err != nil {
		t.Errorf("missing dir = %v, %v", q, err)
	}
	if q, err := loadQ5(t.TempDir()); q != nil || err != nil {
		t.Errorf("dir without summary.json = %v, %v", q, err)
	}

	// Without input token counts there is nothing to estimate from.
	_, q5 := q5Fixture(t)
	b, err := os.ReadFile(filepath.Join(q5, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q5, "summary.json"), bytes.ReplaceAll(b, []byte(`"mean_input_tokens"`), []byte(`"x"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	q, err := loadQ5(q5)
	if err != nil {
		t.Fatal(err)
	}
	if q.Estimate != nil {
		t.Errorf("estimate without input tokens = %+v", q.Estimate)
	}
	if q.Live || q.USD || q.CostUnit != "GFLOPs" {
		t.Errorf("unit = %q, live %v, usd %v", q.CostUnit, q.Live, q.USD)
	}
}

func TestLoadQ5RejectsBadInputs(t *testing.T) {
	t.Parallel()
	tests := map[string]func(t *testing.T, q5 string){
		"malformed summary": func(t *testing.T, q5 string) {
			write(t, filepath.Join(q5, "summary.json"), `{"candidates": [`)
		},
		"malformed meta": func(t *testing.T, q5 string) {
			write(t, filepath.Join(q5, "meta.json"), `{"commit": 1}`)
		},
		"no meta": func(t *testing.T, q5 string) {
			if err := os.Remove(filepath.Join(q5, "meta.json")); err != nil {
				t.Fatal(err)
			}
		},
		"no candidates": func(t *testing.T, q5 string) {
			write(t, filepath.Join(q5, "summary.json"), `{"cost_unit": "gflops", "cheapest": "a", "best": "a"}`)
		},
		"unknown best": func(t *testing.T, q5 string) {
			write(t, filepath.Join(q5, "summary.json"), `{"cost_unit": "gflops", "cheapest": "a", "best": "b", "candidates": [{"model": "a"}]}`)
		},
		"unknown unit": func(t *testing.T, q5 string) {
			write(t, filepath.Join(q5, "summary.json"), `{"cost_unit": "joules", "cheapest": "a", "best": "a", "candidates": [{"model": "a"}]}`)
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, q5 := q5Fixture(t)
			setup(t, q5)
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

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestQ5Formatting(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ got, want string }{
		{q5Cost(264600, false), "264,600"},
		{q5Cost(1730400.4, false), "1,730,400"},
		{q5Cost(42.123, false), "42.12"},
		{q5Cost(1.23456, true), "$1.2346"},
		{points(-0.04), "+0.0"},
		{points(-1.25), "-1.3"},
		{hoursMinutes(26745), "7 h 26 min"},
		{hoursMinutes(780), "13 min"},
		{q5Datasets(map[string]int{"mmlu": 3, "gsm8k": 2, "arc": 1}), "2 GSM8K, 3 MMLU and 1 arc"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// A dollar-priced run with a Claude candidate, an operating point that
// missed the rule and more dataset rules than the table lists.
func TestRenderQ5Variants(t *testing.T) {
	t.Parallel()
	dir, q5 := q5Fixture(t)
	b, err := os.ReadFile(filepath.Join(q5, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	s["cost_unit"] = "usd"
	s["operating_point"].(map[string]any)["met"] = false
	cands := s["candidates"].([]any)
	cands[2].(map[string]any)["model"] = "claude-haiku-5-5"
	s["best"] = "claude-haiku-5-5"
	rules := s["dataset_rules"].([]any)
	for i := range 7 {
		rules = append(rules, map[string]any{"gsm8k": "a", "mmlu": "b", "accuracy": 0.5 - 0.01*float64(i), "cost_per_1k": 1})
	}
	s["dataset_rules"] = rules
	writeJSON(t, filepath.Join(q5, "summary.json"), s)

	var buf bytes.Buffer
	if err := Render(dir, tmplPath, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Cost is the gateway's price table applied to each\nreply's usage, in dollars per 1,000 requests",
		"| $ per 1,000 requests |",
		"| `qwen2.5:0.5b` | 32.0% (28.4–35.8) | 24.3% | 39.7% | $264600.0000 |",
		"No target reached the rule's accuracy",
		"- No target reached 95% of `claude-haiku-5-5`'s accuracy",
		"Of the 11 such rules, the most and the least accurate are\nshown",
		"| knows the dataset: GSM8K to `qwen2.5:3b`, MMLU to `qwen2.5:1.5b` | 63.3% (59.4–67.1) |",
		"| knows the dataset: GSM8K to `qwen2.5:0.5b`, MMLU to `qwen2.5:3b` | 42.5% (38.6–46.5) |",
		"`eval/hedge/ollama.sh`).\n- Q6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	for _, unwanted := range []string{"local models only", "Q5 with the Claude candidates", "knows the dataset: GSM8K to `a`", "GSM8K to `qwen2.5:3b`, MMLU to `qwen2.5:0.5b`"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("render has %q", unwanted)
		}
	}
}

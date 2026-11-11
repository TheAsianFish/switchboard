package results

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/cacheeval"
)

const readmeTmplPath = "../../eval/README-results.md.tmpl"

// copyTestdata copies testdata/<from>/* into dir/<to>.
func copyTestdata(t *testing.T, dir, from, to string) {
	t.Helper()
	dst := filepath.Join(dir, to)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join("testdata", from))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("testdata", from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// writeAll writes every section the renderer tests know: the Q1
// breakdown, Q2's real run, the made-up Q3 and Q4 runs, and the Q5 and Q6
// test summaries.
func writeAll(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	q1 := filepath.Join(dir, Q1Dir)
	if err := os.Mkdir(q1, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBreakdown(t, q1)
	copyTestdata(t, dir, "q2", Q2Dir)
	writeQ3(t, dir)
	writeQ4(t, dir)
	copyTestdata(t, dir, "q5-router", Q5Dir)
	copyTestdata(t, dir, "q6-chaos", Q6Dir)
	return dir
}

func TestRenderReadme(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		setup  func(t *testing.T) string
		golden string
		want   []string
	}{
		{
			name: "every section", setup: writeAll, golden: "readme.golden",
			want: []string{
				// The stack is tracing (the last config that does not embed),
				// the embedding step semantic, across rates.
				"limits, budget, exact, tracing together +1.01 ms p50 at 200 req/s; a semantic-cache miss adds +43.44 ms",
				"at most 100 admitted per limiter window (rate 100), 47 over the rate-plus-burst bound in the worst 60 s; 58 over while Redis was down; 181 over with per-replica limits",
				"Ships `nomic-embed-text` at threshold 0.8025 (number guard on); hit / false-hit rate there: `paws-wiki` 100.0% / 0.0%",
				"for +6.0% upstream requests",
				"Replica kill: 12 errors (0.20%) in its phase; Redis down: 0 errors",
			},
		},
		{
			name: "nothing measured", setup: func(t *testing.T) string { return t.TempDir() }, golden: "readme-not-measured.golden",
			want: []string{"| [Q1: gateway overhead](RESULTS.md#q1-gateway-overhead) | Not measured yet | `make bench` |"},
		},
		{
			name: "passthrough without breakdown",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				q1 := filepath.Join(dir, Q1Dir)
				if err := os.Mkdir(q1, 0o755); err != nil {
					t.Fatal(err)
				}
				writeJSON(t, filepath.Join(q1, "meta.json"), Meta{Commit: "abc1234"})
				writeJSON(t, filepath.Join(q1, "direct-nonstream-50.json"), synthetic(false, 50, 20.25, 21.004, map[string]int{"ok": 3000}))
				writeJSON(t, filepath.Join(q1, "gateway-nonstream-50.json"), synthetic(false, 50, 20.75, 21.001, map[string]int{"ok": 3000}))
				return dir
			},
			want: []string{"| Passthrough +0.50 ms p50 at 50 req/s (non-streaming); per-feature breakdown not measured yet | `make bench` |"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, err := Load(tt.setup(t))
			if err != nil {
				t.Fatal(err)
			}
			first, err := Execute(readmeTmplPath, d)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Execute(readmeTmplPath, d)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Error("two renders of the same inputs differ")
			}
			for _, w := range tt.want {
				if !strings.Contains(string(first), w) {
					t.Errorf("render lacks %q", w)
				}
			}
			// Every question has exactly one row.
			for _, q := range []string{"Q1", "Q2", "Q3", "Q4", "Q5", "Q6"} {
				if n := strings.Count(string(first), "| ["+q+":"); n != 1 {
					t.Errorf("%s has %d rows", q, n)
				}
			}
			if tt.golden != "" {
				golden(t, tt.golden, first)
			}
		})
	}
}

func TestSplice(t *testing.T) {
	t.Parallel()
	body := []byte("| a | b |\n")
	tests := []struct {
		name    string
		doc     string
		want    string
		wantErr string
	}{
		{
			name: "replaces content",
			doc:  "# T\n\n" + ReadmeStart + "\nold\n\nlines\n" + ReadmeEnd + "\n\nafter\n",
			want: "# T\n\n" + ReadmeStart + "\n\n| a | b |\n\n" + ReadmeEnd + "\n\nafter\n",
		},
		{
			name: "empty block",
			doc:  ReadmeStart + "\n" + ReadmeEnd + "\n",
			want: ReadmeStart + "\n\n| a | b |\n\n" + ReadmeEnd + "\n",
		},
		{
			name: "markers indented, end at end of file without newline",
			doc:  "x\n  " + ReadmeStart + "  \nold\n" + ReadmeEnd,
			want: "x\n  " + ReadmeStart + "  \n\n| a | b |\n\n" + ReadmeEnd,
		},
		{
			name: "marker quoted in prose does not count",
			doc:  "Use `" + ReadmeStart + "` to mark it.\n" + ReadmeStart + "\n" + ReadmeEnd + "\n",
			want: "Use `" + ReadmeStart + "` to mark it.\n" + ReadmeStart + "\n\n| a | b |\n\n" + ReadmeEnd + "\n",
		},
		{name: "missing start", doc: "x\n" + ReadmeEnd + "\n", wantErr: "no \"" + ReadmeStart + "\" line"},
		{name: "missing end", doc: ReadmeStart + "\nx\n", wantErr: "no \"" + ReadmeEnd + "\" line"},
		{name: "duplicate start", doc: ReadmeStart + "\n" + ReadmeStart + "\n" + ReadmeEnd + "\n", wantErr: "more than once (lines 1 and 2)"},
		{name: "duplicate end", doc: ReadmeStart + "\n" + ReadmeEnd + "\n" + ReadmeEnd + "\n", wantErr: "more than once (lines 2 and 3)"},
		{name: "reversed", doc: ReadmeEnd + "\nx\n" + ReadmeStart + "\n", wantErr: "comes before"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Splice([]byte(tt.doc), body)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Splice error = %v, want one containing %q", err, tt.wantErr)
				}
				if got != nil {
					t.Error("Splice returned output despite failing")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("Splice =\n%q\nwant\n%q", got, tt.want)
			}
			// Splicing again with the same body changes nothing.
			again, err := Splice(got, body)
			if err != nil || !bytes.Equal(again, got) {
				t.Errorf("second splice = %q, %v; want unchanged", again, err)
			}
		})
	}
}

func TestMainReadme(t *testing.T) {
	t.Parallel()
	const before = "# Title\n\n" + ReadmeStart + "\nstale\n" + ReadmeEnd + "\n\nTail.\n"
	tests := []struct {
		name string
		// readme is the README's content, "" for --readme "".
		readme     string
		code       int
		wantOut    bool
		wantReadme string // a line the README must contain afterwards
	}{
		{name: "writes both", readme: before, code: 0, wantOut: true, wantReadme: "| [Q6: chaos](RESULTS.md#q6-chaos) | Not measured yet | `make chaos` |"},
		{name: "readme disabled", readme: "", code: 0, wantOut: true},
		{name: "bad markers write nothing", readme: "# Title\n" + ReadmeEnd + "\n" + ReadmeStart + "\n", code: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			out := filepath.Join(dir, "RESULTS.md")
			readme := filepath.Join(dir, "README.md")
			args := []string{"--results", filepath.Join(dir, "results"), "--template", tmplPath, "--out", out,
				"--readme-template", readmeTmplPath, "--readme", ""}
			if tt.readme != "" {
				if err := os.WriteFile(readme, []byte(tt.readme), 0o644); err != nil {
					t.Fatal(err)
				}
				args[len(args)-1] = readme
			}
			var stdout, stderr bytes.Buffer
			if code := Main(args, &stdout, &stderr); code != tt.code {
				t.Fatalf("Main = %d, want %d: %s", code, tt.code, stderr.String())
			}
			if _, err := os.Stat(out); (err == nil) != tt.wantOut {
				t.Errorf("RESULTS.md written = %v, want %v", err == nil, tt.wantOut)
			}
			got, err := os.ReadFile(readme)
			switch {
			case tt.readme == "":
				if err == nil {
					t.Error("README written with --readme \"\"")
				}
			case err != nil:
				t.Fatal(err)
			case tt.wantReadme == "":
				if string(got) != tt.readme {
					t.Errorf("README changed despite failing:\n%s", got)
				}
			default:
				s := string(got)
				if !strings.Contains(s, tt.wantReadme) || strings.Contains(s, "stale") ||
					!strings.HasPrefix(s, "# Title\n\n"+ReadmeStart+"\n") || !strings.HasSuffix(s, ReadmeEnd+"\n\nTail.\n") {
					t.Errorf("README =\n%s", s)
				}
			}
		})
	}
}

func TestBreakdownHeadline(t *testing.T) {
	t.Parallel()
	q1 := filepath.Join(t.TempDir(), Q1Dir)
	if err := os.Mkdir(q1, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBreakdown(t, q1)
	q, err := loadQ1(q1)
	if err != nil {
		t.Fatal(err)
	}
	bk := q.Breakdown
	if s := bk.Stack(); s == nil || s.Config != "tracing" {
		t.Errorf("Stack = %+v, want tracing", s)
	}
	if s := bk.EmbedStep(); s == nil || s.Stage != "+ semantic" || s.Rate != "5" {
		t.Errorf("EmbedStep = %+v, want + semantic at 5", s)
	}
	if r := bk.Row("none"); r == nil || !strings.HasPrefix(r.TotalP50.String(), r.TotalP50.Point()+" [") {
		t.Errorf("Row(none) = %+v", r)
	}
	if r := bk.Row("router"); r != nil {
		t.Errorf("Row(router) = %+v, want nil (skipped)", r)
	}
	if got := (Est{}).Point(); got != "n/a" {
		t.Errorf("zero Est Point = %q", got)
	}
	empty := &Q1Breakdown{Rows: []BreakdownRow{{Config: "semantic", embeds: true}}}
	if empty.Stack() != nil || empty.EmbedStep() != nil {
		t.Error("Stack or EmbedStep found a row that is not there")
	}
}

func TestEmbeds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		features []string
		want     bool
	}{
		{"none", nil, false},
		{"tracing", []string{"limits", "tracing"}, false},
		{"semantic", nil, true},
		{"later", []string{"limits", "semantic"}, true},
		{"router", []string{"router"}, true},
	}
	for _, tt := range tests {
		if got := embeds(tt.name, tt.features); got != tt.want {
			t.Errorf("embeds(%q, %v) = %v, want %v", tt.name, tt.features, got, tt.want)
		}
	}
}

func TestHitFalse(t *testing.T) {
	t.Parallel()
	points := []cacheeval.SweepPoint{
		{HitRate: cacheeval.Rate{K: 197, N: 3536, P: 197.0 / 3536}, FalseHitRate: cacheeval.Rate{K: 28, N: 4464, P: 28.0 / 4464}},
		{HitRate: cacheeval.Rate{K: 0, N: 329}, FalseHitRate: cacheeval.Rate{}},
	}
	got := hitFalse([]string{"paws-wiki"}, points)
	want := "`paws-wiki` 5.6% / 0.6%; `1` 0.0% / n/a"
	if got != want {
		t.Errorf("hitFalse = %q, want %q", got, want)
	}
}

func TestGainVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ci   [2]float64
		want string
	}{
		{[2]float64{0.01, 0.03}, "above the line"},
		{[2]float64{-0.03, -0.01}, "below the line"},
		{[2]float64{-0.018, 0.01}, "not clearly above the line"},
		{[2]float64{0, 0.01}, "not clearly above the line"},
	}
	for _, tt := range tests {
		if got := gainVerdict(tt.ci); got != tt.want {
			t.Errorf("gainVerdict(%v) = %q, want %q", tt.ci, got, tt.want)
		}
	}
}

func TestQ2Lookups(t *testing.T) {
	t.Parallel()
	q, err := loadQ2(filepath.Join("testdata", "q2"))
	if err != nil {
		t.Fatal(err)
	}
	if r := q.Scenario("fixed-window"); r == nil || r.SixtyOver != 47 {
		t.Errorf("Scenario(fixed-window) = %+v", r)
	}
	if r := q.Scenario("gcra"); r != nil {
		t.Errorf("Scenario(gcra) = %+v, want nil", r)
	}
	if p := q.Phase("fixed-window-redis-outage", "Redis down"); p == nil || p.Over != 58 {
		t.Errorf("Phase(Redis down) = %+v", p)
	}
	if p := q.Phase("fixed-window", "Redis down"); p != nil {
		t.Errorf("Phase of a run without an outage = %+v, want nil", p)
	}
}

func TestQ6Phases(t *testing.T) {
	t.Parallel()
	q, err := loadQ6(filepath.Join("testdata", "q6-chaos"))
	if err != nil {
		t.Fatal(err)
	}
	if p := q.ReplicaDown; p == nil || p.Name != "replica down" || p.ErrorCount != "12" {
		t.Errorf("ReplicaDown = %+v", p)
	}
	if p := q.RedisDown; p == nil || p.Name != "replica and Redis down" || p.Redis != "down" {
		t.Errorf("RedisDown = %+v", p)
	}
	if q.RedisDown != &q.Phases[2] {
		t.Error("RedisDown does not point into Phases")
	}
}

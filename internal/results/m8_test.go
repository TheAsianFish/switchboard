package results

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/chaos"
)

// m8Fixture copies the M8 flap runs in testdata (the real summaries with
// their buckets dropped) and the Q6 test summary into a results
// directory; runs lists which flap runs to include.
func m8Fixture(t *testing.T, runs ...string) string {
	t.Helper()
	dir := t.TempDir()
	copyTestdata(t, dir, "q6-chaos", Q6Dir)
	for _, run := range runs {
		copyTestdata(t, dir, filepath.Join(M8Dir, run), filepath.Join(M8Dir, run))
	}
	return dir
}

func TestRenderM8(t *testing.T) {
	t.Parallel()
	d, err := Load(m8Fixture(t, M8Before, M8After, M8Q6Rerun))
	if err != nil {
		t.Fatal(err)
	}
	m := d.M8
	if m == nil {
		t.Fatal("no M8 section")
	}
	if len(m.Outages) != 8 || len(m.Phases) != 19 {
		t.Fatalf("%d outages, %d phases", len(m.Outages), len(m.Phases))
	}
	if o := m.Outages[4]; o.Replicas != "3, one killed" || o.BeforeRequests != "49" || o.AfterRequests != "0" || o.AfterExhaust != "–" {
		t.Errorf("outage 5 %+v", o)
	}
	for _, c := range m.Checks {
		if c.Held != "yes" {
			t.Errorf("check %q not held: %s", c.Hypothesis, c.Observed)
		}
	}
	if len(m.Checks) != 6 {
		t.Errorf("%d checks, want 6", len(m.Checks))
	}
	if m.OvershootBefore != "$0.2166" || m.OvershootAfter != "$0.0362" || m.RefillBound != "$0.0410" || m.LostBefore != "$0.004185" || m.LostAfter != "$0" {
		t.Errorf("headline %+v", m)
	}
	if m.RestartedAfter != "$0.0093" || m.ProcessesAfter != "4" || m.Refill != "1 h" || m.T != "92 s" {
		t.Errorf("prose numbers: restarted %s processes %s refill %s T %s", m.RestartedAfter, m.ProcessesAfter, m.Refill, m.T)
	}
	if m.RerunMeta == nil || len(m.Rerun) != 6 || m.Rerun[3].Before != m.Rerun[3].After {
		t.Errorf("rerun %+v", m.Rerun)
	}
	b, err := Execute(tmplPath, d)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{
		"| 6 | 110.0–118.0 | 3 | 58 | $0.0270 | $0.1604 | 7.3 | 20 | $0.0093 | $0.0362 | 4.6 |",
		"| Overshoot at the end | $0.2166 | $0.0362 |",
		"refills over 1 h",
		"overshoot $0.0362 against $0.0410 (4 processes, T = 92.0 s) | yes |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	if strings.Contains(out, "Run `make chaos-flap RUN=before`") {
		t.Error("M8 still listed as not measured")
	}
}

// With only the run before the fix, the after columns are dashes and only
// the checks that apply to it are made.
func TestM8BeforeOnly(t *testing.T) {
	t.Parallel()
	d, err := Load(m8Fixture(t, M8Before))
	if err != nil {
		t.Fatal(err)
	}
	m := d.M8
	if m == nil || m.OvershootAfter != "–" || m.Outages[0].AfterSpent != "–" || m.Refill != "–" || m.RerunMeta != nil {
		t.Fatalf("before-only %+v", m)
	}
	if len(m.Checks) != 3 {
		t.Errorf("%d checks, want 3 (two before, one kill)", len(m.Checks))
	}
	if _, err := Execute(tmplPath, d); err != nil {
		t.Fatal(err)
	}
}

func TestM8Missing(t *testing.T) {
	t.Parallel()
	d, err := Load(m8Fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if d.M8 != nil {
		t.Errorf("M8 without runs: %+v", d.M8)
	}
}

// A run without outages is not a flap run.
func TestM8RejectsARunWithoutOutages(t *testing.T) {
	t.Parallel()
	dir := m8Fixture(t)
	dst := filepath.Join(dir, M8Dir, M8Before)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("testdata", Q6Dir, chaos.SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, chaos.SummaryFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a Q6 summary was accepted as a flap run")
	}
}

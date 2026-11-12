package stalleval

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// t0 is the synthetic runs' bench start.
var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

// armSpec is one synthetic arm: 30 s at one request per second, the
// stall from 10 to 20 s.
type armSpec struct {
	timeout string // first_token_timeout, "" for none
	recs    []bench.Record
	ledger  []fake.Entry
	log     []Transition // gateway warn lines
	polls   []string     // breaker state at polls every 0.5 s from 0
	metrics string
}

func rec(atS float64, outcome string, latency float64, retryAfter string) bench.Record {
	r := bench.Record{At: atS, Outcome: outcome, LatencyMS: &latency, RetryAfter: retryAfter}
	if outcome == bench.OutcomeOK {
		ttft := latency / 2
		r.TTFTMS, r.Status = &ttft, 200
	} else if strings.HasPrefix(outcome, "http_") {
		_, _ = fmt.Sscanf(outcome, "http_%d", &r.Status)
	}
	return r
}

// pollStates returns the state at every 0.5 s poll for transitions ts.
func pollStates(ts []Transition) []string {
	var out []string
	for i := 0; i <= 60; i++ {
		out = append(out, stateAt(ts, float64(i)*0.5))
	}
	return out
}

// afterSpec is an after arm: five stalled arrivals from 10.0 s, opened
// at 12.45 s, a probe at 17 s that fails, closed at 21.3 s.
func afterSpec() armSpec {
	s := armSpec{timeout: "2s", metrics: `switchboard_provider_breaker_state{provider="fake"} 0
switchboard_provider_first_token_timeouts_total{provider="fake"} 5
switchboard_provider_retries_total{kind="timeout",provider="fake"} 5
`}
	for i := 0; i < 10; i++ {
		s.recs = append(s.recs, rec(float64(i), bench.OutcomeOK, 200, ""))
		s.ledger = append(s.ledger, fake.Entry{Time: at(float64(i)), Completed: true})
	}
	// Scheduled before the open: they wait out the timeout, then their
	// retry is refused.
	s.recs = append(s.recs, rec(10, "http_503", 2100, "10"), rec(11, "http_503", 2050, "10"), rec(12, "http_503", 2000, "10"))
	for _, a := range []float64{10.0, 10.1, 10.2, 10.3, 10.4, 17.0} {
		s.ledger = append(s.ledger, fake.Entry{Time: at(a), Stalled: true})
	}
	for _, a := range []float64{13, 15, 16, 18, 19} {
		s.recs = append(s.recs, rec(a, "http_503", 1, "5"))
	}
	s.recs = append(s.recs, rec(14, "http_503", 0.5, ""), rec(17, "http_503", 2005, "10"), rec(20, "http_503", 0.8, "1"))
	for i := 21; i < 30; i++ {
		s.recs = append(s.recs, rec(float64(i), bench.OutcomeOK, 210, ""))
		s.ledger = append(s.ledger, fake.Entry{Time: at(float64(i) + 0.001), Completed: true})
	}
	s.log = []Transition{{12.45, StateOpen}, {17.0, StateHalfOpen}, {19.0, StateOpen}, {21.0, StateHalfOpen}, {21.3, StateClosed}}
	s.polls = pollStates(s.log)
	return s
}

// beforeSpec is a before arm: every stalled request times out at the
// client and the breaker never leaves closed.
func beforeSpec() armSpec {
	s := armSpec{metrics: `switchboard_provider_breaker_state{provider="fake"} 0` + "\n"}
	for i := 0; i < 30; i++ {
		a := float64(i)
		if a >= 10 && a < 20 {
			s.recs = append(s.recs, rec(a, bench.OutcomeTimeout, 30000, ""))
			s.ledger = append(s.ledger, fake.Entry{Time: at(a + 0.001), Stalled: true})
			continue
		}
		s.recs = append(s.recs, rec(a, bench.OutcomeOK, 200, ""))
		s.ledger = append(s.ledger, fake.Entry{Time: at(a + 0.001), Completed: true})
	}
	s.polls = pollStates(nil)
	return s
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if strings.HasSuffix(path, ".gz") {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		if _, err := z.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		b = buf.Bytes()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, b)
}

func writeArm(t *testing.T, root, name string, s armSpec) {
	t.Helper()
	dir := filepath.Join(root, name)
	res := bench.Result{StartedAt: t0, Requests: len(s.recs)}
	res.Config.Rate, res.Config.DurationSec, res.Config.Stream = 1, 30, true
	writeJSONFile(t, filepath.Join(dir, "bench.json"), res)
	writeJSONFile(t, filepath.Join(dir, "meta.json"), map[string]string{"arm": name, "gateway_commit": "abc1234", "config": "eval/stall/" + name + ".yaml"})
	yaml := "providers:\n  - name: fake\n    type: openaicompat\n"
	if s.timeout != "" {
		yaml += "    resilience:\n      first_token_timeout: " + s.timeout + "\n"
	}
	writeFile(t, filepath.Join(dir, "gateway.yaml"), []byte(yaml))
	writeJSONFile(t, filepath.Join(dir, "stall.json"), map[string]any{"stalled": false, "held": 0,
		"transitions": []fake.Transition{{Time: at(10), Stalled: true}, {Time: at(20), Stalled: false}, {Time: at(25), Stalled: true}}})
	var lines bytes.Buffer
	for _, r := range s.recs {
		b, _ := json.Marshal(r)
		lines.Write(append(b, '\n'))
	}
	writeFile(t, filepath.Join(dir, "requests.jsonl.gz"), lines.Bytes())
	writeJSONFile(t, filepath.Join(dir, "ledger.json.gz"), map[string]any{"entries": s.ledger})
	var log bytes.Buffer
	log.WriteString(`{"time":"` + t0.Format(time.RFC3339Nano) + `","level":"WARN","msg":"llm call","provider":"fake"}` + "\nnot json at all\n")
	for _, tr := range s.log {
		fmt.Fprintf(&log, `{"time":%q,"level":"WARN","msg":"provider breaker state changed","provider":"fake","state":%q}`+"\n",
			at(tr.AtSeconds).Format(time.RFC3339Nano), tr.State)
	}
	writeFile(t, filepath.Join(dir, "gateway.log.gz"), log.Bytes())
	var polls bytes.Buffer
	for i, st := range s.polls {
		ts := at(float64(i) * 0.5)
		fmt.Fprintf(&polls, `{"t":%d.%09d,"body":{"providers":[{"provider":"fake","breaker":%q,"hedging":false}]}}`+"\n", ts.Unix(), ts.Nanosecond(), st)
	}
	writeFile(t, filepath.Join(dir, "breaker.jsonl.gz"), polls.Bytes())
	writeFile(t, filepath.Join(dir, "metrics.prom"), []byte(s.metrics))
}

func writeMeta(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "meta.json"), []byte(`{"commit":"abc1234","date":"2026-01-02T03:04:00Z","rate":1,"steady_seconds":10,"stall_seconds":10,"recover_seconds":10,"client_timeout":"30s","poll_interval_seconds":0.5,"arms":"before after","command":"eval/stall/run.sh"}`))
}

func writeSanity(t *testing.T, root, metrics string) {
	t.Helper()
	dir := filepath.Join(root, SanityDir)
	res := bench.Result{StartedAt: t0, Requests: 100, Outcomes: map[string]int{"ok": 100},
		TTFT: bench.Stats{Count: 100, P99: 600, P999: 950, Max: 1450}}
	writeJSONFile(t, filepath.Join(dir, SanityArm+".json"), res)
	writeFile(t, filepath.Join(dir, "meta.json"), []byte(`{"commit":"abc1234","date":"2026-01-02T03:10:00Z","command":"eval/hedge/run.sh"}`))
	writeFile(t, filepath.Join(dir, SanityArm+"-metrics.prom"), []byte(metrics))
}

func fixture(t *testing.T, after armSpec) string {
	t.Helper()
	root := t.TempDir()
	writeMeta(t, root)
	writeArm(t, root, "before", beforeSpec())
	writeArm(t, root, "after", after)
	writeSanity(t, root, `switchboard_provider_retries_total{kind="timeout",provider="fake"} 0`+"\n")
	return root
}

func near(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil, want %v", name, want)
		return
	}
	if math.Abs(*got-want) > 1e-6 {
		t.Errorf("%s: %v, want %v", name, *got, want)
	}
}

func analyse(t *testing.T, root string) *Summary {
	t.Helper()
	s, err := Analyse(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPhases(t *testing.T) {
	t.Parallel()
	s := analyse(t, fixture(t, afterSpec()))
	if len(s.Arms) != 2 || s.Arms[0].Name != "before" || s.Arms[1].Name != "after" {
		t.Fatalf("arms %+v", s.Arms)
	}
	tests := []struct {
		arm, phase         string
		requests, ok       int
		outcomes           map[string]int
		upstream, stalled  int
		failP99, failMax   float64
		from, to           float64
		okTTFTP50, okLat99 float64
	}{
		{"before", PhaseSteady, 10, 10, map[string]int{"ok": 10}, 10, 0, 0, 0, 0, 10, 100, 200},
		// The request at exactly 10 s is the stall's; at 20 s, the
		// recovery's.
		{"before", PhaseStalled, 10, 0, map[string]int{"timeout": 10}, 10, 10, 30000, 30000, 10, 20, 0, 0},
		{"before", PhaseRecovered, 10, 10, map[string]int{"ok": 10}, 10, 0, 0, 0, 20, 30, 100, 200},
		{"after", PhaseStalled, 10, 0, map[string]int{"http_503": 10}, 6, 6, 2100, 2100, 10, 20, 0, 0},
		{"after", PhaseRecovered, 10, 9, map[string]int{"ok": 9, "http_503": 1}, 9, 0, 0.8, 0.8, 20, 30, 105, 210},
	}
	for _, tt := range tests {
		a := s.Arm(tt.arm)
		var p *Phase
		for i := range a.Phases {
			if a.Phases[i].Name == tt.phase {
				p = &a.Phases[i]
			}
		}
		name := tt.arm + "/" + tt.phase
		if p.Requests != tt.requests || p.OK != tt.ok || p.Upstream != tt.upstream || p.UpstreamStalled != tt.stalled {
			t.Errorf("%s: %+v", name, p)
		}
		if fmt.Sprint(p.Outcomes) != fmt.Sprint(tt.outcomes) {
			t.Errorf("%s outcomes %v, want %v", name, p.Outcomes, tt.outcomes)
		}
		if p.Failures.P99 != tt.failP99 || p.Failures.Max != tt.failMax || p.FromSeconds != tt.from || p.ToSeconds != tt.to {
			t.Errorf("%s failures %+v window %v–%v", name, p.Failures, p.FromSeconds, p.ToSeconds)
		}
		if p.OKTTFT.P50 != tt.okTTFTP50 || p.OKLatency.P99 != tt.okLat99 {
			t.Errorf("%s ok TTFT %+v latency %+v", name, p.OKTTFT, p.OKLatency)
		}
	}
	if b := s.Arm("before"); b.StalledOK != 0 || b.ClientTimeouts != 10 || b.Ledger.Stalled != 10 {
		t.Errorf("before: stalled ok %d, client timeouts %d, ledger %+v", b.StalledOK, b.ClientTimeouts, b.Ledger)
	}
}

func TestBreaker(t *testing.T) {
	t.Parallel()
	logOnly := afterSpec()
	pollOnly := afterSpec()
	pollOnly.log = nil
	tests := []struct {
		name         string
		spec         armSpec
		source       string
		firstOpen    float64
		close        float64
		predErr      float64
		lastHalfOpen float64
	}{
		{"from the log", logOnly, SourceLog, 12.45, 1.3, 0.05, 3},
		// Polls every 0.5 s see the open at 12.5 and the close at 21.5.
		{"from the polls", pollOnly, SourcePoll, 12.5, 1.5, 0.1, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := analyse(t, fixture(t, tt.spec)).Arm("after").Breaker
			if b.Source != tt.source || b.Opens != 2 || b.OpensInStall != 2 || len(b.PollTransitions) != 5 {
				t.Fatalf("breaker %+v", b)
			}
			if tt.source == SourceLog && len(b.LogTransitions) != 5 {
				t.Errorf("log transitions %v", b.LogTransitions)
			}
			near(t, "first open", b.FirstOpenSeconds, tt.firstOpen)
			near(t, "time to open", b.TimeToOpenSeconds, tt.firstOpen-10)
			near(t, "fifth stalled arrival", b.FifthStalledArrivalSeconds, 10.4)
			near(t, "predicted open", b.PredictedOpenSeconds, 12.4)
			near(t, "prediction error", b.PredictionErrorSeconds, tt.predErr)
			near(t, "time to close", b.TimeToCloseSeconds, tt.close)
			near(t, "last half-open to stall end", b.LastHalfOpenToStallEndSeconds, tt.lastHalfOpen)
			near(t, "last probe", b.LastProbeSeconds, 17)
			near(t, "last probe to stall end", b.LastProbeToStallEndSeconds, 3)
			// 17 + 2 s timeout + 10 s cooldown + 1 s arrival gap - 20.
			near(t, "predicted close", b.PredictedCloseSeconds, 10)
			if b.Polls != 61 || b.PollGaps.P50 != 500 {
				t.Errorf("polls %d, gaps %+v", b.Polls, b.PollGaps)
			}
		})
	}
	before := analyse(t, fixture(t, afterSpec())).Arm("before").Breaker
	if before.Source != SourcePoll || before.Opens != 0 || before.TimeToOpenSeconds != nil || before.TimeToCloseSeconds != nil || before.PredictedOpenSeconds != nil {
		t.Errorf("before breaker %+v", before)
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	r := analyse(t, fixture(t, afterSpec())).Arm("after").Refusals
	// 13–19 s: scheduled while not closed. The 503s at 10–12 s were
	// scheduled before the open; the one at 20 s is in the recovery.
	if r.Count != 7 || r.WithRetryAfter != 6 || r.Probes != 1 {
		t.Fatalf("refusals %+v", r)
	}
	if r.Latency.P50 != 1 || r.Latency.Max != 2005 || r.Latency.P99 != 2005 {
		t.Errorf("latency %+v", r.Latency)
	}
	if math.Abs(r.RetryAfterShare-6.0/7) > 1e-12 || math.Abs(r.ProbeShare-1.0/7) > 1e-12 {
		t.Errorf("shares %v %v", r.RetryAfterShare, r.ProbeShare)
	}
}

func TestVerdicts(t *testing.T) {
	t.Parallel()
	s := analyse(t, fixture(t, afterSpec()))
	want := map[string]bool{
		"before/" + CheckOpens:            true,
		"before/" + CheckStalledOK:        true,
		"after/" + CheckOpenWithin:        true,  // 2.45 s
		"after/" + CheckPrediction:        true,  // 0.05 s
		"after/" + CheckRefusalP99:        false, // the probe is the p99 of 7
		"after/" + CheckRetryAfter:        false, // 6 of 7
		"after/" + CheckCloseWithin:       true,  // 1.3 s
		SanityArm + "/" + CheckNoTimeouts: true,
	}
	if len(s.Verdicts) != len(want) {
		t.Fatalf("%d verdicts: %+v", len(s.Verdicts), s.Verdicts)
	}
	for _, v := range s.Verdicts {
		k := v.Arm + "/" + v.Check
		held, ok := want[k]
		if !ok || v.Held != held {
			t.Errorf("%s: held %v, want %v (measured %v)", k, v.Held, held, v.Measured)
		}
	}
}

func TestJudge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    *float64
		op   string
		held bool
	}{
		{ptr(3.0), OpLE, true},
		{ptr(3.0001), OpLE, false},
		{ptr(5.0), OpLT, false},
		{ptr(-0.1), OpAbsLE, true},
		{ptr(-0.11), OpAbsLE, false},
		{ptr(100.0), OpEq, false},
		{nil, OpLE, false},
	}
	for _, tt := range tests {
		bound := 3.0
		switch tt.op {
		case OpLT:
			bound = 5
		case OpAbsLE:
			bound = 0.1
		case OpEq:
			bound = 0
		}
		if got := judge("a", "c", "h", tt.v, tt.op, bound, "s"); got.Held != tt.held {
			t.Errorf("%v %s %v: held %v", tt.v, tt.op, bound, got.Held)
		}
	}
}

func TestSanity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		metrics string
		fired   int
		series  bool
	}{
		{"absent series reads as 0", `switchboard_provider_hedges_total{event="sent",provider="fake"} 0` + "\n", 0, false},
		{"fired", `switchboard_provider_first_token_timeouts_total{provider="fake"} 3` + "\n", 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeMeta(t, root)
			writeSanity(t, root, tt.metrics)
			s := analyse(t, root)
			if len(s.Arms) != 0 || s.Sanity == nil {
				t.Fatalf("summary %+v", s)
			}
			if s.Sanity.FirstTokenTimeouts != tt.fired || s.Sanity.FirstTokenTimeoutsSeries != tt.series ||
				s.Sanity.Held != (tt.fired == 0) || s.Sanity.TTFT.Max != 1450 || s.Sanity.TimeoutSeconds != 2 {
				t.Errorf("sanity %+v", s.Sanity)
			}
		})
	}
}

func TestMetricsAbsentFirstTokenSeries(t *testing.T) {
	t.Parallel()
	b := analyse(t, fixture(t, afterSpec()))
	if m := b.Arm("before").Metrics; m.FirstTokenTimeouts != 0 || m.FirstTokenTimeoutsSeries {
		t.Errorf("before metrics %+v", m)
	}
	if m := b.Arm("after").Metrics; m.FirstTokenTimeouts != 5 || !m.FirstTokenTimeoutsSeries || m.Retries["timeout"] != 5 {
		t.Errorf("after metrics %+v", m)
	}
}

// SanityTimeout must be what the sanity run's config sets.
func TestSanityTimeoutMatchesConfig(t *testing.T) {
	t.Parallel()
	d, err := readFirstTokenTimeout("../../eval/hedge/off-ftt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if d != SanityTimeout {
		t.Errorf("eval/hedge/off-ftt.yaml has %v, SanityTimeout is %v", d, SanityTimeout)
	}
}

func TestMissing(t *testing.T) {
	t.Parallel()
	if _, err := Analyse(t.TempDir()); err == nil {
		t.Error("no error without meta.json")
	}
	root := t.TempDir()
	writeMeta(t, root)
	spec := afterSpec()
	writeArm(t, root, "after", spec)
	writeFile(t, filepath.Join(root, "after", "stall.json"), []byte(`{"transitions":[]}`))
	if _, err := Analyse(root); err == nil || !strings.Contains(err.Error(), "stall on") {
		t.Errorf("err %v, want the missing stall", err)
	}
}

func TestMainCLI(t *testing.T) {
	t.Parallel()
	root := fixture(t, afterSpec())
	var out, errOut bytes.Buffer
	if code := Main([]string{"--dir", root}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var s Summary
	if err := json.Unmarshal(out.Bytes(), &s); err != nil || len(s.Arms) != 2 {
		t.Fatalf("output %v: %s", err, out.String())
	}
	if code := Main([]string{"--dir", root, "extra"}, &out, &errOut); code != 2 {
		t.Errorf("exit %d with an extra argument", code)
	}
}

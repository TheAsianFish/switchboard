package results

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/stalleval"
)

func f64(v float64) *float64 { return &v }

// m10Summary is a synthetic summary: before never opens, after opens
// 2.249 s into the stall and closes 0.477 s after it.
func m10Summary() *stalleval.Summary {
	s := &stalleval.Summary{}
	s.Meta.Commit, s.Meta.Date, s.Meta.Command = "abc1234", "2026-01-02T03:04:05Z", "eval/stall/run.sh"
	s.Meta.Rate, s.Meta.SteadySeconds, s.Meta.StallSeconds, s.Meta.RecoverSeconds = 20, 20, 60, 40
	s.Meta.ClientTimeout, s.Meta.PollIntervalSeconds = "30s", 0.05
	phases := func(stalled map[string]int, stalledOK int) []stalleval.Phase {
		return []stalleval.Phase{
			{Name: stalleval.PhaseSteady, FromSeconds: 0, ToSeconds: 20.002, Requests: 401, OK: 401, Outcomes: map[string]int{"ok": 401},
				OKTTFT: bench.Stats{Count: 401, P50: 53.04, P99: 697.83}, Upstream: 401},
			{Name: stalleval.PhaseStalled, FromSeconds: 20.002, ToSeconds: 80.011, Requests: 1200, OK: stalledOK, Outcomes: stalled,
				Failures: bench.Stats{Count: 1200, P99: 30001.979}, Upstream: 1200, UpstreamStalled: 1200},
			{Name: stalleval.PhaseRecovered, FromSeconds: 80.011, ToSeconds: 120, Requests: 799, OK: 799, Outcomes: map[string]int{"ok": 799},
				OKTTFT: bench.Stats{Count: 799, P50: 52, P99: 662.2}, Upstream: 799},
		}
	}
	before := &stalleval.Arm{Name: "before", GatewayCommit: "ed0dedf", Config: "eval/stall/before.yaml",
		Phases: phases(map[string]int{"timeout": 1200}, 0), ClientTimeouts: 1200}
	before.Breaker = stalleval.Breaker{Source: stalleval.SourcePoll, Polls: 100, PollGaps: bench.Stats{P50: 62.9}}
	before.Ledger.Stalled = 1200
	after := &stalleval.Arm{Name: "after", GatewayCommit: "abc1234", Config: "eval/stall/after.yaml",
		FirstTokenTimeoutSeconds: f64(2), StallOffSeconds: 80.011,
		Phases: phases(map[string]int{"http_503": 1200}, 0)}
	after.Breaker = stalleval.Breaker{Source: stalleval.SourceLog, Polls: 100, PollGaps: bench.Stats{P50: 62.9},
		Transitions: []stalleval.Transition{{AtSeconds: 22.251, State: "open"}, {AtSeconds: 80.418, State: "half_open"}, {AtSeconds: 80.488, State: "closed"}},
		Opens:       5, OpensInStall: 5,
		FirstOpenSeconds: f64(22.251301), TimeToOpenSeconds: f64(2.248885),
		PredictedOpenSeconds: f64(22.251334), PredictionErrorSeconds: f64(-0.000033),
		TimeToCloseSeconds: f64(0.476967), LastProbeToStallEndSeconds: f64(11.611139), PredictedCloseSeconds: f64(0.438861)}
	after.Refusals = stalleval.Refusals{Count: 1155, WithRetryAfter: 1155, RetryAfterShare: 1, Probes: 4, ProbeShare: 4.0 / 1155,
		Latency: bench.Stats{Count: 1155, P50: 0.957, P99: 1.57, Max: 2095.598}}
	after.Metrics.FirstTokenTimeouts, after.Metrics.Retries = 51, map[string]int{"timeout": 51}
	after.Ledger.Stalled = 51
	s.Arms = []*stalleval.Arm{before, after}
	s.Sanity = &stalleval.Sanity{Commit: "abc1234", Requests: 12000, Sent: 13000, Upstream: 13000, TimeoutSeconds: 2,
		TTFT: bench.Stats{P99: 637.909, P999: 952.203, Max: 1455.891}, Held: true}
	s.Verdicts = []stalleval.Verdict{
		{Arm: "before", Check: stalleval.CheckOpens, Measured: f64(0), Op: stalleval.OpEq, Held: true},
		{Arm: "before", Check: stalleval.CheckStalledOK, Measured: f64(0), Op: stalleval.OpEq, Held: true},
		{Arm: "after", Check: stalleval.CheckOpenWithin, Measured: f64(2.248885), Op: stalleval.OpLE, Bound: 3, Held: true},
		{Arm: "after", Check: stalleval.CheckPrediction, Measured: f64(-0.000033), Op: stalleval.OpAbsLE, Bound: 0.1, Held: true},
		{Arm: "after", Check: stalleval.CheckRefusalP99, Measured: f64(1.57), Op: stalleval.OpLT, Bound: 5, Held: true},
		{Arm: "after", Check: stalleval.CheckRetryAfter, Measured: f64(100), Op: stalleval.OpEq, Bound: 100, Held: true},
		{Arm: "after", Check: stalleval.CheckCloseWithin, Measured: f64(12.1), Op: stalleval.OpLE, Bound: 11, Held: false},
		{Arm: stalleval.SanityArm, Check: stalleval.CheckNoTimeouts, Measured: f64(0), Op: stalleval.OpEq, Held: true},
	}
	return s
}

func TestRenderM10(t *testing.T) {
	t.Parallel()
	m := buildM10(m10Summary())
	aligned := m10Summary()
	aligned.Meta.StallSeconds = 37
	aligned.Arms = aligned.Arms[1:]
	aligned.Arms[0].Breaker.TimeToCloseSeconds = f64(11.43)
	aligned.Arms[0].Breaker.PredictedCloseSeconds = f64(11.41)
	m.Aligned = buildM10Aligned(m10Summary(), aligned)
	addM10AlignedClose(m)
	m.Trace = &M10Trace{Command: "scripts/trace-check.sh --local --stall", Config: "eval/trace/stall.yaml", TimeoutMS: "300", TTFT: "331.9",
		Attempts: []M10Attempt{{N: "1", Outcome: "first_token_timeout", DurationMS: "300.9"}, {N: "2", Outcome: "won", DurationMS: "172.2"}}}
	if m.Timeout != "2 s" || m.ClientTimeout != "30 s" || m.PollGap != "63" || m.WorstClose != "12.05" || m.HalfOpenAfterEnd != "0.407" {
		t.Errorf("prose %+v", m)
	}
	if m.Aligned.OverAsPredicted != "yes" || m.Aligned.Close != "11.430" {
		t.Errorf("aligned %+v", m.Aligned)
	}
	out, err := Execute(tmplPath, Data{M10: m})
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	for _, want := range []string{
		"| stalled | 20.0–80.0 | 1,200 | 0 | `timeout` 1,200 | – | 30,002.0 | 1,200 (1,200 held) | 1,200 | 0 | `http_503` 1,200 | – | 30,002.0 | 1,200 (1,200 held) |",
		"| Opens (run / stalled phase) | 0 / 0 | 5 / 5 |",
		"| First open minus prediction (ms) | – | -0.03 |",
		"| First-token timeouts / timeout retries (gateway metrics) | – | 51 / 51 |",
		"1,155, p50 0.96 ms, p99 1.57 ms, max 2,095.6 ms",
		"4 of them (0.3%)",
		"| After: closed after the provider answers again (main run) | ≤ 11 s (one cooldown + 1 s) | 12.100 s | no |",
		"| After: closed after the provider answers again, stall ending just after a probe (aligned run) | ≤ 11 s (one cooldown + 1 s) | 11.430 s | no |",
		"| 0 first-token timeouts on Q4's hedging-off arm | 0 | 0 (largest TTFT 1,455.9 ms against the 2 s timeout; 13,000 upstream requests for 13,000 sent) | yes |",
		"| Stall end to closed (s) | 0.477 | 11.430 |",
		"| Closed within 11 s | yes | no |",
		"over the bound as predicted: yes",
		"attempt 1 ended `first_token_timeout` after 300.9 ms; attempt 2 ended `won` after 172.2 ms;",
		"the counter exported no series: it never fired",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("render lacks %q", want)
		}
	}
	if strings.Contains(doc, "Not measured yet. Run `make bench-stall`") {
		t.Error("M10 still listed as not measured")
	}
	if !strings.HasPrefix(m.Headline, "Provider stalled at 20 req/s: breaker opened 0 times in 60 s before; with a 2 s first-token timeout, open 2.249 s") {
		t.Errorf("headline %q", m.Headline)
	}
	if !strings.HasSuffix(m.Headline, "(11.430 s when the stall ended just after a probe)") {
		t.Errorf("headline %q leaves out the aligned run's close", m.Headline)
	}
}

func TestM10NotMeasured(t *testing.T) {
	t.Parallel()
	d, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.M10 != nil {
		t.Fatal("M10 section without a run")
	}
	out, err := Execute(tmplPath, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Not measured yet. Run `make bench-stall`") {
		t.Error("no not-measured line")
	}
}

func TestM10Trace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("meta.json", `{"commit":"abc1234","date":"2026-01-02T03:04:05Z","command":"scripts/trace-check.sh --local --stall","config":"eval/trace/stall.yaml"}`)
	write(M10TraceFile, `{"data":[{"spans":[
{"operationName":"provider.attempt","duration":172210,"tags":[{"key":"switchboard.attempt","value":2},{"key":"switchboard.outcome","value":"won"},{"key":"switchboard.first_token_timeout_ms","value":300}]},
{"operationName":"provider","duration":483106,"tags":[{"key":"switchboard.ttft_ms","value":331.935}]},
{"operationName":"provider.attempt","duration":300878,"tags":[{"key":"switchboard.attempt","value":1},{"key":"switchboard.outcome","value":"first_token_timeout"},{"key":"switchboard.first_token_timeout_ms","value":300}]}
]}]}`)
	tr, err := loadM10Trace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TimeoutMS != "300" || tr.TTFT != "331.9" || len(tr.Attempts) != 2 ||
		tr.Attempts[0] != (M10Attempt{N: "1", Outcome: "first_token_timeout", DurationMS: "300.9"}) ||
		tr.Attempts[1] != (M10Attempt{N: "2", Outcome: "won", DurationMS: "172.2"}) {
		t.Errorf("trace %+v", tr)
	}
	if tr, err := loadM10Trace(t.TempDir()); tr != nil || err != nil {
		t.Errorf("missing trace: %+v, %v", tr, err)
	}
}

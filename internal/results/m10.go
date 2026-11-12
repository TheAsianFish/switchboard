package results

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/stalleval"
)

// M10Dir holds `make bench-stall`'s arms (before/, after/, optionally
// off/) and the sanity check on Q4's hedging-off arm (q4-off-ftt/).
const M10Dir = "m10-stall"

// M10AlignedDir holds the aligned run: `make bench-stall`'s after arm
// with STALL=37, so the stall ends just after a half-open probe went out
// (PLAN.md, M10's changes made while building).
const M10AlignedDir = "m10-stall-aligned"

// M10TraceDir holds the trace of one streamed request whose first
// attempt hit the first-token timeout (scripts/trace-check.sh --stall).
const M10TraceDir = "m10-trace"

// M10TraceFile is the trace in M10TraceDir, Jaeger's API JSON.
const M10TraceFile = "trace-first-token-timeout.json"

// M10 is the stalled-provider section: the same three phases through a
// gateway without and with the first-token timeout.
type M10 struct {
	Summary *stalleval.Summary
	// Arms are the arms present, in stalleval.Arms order, formatted.
	Arms []M10Arm
	// Phases are the per-phase table's rows, one cell per arm.
	Phases []M10Phase
	// Breaker is the breaker table: one number per row, one value per arm.
	Breaker []M10Row
	// Refusals is the after arm's refusals; nil without an after arm.
	Refusals *M10Refusals
	Checks   []M10Check
	Sanity   *M10Sanity

	// For prose, from the run's meta and its first arm: rate, phase
	// lengths, client timeout, poll interval and median poll gap, the
	// timeout, the breaker's cooldown and PLAN.md's worst case for the
	// close (cooldown + timeout + one arrival gap).
	Rate, Steady, Stall, Recover, ClientTimeout string
	PollInterval, PollGap, Timeout, Cooldown    string
	WorstClose                                  string
	// After-arm numbers for prose and the README, "–" without it: the
	// time to open, time to close, refusals' p99, the last probe before
	// the stall's end to the end, and the next half-open after the end.
	TimeToOpen, TimeToClose, RefusalP99 string
	LastProbe, HalfOpenAfterEnd         string
	// BeforeOpens is the before arm's opens, "–" without it.
	BeforeOpens string
	Headline    string
	// Aligned compares the close in the main after arm and the aligned
	// run; nil without the aligned run.
	Aligned *M10Aligned
	// Trace is the traced request; nil without it.
	Trace *M10Trace
}

// M10Aligned is the close in the main run's after arm and the aligned
// run side by side.
type M10Aligned struct {
	Meta     stalleval.Meta
	Hardware string
	// Rows have two values: main run, aligned run.
	Rows []M10Row
	// Predicted and Close are the aligned run's predicted and measured
	// close (s after the stall's end); Bound the close bound;
	// OverAsPredicted whether the close came after the bound, as the
	// prediction said ("yes"/"no", "–" without a close).
	Predicted, Close, Bound, OverAsPredicted string
}

// M10Trace is the traced request's attempts.
type M10Trace struct {
	Commit, Date, Command, Config string
	// TimeoutMS is the attempts' first-token timeout; TTFT the provider
	// span's.
	TimeoutMS, TTFT string
	Attempts        []M10Attempt
}

// M10Attempt is one provider.attempt span.
type M10Attempt struct {
	N, Outcome, DurationMS string
}

// M10Arm is one arm's provenance.
type M10Arm struct {
	Name, Label, Commit, Config, Hardware, Date, Command string
}

// M10Phase is one phase in every arm.
type M10Phase struct {
	Name, Window string
	Cells        []M10PhaseCell
}

// M10PhaseCell is a phase in one arm. Latencies are milliseconds with one
// decimal, "–" without a request to measure.
type M10PhaseCell struct {
	Requests, OK, Errors, TTFT, FailureP99, Upstream string
}

// M10Row is one named number in every arm.
type M10Row struct {
	Name   string
	Values []string
}

// M10Refusals is the after arm's refusals, formatted.
type M10Refusals struct {
	Count, P50, P99, Max, RetryAfter, Probes, ProbeShare string
}

// M10Check is one hypothesis against its bound.
type M10Check struct {
	Hypothesis, Bound, Measured, Held string
}

// M10Sanity is the sanity check, with the committed Q4 off arm beside it.
type M10Sanity struct {
	Meta                            *stalleval.Sanity
	Hardware                        string
	Requests, Fired, Timeout        string
	TTFTP99, TTFTP999, TTFTMax      string
	Q4Commit, Q4TTFTP999, Q4TTFTMax string
	// FiredNote says when the counter exported no series.
	FiredNote string
	// Sent and Upstream are the requests sent and the provider's ledger
	// count; equal when no timeout fired (a fired one is retried).
	Sent, Upstream string
}

const dash = "–"

// loadM10 reads M10's runs under resultsDir: the main run, the aligned
// run and the trace, and, for the sanity check's comparison, Q4's
// committed hedging-off arm.
func loadM10(resultsDir string) (*M10, error) {
	dir, q4dir := filepath.Join(resultsDir, M10Dir), filepath.Join(resultsDir, Q4Dir)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	s, err := stalleval.Analyse(dir)
	if err != nil {
		return nil, fmt.Errorf("results: m10: %w", err)
	}
	if len(s.Arms) == 0 && s.Sanity == nil {
		return nil, nil
	}
	m := buildM10(s)
	if s.Sanity != nil {
		var q4 Q4Meta
		var off bench.Result
		if err := readJSON(filepath.Join(q4dir, "meta.json"), &q4); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		} else if err == nil {
			if err := readJSON(filepath.Join(q4dir, "off.json"), &off); err != nil {
				return nil, err
			}
			m.Sanity.Q4Commit = q4.Commit
			m.Sanity.Q4TTFTP999, m.Sanity.Q4TTFTMax = thousands1(off.TTFT.P999), thousands1(off.TTFT.Max)
		}
	}
	if m.Aligned, err = loadM10Aligned(filepath.Join(resultsDir, M10AlignedDir), s); err != nil {
		return nil, err
	}
	addM10AlignedClose(m)
	if m.Trace, err = loadM10Trace(filepath.Join(resultsDir, M10TraceDir)); err != nil {
		return nil, err
	}
	return m, nil
}

// addM10AlignedClose puts the aligned run's close beside the main run's
// in the hypotheses and the headline: the main run's stall ended late in
// an open window, so its close alone reads as the bound holding when it
// holds only for that alignment.
func addM10AlignedClose(m *M10) {
	al := m.Aligned
	if al == nil || al.Close == dash {
		return
	}
	held := "yes"
	if al.OverAsPredicted == "yes" {
		held = "no"
	}
	row := M10Check{
		Hypothesis: "After: closed after the provider answers again, stall ending just after a probe (aligned run)",
		Bound:      "≤ " + al.Bound + " s (one cooldown + 1 s)",
		Measured:   al.Close + " s",
		Held:       held,
	}
	// Beside the main run's close, or last without one.
	at := len(m.Checks)
	for i, c := range m.Checks {
		if strings.HasPrefix(c.Hypothesis, "After: closed after the provider answers again") {
			at = i + 1
		}
	}
	m.Checks = append(m.Checks[:at], append([]M10Check{row}, m.Checks[at:]...)...)
	m.Headline += fmt.Sprintf(" (%s s when the stall ended just after a probe)", al.Close)
}

func loadM10Aligned(dir string, main *stalleval.Summary) (*M10Aligned, error) {
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	s, err := stalleval.Analyse(dir)
	if err != nil {
		return nil, fmt.Errorf("results: m10 aligned: %w", err)
	}
	return buildM10Aligned(main, s), nil
}

// buildM10Aligned compares main's after arm with the aligned run's; nil
// without the latter.
func buildM10Aligned(main, s *stalleval.Summary) *M10Aligned {
	al := s.Arm("after")
	if al == nil {
		return nil
	}
	m := &M10Aligned{Meta: s.Meta, Hardware: describeHardware(al.Hardware),
		Bound: num(stalleval.CloseWithinSeconds), OverAsPredicted: dash,
		Predicted: secs3(al.Breaker.PredictedCloseSeconds), Close: secs3(al.Breaker.TimeToCloseSeconds)}
	if c := al.Breaker.TimeToCloseSeconds; c != nil {
		m.OverAsPredicted = yesNo(*c > stalleval.CloseWithinSeconds)
	}
	arms := []*stalleval.Arm{main.Arm("after"), al}
	stall := []string{num(main.Meta.StallSeconds), num(s.Meta.StallSeconds)}
	rows := []struct {
		name string
		f    func(i int, a *stalleval.Arm) string
	}{
		{"Stall length (s)", func(i int, _ *stalleval.Arm) string { return stall[i] }},
		{"Opens (run / stalled phase)", func(_ int, a *stalleval.Arm) string {
			return fmt.Sprintf("%d / %d", a.Breaker.Opens, a.Breaker.OpensInStall)
		}},
		{"Last probe in the stall to its end (s)", func(_ int, a *stalleval.Arm) string { return secs3(a.Breaker.LastProbeToStallEndSeconds) }},
		{"Predicted close: probe + timeout + cooldown + one arrival gap, from the stall's end (s)", func(_ int, a *stalleval.Arm) string {
			return secs3(a.Breaker.PredictedCloseSeconds)
		}},
		{"Stall end to closed (s)", func(_ int, a *stalleval.Arm) string { return secs3(a.Breaker.TimeToCloseSeconds) }},
		{"Closed within " + num(stalleval.CloseWithinSeconds) + " s", func(_ int, a *stalleval.Arm) string {
			if c := a.Breaker.TimeToCloseSeconds; c != nil {
				return yesNo(*c <= stalleval.CloseWithinSeconds)
			}
			return dash
		}},
	}
	for _, r := range rows {
		row := M10Row{Name: r.name}
		for i, a := range arms {
			if a == nil {
				row.Values = append(row.Values, dash)
				continue
			}
			row.Values = append(row.Values, r.f(i, a))
		}
		m.Rows = append(m.Rows, row)
	}
	return m
}

// loadM10Trace reads the trace's provider.attempt spans in attempt order.
func loadM10Trace(dir string) (*M10Trace, error) {
	var meta struct {
		Commit  string `json:"commit"`
		Date    string `json:"date"`
		Command string `json:"command"`
		Config  string `json:"config"`
	}
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		Data []struct {
			Spans []struct {
				OperationName string `json:"operationName"`
				Duration      int64  `json:"duration"` // microseconds
				Tags          []struct {
					Key   string `json:"key"`
					Value any    `json:"value"`
				} `json:"tags"`
			} `json:"spans"`
		} `json:"data"`
	}
	path := filepath.Join(dir, M10TraceFile)
	if err := readJSON(path, &doc); err != nil {
		return nil, err
	}
	if len(doc.Data) != 1 {
		return nil, fmt.Errorf("results: %s: %d traces, want 1", path, len(doc.Data))
	}
	t := &M10Trace{Commit: meta.Commit, Date: meta.Date, Command: meta.Command, Config: meta.Config, TimeoutMS: dash, TTFT: dash}
	type attempt struct {
		n int
		M10Attempt
	}
	var atts []attempt
	for _, sp := range doc.Data[0].Spans {
		tags := map[string]any{}
		for _, tg := range sp.Tags {
			tags[tg.Key] = tg.Value
		}
		switch sp.OperationName {
		case "provider":
			if v, ok := tags["switchboard.ttft_ms"].(float64); ok {
				t.TTFT = f1(v)
			}
		case "provider.attempt":
			n, _ := tags["switchboard.attempt"].(float64)
			out, _ := tags["switchboard.outcome"].(string)
			if v, ok := tags["switchboard.first_token_timeout_ms"].(float64); ok {
				t.TimeoutMS = num(v)
			}
			atts = append(atts, attempt{n: int(n), M10Attempt: M10Attempt{N: strconv.Itoa(int(n)), Outcome: out,
				DurationMS: f1(float64(sp.Duration) / 1000)}})
		}
	}
	if len(atts) == 0 {
		return nil, fmt.Errorf("results: %s has no provider.attempt span", path)
	}
	slices.SortFunc(atts, func(a, b attempt) int { return a.n - b.n })
	for _, a := range atts {
		t.Attempts = append(t.Attempts, a.M10Attempt)
	}
	return t, nil
}

func buildM10(s *stalleval.Summary) *M10 {
	meta := s.Meta
	m := &M10{Summary: s,
		Rate: num(meta.Rate), Steady: num(meta.SteadySeconds), Stall: num(meta.StallSeconds),
		Recover: num(meta.RecoverSeconds), ClientTimeout: prose(meta.ClientTimeout),
		PollInterval: num(meta.PollIntervalSeconds * 1000),
		Cooldown:     num(stalleval.BreakerCooldownSeconds),
		PollGap:      dash, Timeout: dash, WorstClose: dash,
		TimeToOpen: dash, TimeToClose: dash, RefusalP99: dash, LastProbe: dash, HalfOpenAfterEnd: dash,
		BeforeOpens: dash,
	}
	if d, err := time.ParseDuration(meta.ClientTimeout); err == nil {
		m.ClientTimeout = num(d.Seconds()) + " s"
	}
	for _, a := range s.Arms {
		label := strings.ToUpper(a.Name[:1]) + a.Name[1:]
		commit := a.GatewayCommit
		m.Arms = append(m.Arms, M10Arm{Name: a.Name, Label: label, Commit: commit, Config: a.Config,
			Hardware: describeHardware(a.Hardware), Date: meta.Date, Command: meta.Command})
		if a.FirstTokenTimeoutSeconds != nil && m.Timeout == dash {
			m.Timeout = num(*a.FirstTokenTimeoutSeconds) + " s"
			if meta.Rate > 0 {
				m.WorstClose = strconv.FormatFloat(stalleval.BreakerCooldownSeconds+*a.FirstTokenTimeoutSeconds+1/meta.Rate, 'f', 2, 64)
			}
		}
		if m.PollGap == dash && a.Breaker.Polls > 1 {
			m.PollGap = strconv.FormatFloat(a.Breaker.PollGaps.P50, 'f', 0, 64)
		}
	}
	if len(s.Arms) > 0 {
		ref := s.Arms[len(s.Arms)-1]
		for i, p := range ref.Phases {
			row := M10Phase{Name: p.Name, Window: fmt.Sprintf("%.1f–%.1f", p.FromSeconds, p.ToSeconds)}
			for _, a := range s.Arms {
				row.Cells = append(row.Cells, m10Cell(a.Phases[i]))
			}
			m.Phases = append(m.Phases, row)
		}
	}
	m.Breaker = m10Breaker(s.Arms)
	if b := s.Arm("before"); b != nil {
		m.BeforeOpens = strconv.Itoa(b.Breaker.Opens)
	}
	if a := s.Arm("after"); a != nil {
		b := a.Breaker
		m.TimeToOpen, m.TimeToClose = secs3(b.TimeToOpenSeconds), secs3(b.TimeToCloseSeconds)
		m.LastProbe = secs3(b.LastProbeToStallEndSeconds)
		for _, t := range b.Transitions {
			if t.State == stalleval.StateHalfOpen && t.AtSeconds >= a.StallOffSeconds {
				m.HalfOpenAfterEnd = strconv.FormatFloat(t.AtSeconds-a.StallOffSeconds, 'f', 3, 64)
				break
			}
		}
		r := a.Refusals
		if r.Count > 0 {
			m.RefusalP99 = ms2(r.Latency.P99)
			m.Refusals = &M10Refusals{Count: thousands(strconv.Itoa(r.Count)),
				P50: ms2(r.Latency.P50), P99: ms2(r.Latency.P99), Max: thousands1(r.Latency.Max),
				RetryAfter: pct1(r.RetryAfterShare), Probes: strconv.Itoa(r.Probes), ProbeShare: pct1(r.ProbeShare)}
		}
	}
	for _, v := range s.Verdicts {
		m.Checks = append(m.Checks, m10Check(s, v))
	}
	if san := s.Sanity; san != nil {
		m.Sanity = &M10Sanity{Meta: san, Hardware: describeHardware(san.Hardware),
			Requests: thousands(strconv.Itoa(san.Requests)), Fired: strconv.Itoa(san.FirstTokenTimeouts),
			Timeout: num(san.TimeoutSeconds) + " s",
			Sent:    thousands(strconv.Itoa(san.Sent)), Upstream: thousands(strconv.Itoa(san.Upstream)),
			TTFTP99: thousands1(san.TTFT.P99), TTFTP999: thousands1(san.TTFT.P999), TTFTMax: thousands1(san.TTFT.Max),
			Q4Commit: dash, Q4TTFTP999: dash, Q4TTFTMax: dash}
		if !san.FirstTokenTimeoutsSeries {
			m.Sanity.FiredNote = " (the counter exported no series: it never fired)"
		}
	}
	before, after := "–", "–"
	if b := s.Arm("before"); b != nil {
		before = fmt.Sprintf("breaker opened %d times in %s s before", b.Breaker.Opens, m.Stall)
	}
	if a := s.Arm("after"); a != nil {
		after = fmt.Sprintf("with a %s first-token timeout, open %s s into the stall, refusals p99 %s ms, closed %s s after it ended",
			m.Timeout, m.TimeToOpen, m.RefusalP99, m.TimeToClose)
	}
	m.Headline = fmt.Sprintf("Provider stalled at %s req/s: %s; %s", m.Rate, before, after)
	return m
}

func m10Cell(p stalleval.Phase) M10PhaseCell {
	c := M10PhaseCell{Requests: thousands(strconv.Itoa(p.Requests)), OK: thousands(strconv.Itoa(p.OK)),
		Errors: dash, TTFT: dash, FailureP99: dash, Upstream: thousands(strconv.Itoa(p.Upstream))}
	var errs []string
	for _, k := range sortedOutcomes(p.Outcomes) {
		if k != bench.OutcomeOK {
			errs = append(errs, fmt.Sprintf("`%s` %s", k, thousands(strconv.Itoa(p.Outcomes[k]))))
		}
	}
	if len(errs) > 0 {
		c.Errors = strings.Join(errs, ", ")
	}
	if p.OKTTFT.Count > 0 {
		c.TTFT = f1(p.OKTTFT.P50) + " / " + f1(p.OKTTFT.P99)
	}
	if p.Failures.Count > 0 {
		c.FailureP99 = thousands1(p.Failures.P99)
	}
	if p.UpstreamStalled > 0 {
		c.Upstream += " (" + thousands(strconv.Itoa(p.UpstreamStalled)) + " held)"
	}
	return c
}

func sortedOutcomes(m map[string]int) []string {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return sortedKeys(keys)
}

func m10Breaker(arms []*stalleval.Arm) []M10Row {
	rows := []struct {
		name string
		f    func(*stalleval.Arm) string
	}{
		{"Transitions read from", func(a *stalleval.Arm) string {
			if a.Breaker.Source == stalleval.SourceLog {
				return "gateway log"
			}
			return "`/providers` polls"
		}},
		{"Opens (run / stalled phase)", func(a *stalleval.Arm) string {
			return fmt.Sprintf("%d / %d", a.Breaker.Opens, a.Breaker.OpensInStall)
		}},
		{"Stall start to first open (s)", func(a *stalleval.Arm) string { return secs3(a.Breaker.TimeToOpenSeconds) }},
		{"Fifth stalled arrival + timeout (s after start)", func(a *stalleval.Arm) string { return secs3(a.Breaker.PredictedOpenSeconds) }},
		{"First open (s after start)", func(a *stalleval.Arm) string { return secs3(a.Breaker.FirstOpenSeconds) }},
		{"First open minus prediction (ms)", func(a *stalleval.Arm) string {
			if a.Breaker.PredictionErrorSeconds == nil {
				return dash
			}
			return added(*a.Breaker.PredictionErrorSeconds * 1000)
		}},
		{"Stall end to closed (s)", func(a *stalleval.Arm) string { return secs3(a.Breaker.TimeToCloseSeconds) }},
		{"Last probe in the stall to its end (s)", func(a *stalleval.Arm) string { return secs3(a.Breaker.LastProbeToStallEndSeconds) }},
		{"Upstream requests held by the stall", func(a *stalleval.Arm) string { return thousands(strconv.Itoa(a.Ledger.Stalled)) }},
		{"First-token timeouts / timeout retries (gateway metrics)", func(a *stalleval.Arm) string {
			if a.FirstTokenTimeoutSeconds == nil {
				return dash
			}
			return fmt.Sprintf("%d / %d", a.Metrics.FirstTokenTimeouts, a.Metrics.Retries["timeout"])
		}},
		{"Client timeouts", func(a *stalleval.Arm) string { return thousands(strconv.Itoa(a.ClientTimeouts)) }},
	}
	out := make([]M10Row, 0, len(rows))
	for _, r := range rows {
		row := M10Row{Name: r.name}
		for _, a := range arms {
			row.Values = append(row.Values, r.f(a))
		}
		out = append(out, row)
	}
	return out
}

func m10Check(s *stalleval.Summary, v stalleval.Verdict) M10Check {
	c := M10Check{Held: yesNo(v.Held), Measured: dash}
	val := func(f func(float64) string) {
		if v.Measured != nil {
			c.Measured = f(*v.Measured)
		}
	}
	switch v.Check {
	case stalleval.CheckOpens:
		c.Hypothesis, c.Bound = "Before: the breaker never opens", "0 opens"
		val(func(x float64) string { return num(x) + " opens" })
	case stalleval.CheckStalledOK:
		c.Hypothesis, c.Bound = "Before: no request scheduled in the stall completes", "0"
		val(func(x float64) string {
			if a := s.Arm(v.Arm); a != nil {
				return fmt.Sprintf("%s of %s", num(x), thousands(strconv.Itoa(a.Phases[1].Requests)))
			}
			return num(x)
		})
	case stalleval.CheckOpenWithin:
		c.Hypothesis, c.Bound = "After: open soon after the stall starts", "≤ "+strconv.FormatFloat(v.Bound, 'f', 1, 64)+" s"
		val(func(x float64) string { return strconv.FormatFloat(x, 'f', 3, 64) + " s" })
	case stalleval.CheckPrediction:
		c.Hypothesis = "After: open at the fifth stalled upstream arrival plus the timeout"
		c.Bound = "within " + num(v.Bound) + " s"
		val(func(x float64) string { return added(x*1000) + " ms" })
	case stalleval.CheckRefusalP99:
		c.Hypothesis, c.Bound = "After: refusals in the stall are fast (p99)", "< "+num(v.Bound)+" ms"
		val(func(x float64) string { return ms2(x) + " ms" })
	case stalleval.CheckRetryAfter:
		c.Hypothesis, c.Bound = "After: refusals carry `Retry-After`", num(v.Bound)+"%"
		val(func(x float64) string {
			out := pctNum(x/100) + "%"
			if a := s.Arm(v.Arm); a != nil {
				out += fmt.Sprintf(" (%s of %s)", thousands(strconv.Itoa(a.Refusals.WithRetryAfter)), thousands(strconv.Itoa(a.Refusals.Count)))
			}
			return out
		})
	case stalleval.CheckCloseWithin:
		c.Hypothesis = "After: closed after the provider answers again (main run)"
		c.Bound = "≤ " + num(v.Bound) + " s (one cooldown + 1 s)"
		val(func(x float64) string { return strconv.FormatFloat(x, 'f', 3, 64) + " s" })
	case stalleval.CheckNoTimeouts:
		c.Hypothesis, c.Bound = "0 first-token timeouts on Q4's hedging-off arm", "0"
		val(func(x float64) string {
			out := num(x)
			if san := s.Sanity; san != nil {
				out += fmt.Sprintf(" (largest TTFT %s ms against the %s s timeout; %s upstream requests for %s sent)",
					thousands1(san.TTFT.Max), num(san.TimeoutSeconds), thousands(strconv.Itoa(san.Upstream)), thousands(strconv.Itoa(san.Sent)))
			}
			return out
		})
	default:
		c.Hypothesis, c.Bound = v.Arm+": "+v.Hypothesis, v.Op+" "+num(v.Bound)+" "+v.Unit
		val(num)
	}
	return c
}

// secs3 is a number of seconds with three decimals, "–" when nil.
func secs3(v *float64) string {
	if v == nil {
		return dash
	}
	return strconv.FormatFloat(*v, 'f', 3, 64)
}

func ms2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// thousands1 is v with one decimal and thousands separators.
func thousands1(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	whole, frac, _ := strings.Cut(s, ".")
	return thousands(whole) + "." + frac
}

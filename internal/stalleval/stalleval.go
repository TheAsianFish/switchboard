// Package stalleval reads the raw files of `make bench-stall` (M10:
// eval/stall/run.sh) and of its sanity check on Q4's hedging-off arm, and
// reduces them to the numbers PLAN.md fixes for M10 before the full runs:
// per-phase outcomes and latency, the breaker's transitions, refusals,
// and each hypothesis judged against its bound.
package stalleval

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"slices"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// Arms are the arm subdirectories eval/stall/run.sh can write, in the
// order they are reported. An arm is present when its bench.json is.
var Arms = []string{"before", "after", "off"}

// SanityDir is the subdirectory holding the sanity check's run:
// `ARMS=off-ftt OUT=eval/results/m10-stall/q4-off-ftt eval/hedge/run.sh`.
const SanityDir = "q4-off-ftt"

// SanityArm is the arm name eval/hedge/run.sh gives the sanity run's files.
const SanityArm = "off-ftt"

// SanityTimeout is eval/hedge/off-ftt.yaml's first_token_timeout. The
// hedge harness does not copy its config into the results, so it is
// recorded here; a test checks it against the file.
const SanityTimeout = 2 * time.Second

// The phases, in order.
const (
	PhaseSteady    = "steady"
	PhaseStalled   = "stalled"
	PhaseRecovered = "recovered"
)

// Breaker states as /providers and the gateway's log name them.
const (
	StateClosed   = "closed"
	StateOpen     = "open"
	StateHalfOpen = "half_open"
)

// Transition sources.
const (
	SourceLog  = "log"
	SourcePoll = "poll"
)

// PLAN.md M10's bounds, fixed before the full runs.
const (
	// OpenWithinSeconds: the breaker is open within this long of the
	// stall's start.
	OpenWithinSeconds = 3.0
	// PredictionToleranceSeconds: the open is this close to the fifth
	// stalled upstream arrival plus the first-token timeout.
	PredictionToleranceSeconds = 0.1
	// RefusalP99MS: refusals' p99 is under this.
	RefusalP99MS = 5.0
	// RetryAfterPercent: the share of refusals that carry Retry-After.
	RetryAfterPercent = 100.0
	// CloseWithinSeconds: one cooldown (10 s) plus 1 s after the stall's
	// end, the breaker has closed.
	CloseWithinSeconds = 11.0
	// BreakerCooldownSeconds is the breaker's default cooldown, which
	// both arms keep.
	BreakerCooldownSeconds = 10.0
	// BreakerFailures is the breaker's default: it opens after this many
	// failed attempts in a row, so the prediction uses the fifth stalled
	// arrival.
	BreakerFailures = 5
)

// Meta is the run-level meta.json eval/stall/run.sh writes.
type Meta struct {
	Commit       string `json:"commit"`
	Date         string `json:"date"`
	BeforeRef    string `json:"before_ref"`
	FakeProvider struct {
		Latency       string  `json:"latency"`
		LatencySigma  float64 `json:"latency_sigma"`
		SlowFraction  float64 `json:"slow_fraction"`
		SlowFactor    float64 `json:"slow_factor"`
		Tokens        int     `json:"tokens"`
		TokenInterval string  `json:"token_interval"`
		Seed          uint64  `json:"seed"`
	} `json:"fake_provider"`
	Rate                float64 `json:"rate"`
	SteadySeconds       float64 `json:"steady_seconds"`
	StallSeconds        float64 `json:"stall_seconds"`
	RecoverSeconds      float64 `json:"recover_seconds"`
	ClientTimeout       string  `json:"client_timeout"`
	PollIntervalSeconds float64 `json:"poll_interval_seconds"`
	Arms                string  `json:"arms"`
	Command             string  `json:"command"`
}

// Summary is the whole M10 run.
type Summary struct {
	Meta Meta `json:"meta"`
	// Arms has the arms present, in Arms order.
	Arms []*Arm `json:"arms"`
	// Sanity is the Q4 hedging-off arm with the timeout; nil if not run.
	Sanity *Sanity `json:"q4_off_ftt,omitempty"`
	// Verdicts are PLAN.md's hypotheses, judged for each arm present.
	Verdicts []Verdict `json:"verdicts"`
}

// Arm is one arm: a fresh gateway and fake provider through the three
// phases. Offsets are seconds from the bench's started_at (its first
// scheduled request).
type Arm struct {
	Name          string         `json:"arm"`
	GatewayCommit string         `json:"gateway_commit"`
	Config        string         `json:"config"`
	StartedAt     time.Time      `json:"started_at"`
	Hardware      bench.Hardware `json:"hardware"`
	// DurationSeconds is the bench's scheduled duration (no warm-up).
	DurationSeconds float64 `json:"duration_seconds"`
	// FirstTokenTimeoutSeconds is the gateway config's
	// first_token_timeout; nil when it is not set.
	FirstTokenTimeoutSeconds *float64 `json:"first_token_timeout_seconds"`
	// StallOnSeconds and StallOffSeconds are the fake provider's first
	// stall-on transition and the first stall-off after it.
	StallOnSeconds  float64        `json:"stall_on_seconds"`
	StallOffSeconds float64        `json:"stall_off_seconds"`
	Requests        int            `json:"requests"`
	Outcomes        map[string]int `json:"outcomes"`
	Phases          []Phase        `json:"phases"`
	// StalledOK is the requests scheduled in the stalled phase that
	// completed; ClientTimeouts the requests (any phase) the client gave
	// up on.
	StalledOK      int      `json:"stalled_ok"`
	ClientTimeouts int      `json:"client_timeouts"`
	Breaker        Breaker  `json:"breaker"`
	Refusals       Refusals `json:"refusals"`
	Metrics        Metrics  `json:"metrics"`
	Ledger         Ledger   `json:"ledger"`
}

// Phase is one phase of an arm. Requests are assigned by their scheduled
// time, ledger entries by their arrival at the provider; the recovered
// phase also takes anything after the bench's end (late retries).
type Phase struct {
	Name        string  `json:"name"`
	FromSeconds float64 `json:"from_seconds"`
	ToSeconds   float64 `json:"to_seconds"`
	Requests    int     `json:"requests"`
	OK          int     `json:"ok"`
	// Outcomes are the bench's outcomes (ok, http_503, timeout, ...).
	Outcomes map[string]int `json:"outcomes"`
	// OKLatency and OKTTFT are the completed requests' total latency and
	// time to first token; Failures the latency of every other request
	// that has one (scheduled time to the error).
	OKLatency bench.Stats `json:"ok_latency_ms"`
	OKTTFT    bench.Stats `json:"ok_ttft_ms"`
	Failures  bench.Stats `json:"failure_latency_ms"`
	// Upstream is the fake provider's ledger entries that arrived in the
	// phase; UpstreamStalled those it held unanswered.
	Upstream        int `json:"upstream"`
	UpstreamStalled int `json:"upstream_stalled"`
}

// Transition is one change of the breaker's state.
type Transition struct {
	AtSeconds float64 `json:"at_s"`
	State     string  `json:"state"`
}

// Breaker is what the breaker did in an arm.
type Breaker struct {
	// Source is where Transitions come from: the gateway's warn lines
	// ("log") when it wrote any, else the /providers polls ("poll"),
	// where a transition is stamped at the first poll that saw the new
	// state, so it lags the change by up to a poll gap (PollGaps).
	Source          string       `json:"source"`
	Transitions     []Transition `json:"transitions"`
	LogTransitions  []Transition `json:"log_transitions"`
	PollTransitions []Transition `json:"poll_transitions"`
	Polls           int          `json:"polls"`
	PollGaps        bench.Stats  `json:"poll_gap_ms"`
	// Opens counts transitions to open over the run; OpensInStall those
	// in the stalled phase.
	Opens        int `json:"opens"`
	OpensInStall int `json:"opens_in_stall"`
	// FirstOpenSeconds is the first open at or after the stall's start;
	// TimeToOpenSeconds the same from the stall's start.
	FirstOpenSeconds  *float64 `json:"first_open_s"`
	TimeToOpenSeconds *float64 `json:"time_to_open_s"`
	// The fifth-failure prediction: the fifth stalled ledger entry's
	// arrival plus the first-token timeout, and the first open minus it.
	FifthStalledArrivalSeconds *float64 `json:"fifth_stalled_arrival_s"`
	PredictedOpenSeconds       *float64 `json:"predicted_open_s"`
	PredictionErrorSeconds     *float64 `json:"prediction_error_s"`
	// TimeToCloseSeconds runs from the stall's end to the first
	// transition to closed at or after it; 0 if the breaker was closed
	// at the stall's end, nil if it never opened or never closed.
	TimeToCloseSeconds *float64 `json:"time_to_close_s"`
	// LastProbeSeconds is the scheduled time of the last probe (see
	// Refusals) at or before the stall's end; LastProbeToStallEndSeconds
	// the stall's end minus it. LastHalfOpenToStallEndSeconds is the same
	// from the last transition to half-open, which the gateway records
	// lazily, at the first State or Allow call after the cooldown.
	LastProbeSeconds *float64 `json:"last_probe_s"`
	// PredictedCloseSeconds is the close that last probe predicts, from
	// the stall's end: the probe is held, fails at the first-token
	// timeout and reopens the breaker for one cooldown, and the next
	// arrival after that probes and succeeds, so the probe's scheduled
	// time + timeout + cooldown + one arrival gap (1/rate) - stall end.
	// It is the pre-stated prediction for a stall that ends just after a
	// probe; nil without a probe or a timeout.
	PredictedCloseSeconds         *float64 `json:"predicted_close_s"`
	LastProbeToStallEndSeconds    *float64 `json:"last_probe_to_stall_end_s"`
	LastHalfOpenToStallEndSeconds *float64 `json:"last_half_open_to_stall_end_s"`
}

// Refusals are the 503 replies to requests scheduled in the stalled phase
// while the breaker was not closed: from a transition to open until the
// next transition to closed, which takes in the half-open stretches.
// Probes are those of them whose latency reached the first-token timeout:
// let through to the provider, they waited the timeout out before their
// retry was refused, where a refusal answers at once. Probes stay in
// Latency; ProbeShare says how many there are.
type Refusals struct {
	Count           int         `json:"count"`
	Latency         bench.Stats `json:"latency_ms"`
	WithRetryAfter  int         `json:"with_retry_after"`
	RetryAfterShare float64     `json:"retry_after_share"`
	Probes          int         `json:"probes"`
	ProbeShare      float64     `json:"probe_share"`
}

// Metrics are the gateway's provider counters at the end of the arm.
type Metrics struct {
	// FirstTokenTimeouts is switchboard_provider_first_token_timeouts_total.
	// A Prometheus counter vector with no label values set exports no
	// series, so an absent series means the timeout never fired and
	// reads as 0; FirstTokenTimeoutsSeries says whether one was exported.
	FirstTokenTimeouts       int            `json:"first_token_timeouts"`
	FirstTokenTimeoutsSeries bool           `json:"first_token_timeouts_series"`
	Retries                  map[string]int `json:"retries"`
	BreakerState             *float64       `json:"breaker_state"`
}

// Ledger is the fake provider's ledger over the whole arm.
type Ledger struct {
	Entries   int `json:"entries"`
	Stalled   int `json:"stalled"`
	Completed int `json:"completed"`
}

// Sanity is Q4's hedging-off arm run again with the first-token timeout.
type Sanity struct {
	Commit         string         `json:"commit"`
	Date           string         `json:"date"`
	Command        string         `json:"command"`
	Hardware       bench.Hardware `json:"hardware"`
	Requests       int            `json:"requests"`
	Errors         int            `json:"errors"`
	TimeoutSeconds float64        `json:"timeout_seconds"`
	// FirstTokenTimeouts reads an absent series as 0 (see Metrics).
	FirstTokenTimeouts       int  `json:"first_token_timeouts"`
	FirstTokenTimeoutsSeries bool `json:"first_token_timeouts_series"`
	// Sent is the requests the bench sent, warm-up included; Upstream the
	// requests the fake provider's ledger received. A timeout that fired
	// is retried, so Held needs them equal as well as no timeouts counted.
	Sent     int `json:"sent"`
	Upstream int `json:"upstream"`
	// TTFT is the measured requests' time to first token.
	TTFT bench.Stats `json:"ttft_ms"`
	Held bool        `json:"held"`
}

// Comparison operators in a Verdict.
const (
	OpEq    = "=="
	OpLE    = "<="
	OpLT    = "<"
	OpAbsLE = "|x| <="
)

// Verdict is one hypothesis against its bound.
type Verdict struct {
	Arm        string `json:"arm"`
	Check      string `json:"check"`
	Hypothesis string `json:"hypothesis"`
	// Measured is nil when the run has nothing to measure (the breaker
	// never opened, no refusals); the verdict then fails.
	Measured *float64 `json:"measured"`
	Op       string   `json:"op"`
	Bound    float64  `json:"bound"`
	Unit     string   `json:"unit"`
	Held     bool     `json:"held"`
}

// Verdict checks.
const (
	CheckOpens       = "opens"
	CheckStalledOK   = "stalled_ok"
	CheckOpenWithin  = "open_within"
	CheckPrediction  = "open_prediction"
	CheckRefusalP99  = "refusal_p99"
	CheckRetryAfter  = "retry_after"
	CheckCloseWithin = "close_within"
	CheckNoTimeouts  = "no_timeouts"
)

// Analyse reads the run under dir: its meta.json, each arm present and
// the sanity check if present.
func Analyse(dir string) (*Summary, error) {
	s := &Summary{}
	if err := readJSON(filepath.Join(dir, "meta.json"), &s.Meta); err != nil {
		return nil, err
	}
	for _, name := range Arms {
		a, err := analyseArm(filepath.Join(dir, name), name)
		if err != nil {
			return nil, err
		}
		if a != nil {
			s.Arms = append(s.Arms, a)
			s.Verdicts = append(s.Verdicts, armVerdicts(a)...)
		}
	}
	san, err := analyseSanity(filepath.Join(dir, SanityDir))
	if err != nil {
		return nil, err
	}
	if san != nil {
		s.Sanity = san
		s.Verdicts = append(s.Verdicts, Verdict{
			Arm: SanityArm, Check: CheckNoTimeouts,
			Hypothesis: "first-token timeouts on Q4's hedging-off arm",
			Measured:   ptr(float64(san.FirstTokenTimeouts)), Op: OpEq, Bound: 0, Unit: "",
			Held: san.Held,
		})
	}
	return s, nil
}

// Arm returns the named arm, nil if absent.
func (s *Summary) Arm(name string) *Arm {
	for _, a := range s.Arms {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func analyseArm(dir, name string) (*Arm, error) {
	var res bench.Result
	if err := readJSON(filepath.Join(dir, "bench.json"), &res); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	a := &Arm{Name: name, StartedAt: res.StartedAt, Hardware: res.Hardware,
		DurationSeconds: res.Config.WarmupSec + res.Config.DurationSec}
	var meta struct {
		GatewayCommit string `json:"gateway_commit"`
		Config        string `json:"config"`
	}
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return nil, err
	}
	a.GatewayCommit, a.Config = meta.GatewayCommit, meta.Config
	ftt, err := readFirstTokenTimeout(filepath.Join(dir, "gateway.yaml"))
	if err != nil {
		return nil, err
	}
	if ftt > 0 {
		a.FirstTokenTimeoutSeconds = ptr(ftt.Seconds())
	}
	start := res.StartedAt
	offset := func(t time.Time) float64 { return t.Sub(start).Seconds() }

	on, off, err := readStall(filepath.Join(dir, "stall.json"))
	if err != nil {
		return nil, err
	}
	a.StallOnSeconds, a.StallOffSeconds = round6(offset(on)), round6(offset(off))

	recs, err := readRecords(filepath.Join(dir, "requests.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	ledger, err := readLedger(filepath.Join(dir, "ledger.json.gz"))
	if err != nil {
		return nil, err
	}
	a.Phases = phases(a, recs, ledger, offset)
	a.Requests = len(recs)
	a.Outcomes = map[string]int{}
	for _, r := range recs {
		a.Outcomes[r.Outcome]++
		if r.Outcome == bench.OutcomeTimeout {
			a.ClientTimeouts++
		}
	}
	a.StalledOK = a.Phases[1].OK
	for _, e := range ledger {
		a.Ledger.Entries++
		if e.Stalled {
			a.Ledger.Stalled++
		}
		if e.Completed {
			a.Ledger.Completed++
		}
	}

	logT, err := readLogTransitions(filepath.Join(dir, "gateway.log.gz"), offset)
	if err != nil {
		return nil, err
	}
	pollT, gaps, polls, err := readPollTransitions(filepath.Join(dir, "breaker.jsonl.gz"), start)
	if err != nil {
		return nil, err
	}
	b := &a.Breaker
	b.LogTransitions, b.PollTransitions, b.Polls, b.PollGaps = logT, pollT, polls, gaps
	b.Source, b.Transitions = SourcePoll, pollT
	if len(logT) > 0 {
		b.Source, b.Transitions = SourceLog, logT
	}
	analyseBreaker(a, ledger, offset)
	a.Refusals = refusals(a, recs)
	if a.Refusals.Probes > 0 {
		var last float64
		for _, r := range probes(a, recs) {
			last = max(last, r.At)
		}
		b.LastProbeSeconds = sec(last)
		b.LastProbeToStallEndSeconds = sec(a.StallOffSeconds - last)
		if res.Config.Rate > 0 {
			b.PredictedCloseSeconds = sec(last + *a.FirstTokenTimeoutSeconds + BreakerCooldownSeconds + 1/res.Config.Rate - a.StallOffSeconds)
		}
	}

	if a.Metrics, err = readMetrics(filepath.Join(dir, "metrics.prom")); err != nil {
		return nil, err
	}
	return a, nil
}

// phases splits records and ledger entries into the three phases.
func phases(a *Arm, recs []bench.Record, ledger []fake.Entry, offset func(time.Time) float64) []Phase {
	ps := []Phase{
		{Name: PhaseSteady, FromSeconds: 0, ToSeconds: a.StallOnSeconds},
		{Name: PhaseStalled, FromSeconds: a.StallOnSeconds, ToSeconds: a.StallOffSeconds},
		{Name: PhaseRecovered, FromSeconds: a.StallOffSeconds, ToSeconds: a.DurationSeconds},
	}
	type acc struct{ okLat, okTTFT, fail []time.Duration }
	accs := make([]acc, len(ps))
	for i := range ps {
		ps[i].Outcomes = map[string]int{}
	}
	for _, r := range recs {
		i := a.phaseOf(r.At)
		p := &ps[i]
		p.Requests++
		p.Outcomes[r.Outcome]++
		if r.Outcome == bench.OutcomeOK {
			p.OK++
			if r.LatencyMS != nil {
				accs[i].okLat = append(accs[i].okLat, msDur(*r.LatencyMS))
			}
			if r.TTFTMS != nil {
				accs[i].okTTFT = append(accs[i].okTTFT, msDur(*r.TTFTMS))
			}
		} else if r.LatencyMS != nil {
			accs[i].fail = append(accs[i].fail, msDur(*r.LatencyMS))
		}
	}
	for i := range ps {
		ps[i].OKLatency = bench.Summarise(accs[i].okLat)
		ps[i].OKTTFT = bench.Summarise(accs[i].okTTFT)
		ps[i].Failures = bench.Summarise(accs[i].fail)
	}
	for _, e := range ledger {
		p := &ps[a.phaseOf(offset(e.Time))]
		p.Upstream++
		if e.Stalled {
			p.UpstreamStalled++
		}
	}
	return ps
}

// phaseOf is the index of the phase containing offset t: steady before
// the stall's start, stalled up to its end, recovered from then on.
func (a *Arm) phaseOf(t float64) int {
	switch {
	case t < a.StallOnSeconds:
		return 0
	case t < a.StallOffSeconds:
		return 1
	default:
		return 2
	}
}

func analyseBreaker(a *Arm, ledger []fake.Entry, offset func(time.Time) float64) {
	b := &a.Breaker
	var lastHalfOpen *float64
	for _, t := range b.Transitions {
		switch t.State {
		case StateOpen:
			b.Opens++
			if a.phaseOf(t.AtSeconds) == 1 {
				b.OpensInStall++
			}
			if b.FirstOpenSeconds == nil && t.AtSeconds >= a.StallOnSeconds {
				b.FirstOpenSeconds = sec(t.AtSeconds)
				b.TimeToOpenSeconds = sec(t.AtSeconds - a.StallOnSeconds)
			}
		case StateHalfOpen:
			if t.AtSeconds <= a.StallOffSeconds {
				lastHalfOpen = sec(t.AtSeconds)
			}
		}
	}
	if lastHalfOpen != nil {
		b.LastHalfOpenToStallEndSeconds = sec(a.StallOffSeconds - *lastHalfOpen)
	}
	if b.Opens > 0 {
		if stateAt(b.Transitions, a.StallOffSeconds) == StateClosed {
			b.TimeToCloseSeconds = ptr(0.0)
		} else {
			for _, t := range b.Transitions {
				if t.State == StateClosed && t.AtSeconds >= a.StallOffSeconds {
					b.TimeToCloseSeconds = sec(t.AtSeconds - a.StallOffSeconds)
					break
				}
			}
		}
	}
	var stalled []float64
	for _, e := range ledger {
		if e.Stalled {
			stalled = append(stalled, round6(offset(e.Time)))
		}
	}
	slices.Sort(stalled)
	if len(stalled) >= BreakerFailures {
		b.FifthStalledArrivalSeconds = sec(stalled[BreakerFailures-1])
		if a.FirstTokenTimeoutSeconds != nil {
			pred := stalled[BreakerFailures-1] + *a.FirstTokenTimeoutSeconds
			b.PredictedOpenSeconds = sec(pred)
			if b.FirstOpenSeconds != nil {
				b.PredictionErrorSeconds = sec(*b.FirstOpenSeconds - pred)
			}
		}
	}
}

// stateAt is the breaker's state just after offset t (transitions at t
// included); closed before the first transition.
func stateAt(ts []Transition, t float64) string {
	st := StateClosed
	for _, tr := range ts {
		if tr.AtSeconds > t {
			break
		}
		st = tr.State
	}
	return st
}

// notClosed reports whether offset t falls between a transition to open
// and the next transition to closed.
func notClosed(ts []Transition, t float64) bool { return stateAt(ts, t) != StateClosed }

// refused is the refusal set: 503s scheduled in the stalled phase while
// the breaker was not closed.
func refused(a *Arm, recs []bench.Record) []bench.Record {
	var out []bench.Record
	for _, r := range recs {
		if r.Status == 503 && a.phaseOf(r.At) == 1 && notClosed(a.Breaker.Transitions, r.At) {
			out = append(out, r)
		}
	}
	return out
}

// probes are the refusals whose latency reached the first-token timeout.
func probes(a *Arm, recs []bench.Record) []bench.Record {
	if a.FirstTokenTimeoutSeconds == nil {
		return nil
	}
	var out []bench.Record
	for _, r := range refused(a, recs) {
		if r.LatencyMS != nil && *r.LatencyMS >= *a.FirstTokenTimeoutSeconds*1000 {
			out = append(out, r)
		}
	}
	return out
}

func refusals(a *Arm, recs []bench.Record) Refusals {
	var out Refusals
	var lat []time.Duration
	for _, r := range refused(a, recs) {
		out.Count++
		if r.RetryAfter != "" {
			out.WithRetryAfter++
		}
		if r.LatencyMS != nil {
			lat = append(lat, msDur(*r.LatencyMS))
		}
	}
	out.Latency = bench.Summarise(lat)
	out.Probes = len(probes(a, recs))
	if out.Count > 0 {
		out.RetryAfterShare = float64(out.WithRetryAfter) / float64(out.Count)
		out.ProbeShare = float64(out.Probes) / float64(out.Count)
	}
	return out
}

func armVerdicts(a *Arm) []Verdict {
	b := a.Breaker
	switch a.Name {
	case "before":
		return []Verdict{
			{Arm: a.Name, Check: CheckOpens, Hypothesis: "breaker opens in the run",
				Measured: ptr(float64(b.Opens)), Op: OpEq, Bound: 0, Held: b.Opens == 0},
			{Arm: a.Name, Check: CheckStalledOK, Hypothesis: "requests scheduled in the stall that complete",
				Measured: ptr(float64(a.StalledOK)), Op: OpEq, Bound: 0, Held: a.StalledOK == 0},
		}
	case "after":
		vs := []Verdict{
			judge(a.Name, CheckOpenWithin, "time from the stall's start to the first open",
				b.TimeToOpenSeconds, OpLE, OpenWithinSeconds, "s"),
			judge(a.Name, CheckPrediction, "first open minus (fifth stalled arrival + timeout)",
				b.PredictionErrorSeconds, OpAbsLE, PredictionToleranceSeconds, "s"),
		}
		var p99, share *float64
		if a.Refusals.Count > 0 {
			p99 = ptr(a.Refusals.Latency.P99)
			share = ptr(a.Refusals.RetryAfterShare * 100)
		}
		vs = append(vs,
			judge(a.Name, CheckRefusalP99, "refusals' p99 latency", p99, OpLT, RefusalP99MS, "ms"),
			judge(a.Name, CheckRetryAfter, "refusals with Retry-After", share, OpEq, RetryAfterPercent, "%"),
			judge(a.Name, CheckCloseWithin, "time from the stall's end to the breaker closing",
				b.TimeToCloseSeconds, OpLE, CloseWithinSeconds, "s"),
		)
		return vs
	}
	return nil
}

func judge(arm, check, hyp string, v *float64, op string, bound float64, unit string) Verdict {
	out := Verdict{Arm: arm, Check: check, Hypothesis: hyp, Measured: v, Op: op, Bound: bound, Unit: unit}
	if v == nil {
		return out
	}
	switch op {
	case OpEq:
		out.Held = *v == bound
	case OpLE:
		out.Held = *v <= bound
	case OpLT:
		out.Held = *v < bound
	case OpAbsLE:
		out.Held = math.Abs(*v) <= bound
	}
	return out
}

func analyseSanity(dir string) (*Sanity, error) {
	var res bench.Result
	if err := readJSON(filepath.Join(dir, SanityArm+".json"), &res); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var meta struct {
		Commit  string `json:"commit"`
		Date    string `json:"date"`
		Command string `json:"command"`
	}
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return nil, err
	}
	m, err := readMetrics(filepath.Join(dir, SanityArm+"-metrics.prom"))
	if err != nil {
		return nil, err
	}
	// The counter alone cannot fail the check: a scrape that failed leaves
	// an empty file, which reads like a counter that never fired. The fake
	// provider's ledger can: every timeout that fired was retried, so the
	// provider would have seen more requests than the bench sent.
	var ledger struct {
		Requests int `json:"requests"`
	}
	if err := readJSON(filepath.Join(dir, SanityArm+"-ledger.json"), &ledger); err != nil {
		return nil, err
	}
	sent := res.Requests + res.WarmupRequests
	return &Sanity{
		Commit: meta.Commit, Date: meta.Date, Command: meta.Command, Hardware: res.Hardware,
		Requests: res.Requests, Errors: res.Errors(), TimeoutSeconds: SanityTimeout.Seconds(),
		FirstTokenTimeouts: m.FirstTokenTimeouts, FirstTokenTimeoutsSeries: m.FirstTokenTimeoutsSeries,
		Sent: sent, Upstream: ledger.Requests,
		TTFT: res.TTFT, Held: m.FirstTokenTimeouts == 0 && ledger.Requests == sent && res.Errors() == 0,
	}, nil
}

func ptr[T any](v T) *T { return &v }

// sec is a number of seconds rounded to the microsecond, the resolution
// of the bench's offsets.
func sec(v float64) *float64 { return ptr(round6(v)) }

// msDur converts a logged millisecond value (rounded to the microsecond)
// back to a duration for bench.Summarise.
func msDur(ms float64) time.Duration {
	return time.Duration(math.Round(ms * float64(time.Millisecond)))
}

// errNoStall is returned when stall.json lacks an on or off transition.
var errNoStall = errors.New("stalleval: stall.json has no stall on followed by a stall off")

func wrap(path string, err error) error { return fmt.Errorf("stalleval: %s: %w", path, err) }

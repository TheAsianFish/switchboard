package results

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/chaos"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// Q6Dir is the subdirectory written by chaos/run.sh.
const Q6Dir = "q6-chaos"

// Q6 is the chaos section, rendered from summary.json.
type Q6 struct {
	Meta     chaos.Meta
	Hardware string
	Scaled   bool
	// Rule is the steady-state rule in words.
	Rule string
	// Reference values, formatted.
	RefErrorRate, RefP99, ErrorLimit, P99Limit string
	Streams                                    []Q6Stream
	Phases                                     []Q6Phase
	Events                                     []Q6Event
	Checks                                     []Q6Check
	// Budget and Grace are the budgeted key's allowance and per-replica
	// grace, formatted; LimitRate the limited key's rate per window.
	Budget, Grace, LimitRate string
	MaxSendDelay             string
	// FakeLatency and FakeTokenInterval are the fake provider's settings
	// for prose, e.g. "20 ms".
	FakeLatency, FakeTokenInterval string
}

// Q6Stream is one load stream.
type Q6Stream struct {
	Name, Model, Rate, Mode, Requests string
}

// Q6Phase is one row of the phase tables; every value is formatted.
type Q6Phase struct {
	Name, From, To, Replicas, Redis string
	// Main stream.
	Requests, ErrorRate, Errors, P50, P99, TTFTP99, Slow, HitRate, RepeatHitRate string
	// Limited key.
	LimOffered, LimRefused, LimAdmitted, MostInWindow, LocalBound, Over string
	// Budgeted key.
	BudOffered, BudRefused, BudAdmitted, Spent, Cumulative, Overshoot, GraceBound string
}

// Q6Event is one docker action and the recovery after it.
type Q6Event struct {
	Name, At, Took, Recovery, Window, Errors, Worst string
}

// Q6Check is a hypothesis from PLAN.md set against the run.
type Q6Check struct {
	Hypothesis, Observed, Held string
}

func loadQ6(dir string) (*Q6, error) {
	path := filepath.Join(dir, chaos.SummaryFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	var s chaos.Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("results: %s: %w", path, err)
	}
	if len(s.Phases) == 0 {
		return nil, fmt.Errorf("results: %s has no phases", path)
	}
	return buildQ6(&s)
}

func buildQ6(s *chaos.Summary) (*Q6, error) {
	budget, err := cost.ParseUSD(s.Meta.Budget.USD)
	if err != nil {
		return nil, fmt.Errorf("results: q6 budget: %w", err)
	}
	grace, err := cost.ParseUSD(s.Meta.Budget.GraceUSD)
	if err != nil {
		return nil, fmt.Errorf("results: q6 grace: %w", err)
	}
	r := s.Rule
	q := &Q6{
		Meta:     s.Meta,
		Hardware: describeHardware(s.Hardware),
		Scaled:   s.Meta.TimeScale != 1,
		Rule: fmt.Sprintf("the fleet is steady from the start of the first whole %s s bucket after the event from which %d consecutive buckets "+
			"each have an error rate at most max(%s, %s × the steady phase's) and a p99 latency at most %s × the steady phase's, "+
			"all before the next event or the end of the run; otherwise \"not recovered\"",
			num(r.BucketSeconds), r.Consecutive, q6Pct(r.MinErrorRate), num(r.ErrorFactor), num(r.P99Factor)),
		RefErrorRate: q6Pct(s.Reference.ErrorRate),
		RefP99:       msf(s.Reference.P99MS),
		ErrorLimit:   q6Pct(s.Reference.ErrorLimit),
		P99Limit:     msf(s.Reference.P99LimitMS),
		Budget:       q6USD(budget),
		Grace:        q6USD(grace),
		LimitRate:    fmt.Sprintf("%d per %s s", s.Meta.Limit.Rate, num(s.Meta.Limit.PeriodSeconds)),

		FakeLatency:       prose(s.Meta.FakeProvider.Latency),
		FakeTokenInterval: prose(s.Meta.FakeProvider.TokenInterval),
	}
	var maxDelay float64
	for _, st := range s.Streams {
		mode := "non-streaming"
		if st.Stream {
			mode = "streaming"
		}
		if st.Repeat > 0 {
			mode += fmt.Sprintf(", %s repeats of %d prompts", q6Pct(st.Repeat), st.Pool)
		}
		q.Streams = append(q.Streams, Q6Stream{Name: st.Name, Model: st.Model, Rate: num(st.Rate), Mode: mode, Requests: fmt.Sprint(st.Requests)})
		maxDelay = max(maxDelay, st.MaxSendDelayMS)
	}
	q.MaxSendDelay = msf(maxDelay)
	for _, p := range s.Phases {
		redis := "up"
		if !p.RedisUp {
			redis = "down"
		}
		m, l, b := p.Main, p.Limited, p.Budget
		q.Phases = append(q.Phases, Q6Phase{
			Name: p.Name, From: fmt.Sprintf("%.1f", p.FromSeconds), To: fmt.Sprintf("%.1f", p.ToSeconds),
			Replicas: fmt.Sprintf("%d of %d", p.ReplicasUp, s.Meta.Replicas), Redis: redis,
			Requests: fmt.Sprint(m.Requests), ErrorRate: q6Pct2(m.ErrorRate), Errors: q6Kinds(m.ErrorsByKind),
			P50: msf(m.Latency.P50), P99: msf(m.Latency.P99), TTFTP99: msf(m.TTFT.P99), Slow: fmt.Sprint(m.Slow),
			HitRate: q6Pct(m.CacheHitRate), RepeatHitRate: q6Pct(m.RepeatHitRate),
			LimOffered: fmt.Sprint(l.Offered), LimRefused: fmt.Sprint(l.Refused), LimAdmitted: fmt.Sprint(l.Admitted),
			MostInWindow: fmt.Sprint(l.MostInWindow), LocalBound: fmt.Sprint(l.LocalBound), Over: fmt.Sprint(l.OverAdmissions),
			BudOffered: fmt.Sprint(b.Offered), BudRefused: fmt.Sprint(b.Refused), BudAdmitted: fmt.Sprint(b.Admitted),
			Spent: q6USD(b.Spent), Cumulative: q6USD(b.Cumulative), Overshoot: q6USD(b.Overshoot), GraceBound: q6Dash(b.GraceBound),
		})
	}
	for _, e := range s.Events {
		ev := Q6Event{
			Name: e.Name, At: fmt.Sprintf("%.2f", e.AtSeconds), Took: fmt.Sprintf("%.2f", e.DoneSeconds-e.AtSeconds),
			Recovery: "not recovered", Window: fmt.Sprintf("%.1f", e.WindowSeconds), Errors: fmt.Sprint(e.ErrorsBeforeRecovery),
		}
		if e.RecoverySeconds != nil {
			ev.Recovery = recoveredAt(s, &e)
		}
		if w := e.WorstBucket; w != nil {
			ev.Worst = fmt.Sprintf("%d errors of %d, p99 %s ms (%s–%s s)", w.Errors, w.Requests, msf(w.P99MS), num(w.StartSeconds), num(w.StartSeconds+s.Rule.BucketSeconds))
		}
		q.Events = append(q.Events, ev)
	}
	q.Checks = q6Checks(s, budget)
	return q, nil
}

// q6Checks sets PLAN.md's hypotheses against the run. Phases are found by
// their state (how many replicas and whether Redis were up), so a run
// with another schedule gets the checks that apply to it.
func q6Checks(s *chaos.Summary, budget cost.Nanos) []Q6Check {
	n := s.Meta.Replicas
	held := func(ok bool) string {
		if ok {
			return "yes"
		}
		return "no"
	}
	event := func(action, target string) *chaos.EventResult {
		for i := range s.Events {
			if s.Events[i].Action == action && (s.Events[i].Target == chaos.RedisTarget) == (target == chaos.RedisTarget) {
				return &s.Events[i]
			}
		}
		return nil
	}
	recovery := func(e *chaos.EventResult) string {
		if e == nil || e.RecoverySeconds == nil {
			return "not recovered"
		}
		return "steady after " + recoveredAt(s, e)
	}
	fast := func(e *chaos.EventResult) bool { return e != nil && e.RecoverySeconds != nil && *e.RecoverySeconds < 1 }
	var out []Q6Check
	phase := func(match func(i int, p chaos.PhaseResult) bool) (int, *chaos.PhaseResult) {
		for i := range s.Phases {
			if match(i, s.Phases[i]) {
				return i, &s.Phases[i]
			}
		}
		return -1, nil
	}
	if _, p := phase(func(_ int, p chaos.PhaseResult) bool { return p.ReplicasUp < n && p.RedisUp }); p != nil {
		e := event(chaos.ActionKill, "replica")
		out = append(out, Q6Check{
			Hypothesis: "Replica kill: errors only from the streams in flight on the killed replica; steady again within a second",
			Observed:   fmt.Sprintf("%d errors in `%s` (%s); %s", p.Main.Errors, p.Name, q6Kinds(p.Main.ErrorsByKind), recovery(e)),
			Held:       held(fast(e)),
		})
	}
	ri, rd := phase(func(_ int, p chaos.PhaseResult) bool { return !p.RedisUp })
	if rd != nil {
		e := event(chaos.ActionKill, chaos.RedisTarget)
		out = append(out, Q6Check{
			Hypothesis: "Redis kill: no errors, and the cache is bypassed (hit rate 0)",
			Observed:   fmt.Sprintf("%d errors, hit rate %s, p99 %s ms against %s ms steady; %s", rd.Main.Errors, q6Pct(rd.Main.CacheHitRate), msf(rd.Main.Latency.P99), msf(s.Reference.P99MS), recovery(e)),
			Held:       held(rd.Main.Errors == 0 && rd.Main.CacheHitRate < 0.01),
		})
		out = append(out, Q6Check{
			Hypothesis: "Redis kill: the limited key falls back to rate ÷ replicas per replica, so over-admissions stay within one window's local share",
			Observed:   fmt.Sprintf("%d over-admissions, at most %d in a window (local bound %d per window)", rd.Limited.OverAdmissions, rd.Limited.MostInWindow, rd.Limited.LocalBound),
			Held:       held(rd.Limited.OverAdmissions <= rd.Limited.LocalBound),
		})
		out = append(out, Q6Check{
			Hypothesis: "Redis kill: the spent budget lets through at most the live replicas' grace",
			Observed:   fmt.Sprintf("%s spent while Redis was down, bound %s", q6USD(rd.Budget.Spent), q6USD(rd.Budget.GraceBound)),
			Held:       held(rd.Budget.Spent <= rd.Budget.GraceBound),
		})
	}
	if ri >= 0 {
		if _, back := phase(func(i int, p chaos.PhaseResult) bool { return i > ri && p.RedisUp }); back != nil {
			e := event(chaos.ActionStart, chaos.RedisTarget)
			steady := s.Phases[0].Main
			out = append(out, Q6Check{
				Hypothesis: "Redis restart: no errors; the empty cache refills, so the hit rate climbs back",
				Observed: fmt.Sprintf("%d errors; hit rate %s (%s of repeats) against %s (%s) steady; %s", back.Main.Errors,
					q6Pct(back.Main.CacheHitRate), q6Pct(back.Main.RepeatHitRate), q6Pct(steady.CacheHitRate), q6Pct(steady.RepeatHitRate), recovery(e)),
				Held: held(back.Main.Errors == 0 && back.Main.CacheHitRate > 0),
			})
		}
		last := s.Phases[len(s.Phases)-1]
		maxGrace := cost.Nanos(0)
		for _, p := range s.Phases {
			maxGrace = max(maxGrace, p.Budget.GraceBound)
		}
		out = append(out, Q6Check{
			Hypothesis: "Redis restart: the budget ledger comes back empty, so the overshoot at the end is about one whole budget",
			Observed:   fmt.Sprintf("overshoot %s at the end against a budget of %s", q6USD(last.Budget.Overshoot), q6USD(budget)),
			Held:       held(math.Abs(float64(last.Budget.Overshoot-budget)) <= float64(maxGrace)),
		})
	}
	if len(s.Phases) > 1 {
		last := s.Phases[len(s.Phases)-1]
		if last.ReplicasUp == n && last.RedisUp {
			e := event(chaos.ActionStart, "replica")
			out = append(out, Q6Check{
				Hypothesis: "Replica restart: no errors; the replica takes its share again within a second",
				Observed:   fmt.Sprintf("%d errors in `%s`; %s", last.Main.Errors, last.Name, recovery(e)),
				Held:       held(last.Main.Errors == 0 && fast(e)),
			})
		}
	}
	return out
}

// recoveredAt formats a recovery time with the bucket it ends at, since
// the rule's resolution is one bucket: "0.996 s (from 31 s)".
func recoveredAt(s *chaos.Summary, e *chaos.EventResult) string {
	return fmt.Sprintf("%.3f s (from %s s)", *e.RecoverySeconds, num(math.Round((e.AtSeconds+*e.RecoverySeconds)*1000)/1000))
}

// q6Kinds formats error counts by kind, most first, e.g. "http_502 3,
// malformed_stream 2"; "none" if there are none.
func q6Kinds(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.SortFunc(ks, func(a, b string) int {
		if m[a] != m[b] {
			return m[b] - m[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func q6Pct(v float64) string { return fmt.Sprintf("%.1f%%", 100*v) }

func q6Pct2(v float64) string { return fmt.Sprintf("%.2f%%", 100*v) }

// q6USD formats an amount to the hundredth of a cent.
func q6USD(n cost.Nanos) string { return fmt.Sprintf("$%.4f", n.USD()) }

func q6Dash(n cost.Nanos) string {
	if n == 0 {
		return "–"
	}
	return q6USD(n)
}

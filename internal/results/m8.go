package results

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/TheAsianFish/switchboard/internal/chaos"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// M8Dir holds the flap runs (`make chaos-flap RUN=before|after`) and the
// Q6 re-run made after the fix, one subdirectory each.
const M8Dir = "m8-flap"

// The M8 runs' subdirectories.
const (
	M8Before  = "before"
	M8After   = "after"
	M8Q6Rerun = "q6-rerun"
)

// M8 is the flapping-store section: the same schedule before and after
// grace became a time-refilled allowance, side by side.
type M8 struct {
	Before, After *M8Run
	// Budget, Grace and Refill are formatted from the after run's meta.
	Budget, Grace, Refill string
	Outages               []M8Outage
	Totals                []M8Total
	Phases                []M8Phase
	Checks                []Q6Check
	// Rerun compares `make chaos` after the fix with the committed Q6.
	Rerun []M8Total
	// RerunMeta is the Q6 re-run's meta; nil without a re-run.
	RerunMeta *chaos.Meta
	Q6Commit  string
	// Headline numbers for prose and the README, "–" where a run is
	// missing: overshoot at the end, the after run's refill bound, the
	// killed replica's loss, the per-outage slope and the restarted
	// replica's spend in its first outage after the fix.
	OvershootBefore, OvershootAfter, RefillBound string
	LostBefore, LostAfter, SlopeBefore, R2Before string
	FirstAfter, RestartedAfter, ProcessesAfter   string
	T                                            string
}

// M8Run is one flap run.
type M8Run struct {
	Meta     chaos.Meta
	Hardware string
	Summary  *chaos.Summary
}

// M8Outage is one outage in both runs.
type M8Outage struct {
	N, Window, Replicas                                    string
	BeforeRequests, BeforeSpent, BeforeOver, BeforeExhaust string
	AfterRequests, AfterSpent, AfterOver, AfterExhaust     string
}

// M8Total is one named number in both runs (or, for Rerun, committed and
// re-run).
type M8Total struct {
	Name, Before, After string
}

// M8Phase is one phase's reconciliation and main-stream numbers in both
// runs.
type M8Phase struct {
	Name, Redis, Replicas string
	Before, After         M8PhaseRun
}

// M8PhaseRun is a phase in one run.
type M8PhaseRun struct {
	ErrorRate, P99, Ledger, Redis, Gap, Reserved string
}

// loadM8 reads the flap runs under dir and, for the re-run comparison,
// the committed Q6 run under q6dir.
func loadM8(dir, q6dir string) (*M8, error) {
	before, err := loadM8Run(filepath.Join(dir, M8Before))
	if err != nil {
		return nil, err
	}
	after, err := loadM8Run(filepath.Join(dir, M8After))
	if err != nil {
		return nil, err
	}
	if before == nil && after == nil {
		return nil, nil
	}
	rerun, err := loadM8Run(filepath.Join(dir, M8Q6Rerun))
	if err != nil {
		return nil, err
	}
	m, err := buildM8(before, after)
	if err != nil || rerun == nil {
		return m, err
	}
	committed, err := loadM8Run(q6dir)
	if err != nil {
		return nil, err
	}
	if committed != nil {
		m.RerunMeta, m.Q6Commit = &rerun.Meta, committed.Meta.Commit
		m8Rerun(m, committed.Summary, rerun.Summary)
	}
	return m, nil
}

func loadM8Run(dir string) (*M8Run, error) {
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
	return &M8Run{Meta: s.Meta, Hardware: describeHardware(s.Hardware), Summary: &s}, nil
}

// buildM8 needs at least one flap run; a missing one renders as dashes.
func buildM8(before, after *M8Run) (*M8, error) {
	ref := after
	if ref == nil {
		ref = before
	}
	for _, r := range []*M8Run{before, after} {
		if r != nil && r.Summary.Flap == nil {
			return nil, fmt.Errorf("results: m8: run %s has no outages", r.Meta.Date)
		}
	}
	grace, err := cost.ParseUSD(ref.Meta.Budget.GraceUSD)
	if err != nil {
		return nil, fmt.Errorf("results: m8 grace: %w", err)
	}
	budget, err := cost.ParseUSD(ref.Meta.Budget.USD)
	if err != nil {
		return nil, fmt.Errorf("results: m8 budget: %w", err)
	}
	m := &M8{Before: before, After: after, Budget: q6USD(budget), Grace: q6USD(grace), Refill: "–"}
	if after != nil && after.Meta.Budget.GraceRefill != "" {
		m.Refill = after.Meta.Budget.GraceRefill
		if d, err := time.ParseDuration(m.Refill); err == nil {
			switch {
			case d%time.Hour == 0:
				m.Refill = fmt.Sprintf("%d h", d/time.Hour)
			case d%time.Minute == 0:
				m.Refill = fmt.Sprintf("%d min", d/time.Minute)
			default:
				m.Refill = prose(m.Refill)
			}
		}
	}
	flap := func(r *M8Run) *chaos.FlapResult {
		if r == nil {
			return nil
		}
		return r.Summary.Flap
	}
	fb, fa := flap(before), flap(after)
	n := 0
	for _, f := range []*chaos.FlapResult{fb, fa} {
		if f != nil {
			n = max(n, len(f.Outages))
		}
	}
	for i := range n {
		row := M8Outage{N: fmt.Sprint(i + 1), Window: "–", Replicas: "–",
			BeforeRequests: "–", BeforeSpent: "–", BeforeOver: "–", BeforeExhaust: "–",
			AfterRequests: "–", AfterSpent: "–", AfterOver: "–", AfterExhaust: "–"}
		for k, f := range []*chaos.FlapResult{fb, fa} {
			if f == nil || i >= len(f.Outages) {
				continue
			}
			o := f.Outages[i]
			row.Window = fmt.Sprintf("%.1f–%.1f", o.FromSeconds, o.ToSeconds)
			row.Replicas = fmt.Sprint(o.ReplicasUp)
			if o.ReplicaKilled {
				row.Replicas += ", one killed"
			}
			req, sp, ov, ex := fmt.Sprint(o.Requests), q6USD(o.Spent), q6USD(o.OvershootAfter), fmt.Sprintf("%.1f", o.SecondsToExhaust)
			if o.Requests == 0 {
				ex = "–"
			}
			if k == 0 {
				row.BeforeRequests, row.BeforeSpent, row.BeforeOver, row.BeforeExhaust = req, sp, ov, ex
			} else {
				row.AfterRequests, row.AfterSpent, row.AfterOver, row.AfterExhaust = req, sp, ov, ex
			}
		}
		m.Outages = append(m.Outages, row)
	}
	both := func(name string, get func(f *chaos.FlapResult) string) M8Total {
		t := M8Total{Name: name, Before: "–", After: "–"}
		if fb != nil {
			t.Before = get(fb)
		}
		if fa != nil {
			t.After = get(fa)
		}
		return t
	}
	m.Totals = []M8Total{
		both("Overshoot at the end", func(f *chaos.FlapResult) string { return q6USD(f.Overshoot) }),
		both("Spent during the outages", func(f *chaos.FlapResult) string { return q6USD(f.OutageSpend) }),
		both("Overshoot added per outage (least-squares slope)", func(f *chaos.FlapResult) string {
			return fmt.Sprintf("%s (R² %.3f)", q6USD(f.Slope), f.R2)
		}),
		both("T, first disconnect to last connect", func(f *chaos.FlapResult) string { return fmt.Sprintf("%.1f s", f.TSeconds) }),
		both("Replica processes in T", func(f *chaos.FlapResult) string { return fmt.Sprint(f.Processes) }),
		both("Bound if grace resets every outage", func(f *chaos.FlapResult) string { return q6USD(f.PerOutageBound) }),
		both("Bound processes × grace × (1 + T / refill)", func(f *chaos.FlapResult) string {
			if f.RefillBound == 0 {
				return "– (no refill)"
			}
			return q6USD(f.RefillBound)
		}),
		both("Killed replica's spend Redis never saw (bound: one grace)", func(f *chaos.FlapResult) string {
			if f.Kill == nil {
				return "no kill"
			}
			return fmt.Sprintf("%s (estimated pending %s)", m8USD(f.Kill.Lost), m8USD(f.Kill.EstimatedPending))
		}),
		both("Largest gap in a reachable phase before the kill", func(f *chaos.FlapResult) string { return m8USD(f.MaxControlGap) }),
		both("Most negative gap after the first outage", func(f *chaos.FlapResult) string { return m8USD(f.MinGap) }),
	}

	phases := func(r *M8Run) []chaos.PhaseResult {
		if r == nil {
			return nil
		}
		return r.Summary.Phases
	}
	pb, pa := phases(before), phases(after)
	for i := range max(len(pb), len(pa)) {
		var row M8Phase
		dash := M8PhaseRun{"–", "–", "–", "–", "–", "–"}
		row.Before, row.After = dash, dash
		for k, ps := range [][]chaos.PhaseResult{pb, pa} {
			if i >= len(ps) {
				continue
			}
			p := ps[i]
			row.Name = p.Name
			row.Redis = "up"
			if !p.RedisUp {
				row.Redis = "cut off"
			}
			row.Replicas = fmt.Sprint(p.ReplicasUp)
			pr := M8PhaseRun{ErrorRate: q6Pct2(p.Main.ErrorRate), P99: msf(p.Main.Latency.P99), Ledger: "–", Redis: "–", Gap: "–", Reserved: "–"}
			if rc := p.Budget.Reconcile; rc != nil {
				pr.Ledger, pr.Redis, pr.Gap, pr.Reserved = q6USD(rc.Ledger), q6USD(rc.RedisSpent), m8USD(rc.Unreconciled), m8USD(rc.RedisReserved)
			}
			if k == 0 {
				row.Before = pr
			} else {
				row.After = pr
			}
		}
		m.Phases = append(m.Phases, row)
	}
	m.Checks = m8Checks(fb, fa, grace)
	m.OvershootBefore, m.OvershootAfter, m.RefillBound = "–", "–", "–"
	m.LostBefore, m.LostAfter, m.SlopeBefore, m.R2Before = "–", "–", "–", "–"
	m.FirstAfter, m.RestartedAfter, m.ProcessesAfter, m.T = "–", "–", "–", "–"
	if f := fb; f != nil {
		m.OvershootBefore, m.SlopeBefore, m.R2Before = q6USD(f.Overshoot), q6USD(f.Slope), fmt.Sprintf("%.3f", f.R2)
		m.T = fmt.Sprintf("%.0f s", f.TSeconds)
		if f.Kill != nil {
			m.LostBefore = m8USD(f.Kill.Lost)
		}
	}
	if f := fa; f != nil {
		m.OvershootAfter, m.RefillBound, m.ProcessesAfter = q6USD(f.Overshoot), q6USD(f.RefillBound), fmt.Sprint(f.Processes)
		m.T = fmt.Sprintf("%.0f s", f.TSeconds)
		if f.Kill != nil {
			m.LostAfter = m8USD(f.Kill.Lost)
		}
		if len(f.Outages) > 0 {
			m.FirstAfter = q6USD(f.Outages[0].Spent)
		}
		// The first outage after the killed replica came back.
		for _, o := range f.Outages {
			if f.Kill != nil && o.FromSeconds > f.Kill.AtSeconds && !o.ReplicaKilled {
				m.RestartedAfter = q6USD(o.Spent)
				break
			}
		}
	}
	return m, nil
}

// m8USD formats an amount to the millionth of a dollar, enough to show a
// single $0.000465 reply.
func m8USD(n cost.Nanos) string {
	if n == 0 {
		return "$0"
	}
	return fmt.Sprintf("$%.6f", n.USD())
}

// m8Checks judges PLAN.md M8's hypotheses by the rules fixed there before
// the runs.
func m8Checks(before, after *chaos.FlapResult, grace cost.Nanos) []Q6Check {
	held := func(ok bool) string {
		if ok {
			return "yes"
		}
		return "no"
	}
	// One in-flight reply at the chaos model's price.
	const reply = cost.Nanos(465_000)
	var out []Q6Check
	if f := before; f != nil {
		out = append(out, Q6Check{
			Hypothesis: "Before: eight outages overshoot by about 8 × 3 × grace = $0.24 (falsified under $0.06)",
			Observed:   fmt.Sprintf("overshoot %s at the end; %s spent during the outages", q6USD(f.Overshoot), q6USD(f.OutageSpend)),
			Held:       held(f.Overshoot >= 60*cost.Dollar/1000),
		})
		lo, hi := 3*grace*3/4, 3*grace*5/4
		out = append(out, Q6Check{
			Hypothesis: fmt.Sprintf("Before: the overshoot grows linearly with the outages (R² ≥ 0.95, slope %s to %s per outage)", q6USD(lo), q6USD(hi)),
			Observed:   fmt.Sprintf("slope %s per outage, R² %.3f", q6USD(f.Slope), f.R2),
			Held:       held(f.R2 >= 0.95 && f.Slope >= lo && f.Slope <= hi),
		})
	}
	if f := after; f != nil && f.RefillBound > 0 && len(f.Outages) > 0 {
		out = append(out, Q6Check{
			Hypothesis: "After: overshoot at most processes × grace × (1 + T / refill), a restarted replica counted as a new process",
			Observed:   fmt.Sprintf("overshoot %s against %s (%d processes, T = %.1f s)", q6USD(f.Overshoot), q6USD(f.RefillBound), f.Processes, f.TSeconds),
			Held:       held(f.Overshoot <= f.RefillBound),
		})
		first := f.Outages[0].Spent
		later := f.OutageSpend - first
		// One fresh grace for the restarted process plus what every
		// process's allowance could refill over T.
		allowed := grace + (f.RefillBound - cost.Nanos(f.Processes)*grace)
		out = append(out, Q6Check{
			Hypothesis: "After: whatever the outage count; outages 2 to 8 add at most the restarted process's grace plus the refill",
			Observed:   fmt.Sprintf("outage 1 spent %s, outages 2 to %d together %s (allowed %s)", q6USD(first), len(f.Outages), q6USD(later), q6USD(allowed)),
			Held:       held(later <= allowed),
		})
	}
	for _, c := range []struct {
		name string
		f    *chaos.FlapResult
	}{{"Before", before}, {"After", after}} {
		f := c.f
		if f == nil || f.Kill == nil {
			continue
		}
		pred := "$0.004 to $0.005"
		if c.name == "After" {
			pred = "about $0: its allowance was spent in outage 1"
		}
		out = append(out, Q6Check{
			Hypothesis: fmt.Sprintf("%s: the replica killed %.1f s into outage %s takes its pending spend with it (%s), at most one grace; nothing is lost without the kill",
				c.name, f.Kill.IntoOutageSeconds, killedOutage(f), pred),
			Observed: fmt.Sprintf("Redis short of the provider by %s at the end (bound %s); largest gap in a reachable phase before the kill %s",
				m8USD(f.Kill.Lost), q6USD(f.Kill.Bound), m8USD(f.MaxControlGap)),
			Held: held(f.Kill.Lost >= 0 && f.Kill.Lost <= f.Kill.Bound && f.MaxControlGap <= reply),
		})
	}
	return out
}

func killedOutage(f *chaos.FlapResult) string {
	for _, o := range f.Outages {
		if o.ReplicaKilled {
			return fmt.Sprint(o.N)
		}
	}
	return "?"
}

// m8Rerun sets the Q6 re-run's headline numbers beside the committed Q6.
func m8Rerun(m *M8, committed *chaos.Summary, rerun *chaos.Summary) {
	pick := func(s *chaos.Summary) map[string]string {
		out := map[string]string{}
		var replicaDown, redisDown, back *chaos.PhaseResult
		for i := range s.Phases {
			p := &s.Phases[i]
			switch {
			case redisDown == nil && !p.RedisUp:
				redisDown = p
			case replicaDown == nil && p.RedisUp && p.ReplicasUp < s.Meta.Replicas && redisDown == nil:
				replicaDown = p
			case redisDown != nil && back == nil && p.RedisUp:
				back = p
			}
		}
		if replicaDown != nil {
			out["kill"] = q6Pct2(replicaDown.Main.ErrorRate)
		}
		if redisDown != nil {
			out["p99"] = msf(redisDown.Main.Latency.P99) + " ms"
			out["errors"] = fmt.Sprint(redisDown.Main.Errors)
			out["outage"] = fmt.Sprintf("%s (bound %s)", q6USD(redisDown.Budget.Spent), q6USD(redisDown.Budget.GraceBound))
		}
		if back != nil {
			out["repeat"] = q6Pct(back.Main.RepeatHitRate)
		}
		out["overshoot"] = q6USD(s.Phases[len(s.Phases)-1].Budget.Overshoot)
		return out
	}
	c, r := pick(committed), pick(rerun)
	for _, row := range []struct{ key, name string }{
		{"kill", "Error rate after the replica kill"},
		{"errors", "Errors with Redis down"},
		{"p99", "p99 with Redis down"},
		{"outage", "Spent while Redis was down"},
		{"repeat", "Hit rate of repeats after the Redis restart"},
		{"overshoot", "Overshoot at the end"},
	} {
		cv, rv := c[row.key], r[row.key]
		if cv == "" {
			cv = "–"
		}
		if rv == "" {
			rv = "–"
		}
		m.Rerun = append(m.Rerun, M8Total{Name: row.name, Before: cv, After: rv})
	}
}

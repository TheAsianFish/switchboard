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
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/chaos"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// M9Dir holds M9's runs: `make chaos-persist` writes chaos-<mode>/ (a
// `make chaos` run with REDIS_PERSIST=<mode>) and `make bench-persist`
// writes bench/<mode>/ (a Q1 breakdown run of the budget config).
const M9Dir = "m9-persist"

// M9BenchDir is the subdirectory of M9Dir holding the bench runs.
const M9BenchDir = "bench"

// M9Modes are Redis's append-only modes in the order PLAN.md's rule
// tries them: cheapest first.
var M9Modes = []string{"none", "everysec", "always"}

// M9RestartPhase is the Q6 schedule's phase after the Redis restart, the
// one M9 reads errors, hit rate and the reconciliation gap in. Its
// snapshot is taken 0.5 s before replica 2 starts.
const M9RestartPhase = "replica down, Redis back"

// PLAN.md M9's thresholds, fixed before the full runs.
const (
	m9EverysecCostMS = 0.1  // everysec: p50 and p99, both metrics
	m9AlwaysCostMS   = 0.2  // always: p99, both metrics
	m9RuleCostMS     = 0.5  // the rule: larger p99 cost
	m9RepeatHitRate  = 0.95 // everysec: hit rate of repeats after the restart
)

// M9 is the section on budgets across a Redis restart: Q6's schedule
// once per append-only mode, and what each mode adds to a request.
type M9 struct {
	// Modes are M9Modes, the per-mode tables' columns.
	Modes []string
	// Runs has one entry per mode, in M9Modes order; a mode's Chaos or
	// Bench is nil when that run is missing.
	Runs []*M9Run
	// ChaosRows and BenchRows are the per-mode table: one number per row,
	// one value per mode ("–" where the run is missing). Nil without any
	// chaos (bench) run.
	ChaosRows, BenchRows []M9Row
	// Slow is Q6's slow stretch read from the none arm's logs: the store
	// errors the replicas logged after the Redis kill, by class. SlowEvent
	// names the kill and SlowWindow how long Redis stayed down; nil
	// classes without a none arm with logs.
	Slow                  []M9Class
	SlowEvent, SlowWindow string
	// Restarts are the replicas' way back after the Redis start, per mode
	// with a chaos run whose logs were kept.
	Restarts []M9Restart
	Checks   []Q6Check
	// Rule is the pre-stated rule applied mode by mode. Default is the
	// mode it picks; DefaultReason says why. Decided is false when a mode
	// the rule needed was not measured (Default is then empty).
	Rule          []M9RuleRow
	Default       string
	DefaultReason string
	Decided       bool
	// NoPass is true when no mode passed, so the default stays none.
	NoPass bool

	// For prose, from the chaos runs' meta and summary (the first chaos
	// run in mode order): the budget and grace; the grace bound at the
	// Redis kill (replicas up × grace); the budget stream's rate and
	// price per reply (steady phase spend over its ledger requests); the
	// everysec bound (grace bound plus one second of spend); Redis's data
	// filesystem; the schedule's scale.
	Budget, Grace, ReplicasAtKill, GraceBound   string
	BudgetRate, PricePerReply, EverysecBound    string
	DataFS, TimeScale, DurationSeconds, Persist string
	// Bench: the rate and window, the data filesystem, the drift (largest
	// spread between a mode's two direct runs) per metric and percentile,
	// and Q1's committed drift beside it ("–" without Q1's breakdown).
	BenchRate, BenchWindow, BenchDataFS                      string
	DriftTotalP50, DriftTotalP99, DriftTTFTP50, DriftTTFTP99 string
	// DriftMax is the largest of the four drifts, "n/a" until both
	// metrics have one.
	DriftMax               string
	Q1DriftP50, Q1DriftP99 string
	Resamples              int
	// ToMeasure lists the missing runs with their commands.
	ToMeasure []string
	// Reading is sentences built from the numbers above, no
	// interpretation; Headline the README's line.
	Reading  []string
	Headline string
}

// M9Run is one mode's runs.
type M9Run struct {
	Mode  string
	Chaos *M9Chaos
	Bench *M9Bench
}

// M9Chaos is one mode's `make chaos` run, every number formatted. Fields
// read from the logs are "–" when the run kept none.
type M9Chaos struct {
	Meta     chaos.Meta
	Hardware string
	Summary  *chaos.Summary
	// Overshoot is at the end of the run; Lost is the reconciliation gap
	// at the snapshot ending M9RestartPhase (ledger minus Redis spent).
	Overshoot, Lost string
	// In M9RestartPhase: the main stream's error rate and errors, the
	// other streams' errors other than 429 and 402, the hit rate of
	// repeats; SteadyRepeatHitRate is the steady phase's.
	ErrorRate, Errors, OtherErrors, RepeatHitRate, SteadyRepeatHitRate string
	// OutageSpent is what the budgeted key spent while Redis was down,
	// with its grace bound.
	OutageSpent, OutageBound string
	// From the logs, for the (first) Redis start: AOF load time Redis
	// reported, start to "Ready to accept connections", replica lines with
	// a LOADING error, start until the last live replica component logged
	// its recovery, store errors by class after the start.
	AOFLoaded, Ready, Loading, Degraded, RestartErrors string
	// AOFSize is the append-only file's size at the end; DataFS Redis's
	// /data filesystem.
	AOFSize, DataFS string
	// Raw values for prose; nil when not recorded.
	ReadySeconds, AOFLoadedSeconds, DegradedSeconds *float64
	LoadingLines                                    int
	// FirstDNSSeconds and LastTimeoutSeconds are the first DNS error line
	// and the last timeout line after the Redis kill, seconds after it.
	FirstDNSSeconds, LastTimeoutSeconds *float64

	overshoot, bound, graceBound cost.Nanos
	boundOK, logs                bool
	restart                      *chaos.PhaseResult
	steady                       chaos.MainPhase
}

// M9Bench is one mode's bench-persist run.
type M9Bench struct {
	Meta     Meta
	Hardware string
	// Window is "30 s after 2 s warm-up"; RedisArgs and DataFS how Redis
	// ran.
	Window, RedisArgs, DataFS string
	// Added latency against the mode's pooled direct runs, ms: non-
	// streaming total and streaming TTFT.
	TotalP50, TotalP99, TTFTP50, TTFTP99 Est
	// Cost against none's added latency at the same percentile; n/a for
	// none itself and without a none run.
	CostTotalP50, CostTotalP99, CostTTFTP50, CostTTFTP99 Est
	// Spread between the mode's two direct runs, ms.
	SpreadTotalP50, SpreadTotalP99, SpreadTTFTP50, SpreadTTFTP99 string
	// From Redis's INFO persistence after the non-streaming and streaming
	// runs: delayed fsyncs and the append-only file's size.
	DelayedFsync, AOFSize string
	// Requests and errors of the gateway runs, "non-streaming / streaming".
	Requests, Errors string

	added    [2]diff // [m9Total, m9TTFT]
	cost     [2]diff
	spread   [2][2]float64 // [metric][p50, p99]
	spreadOK [2]bool
}

const (
	m9Total = 0
	m9TTFT  = 1
)

// M9Row is one number per mode.
type M9Row struct {
	Name   string
	Values []string
}

// M9Class is one error class the replicas logged after the Redis kill.
type M9Class struct {
	Class, Lines, PoolLines, First, Last, Example string
}

// M9Restart is one mode's Redis start as the logs saw it.
type M9Restart struct {
	Mode, At, Ready, AOFLoaded, Degraded, Recoveries, Errors string
}

// M9RuleRow is one mode against the rule.
type M9RuleRow struct {
	Mode string
	// Overshoot against the bound, and whether it is within it.
	Overshoot, OvershootOK string
	// Cost is the larger p99 cost of the two metrics with the drift;
	// CostOK whether it passes.
	Cost, CostOK string
	// Passes is "yes", "no" or "not measured".
	Passes string
}

// loadM9 reads M9's runs under dir; q1 is the loaded Q1 section, for its
// committed drift. Nil when no run exists.
func loadM9(dir string, q1 *Q1) (*M9, error) {
	m := &M9{Modes: M9Modes, Resamples: q1Resamples}
	found := false
	for _, mode := range M9Modes {
		r := &M9Run{Mode: mode}
		c, err := loadM9Chaos(filepath.Join(dir, "chaos-"+mode))
		if err != nil {
			return nil, err
		}
		if c != nil && c.Meta.RedisPersist != "" && c.Meta.RedisPersist != mode {
			return nil, fmt.Errorf("results: %s/chaos-%s: summary says redis_persist %q", dir, mode, c.Meta.RedisPersist)
		}
		b, err := loadM9Bench(filepath.Join(dir, M9BenchDir, mode), mode)
		if err != nil {
			return nil, err
		}
		r.Chaos, r.Bench = c, b
		found = found || c != nil || b != nil
		m.Runs = append(m.Runs, r)
	}
	if !found {
		return nil, nil
	}
	m.Q1DriftP50, m.Q1DriftP99 = "–", "–"
	if q1 != nil && q1.Breakdown != nil && len(q1.Breakdown.Baselines) > 0 {
		m.Q1DriftP50, m.Q1DriftP99 = msf(q1.Breakdown.driftP50), msf(q1.Breakdown.driftP99)
	}
	buildM9(m)
	return m, nil
}

func loadM9Chaos(dir string) (*M9Chaos, error) {
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
	return buildM9Chaos(&s)
}

// m9KillPhase is the first phase with Redis down.
func m9KillPhase(s *chaos.Summary) *chaos.PhaseResult {
	for i := range s.Phases {
		if !s.Phases[i].RedisUp {
			return &s.Phases[i]
		}
	}
	return nil
}

func m9Phase(s *chaos.Summary, name string) *chaos.PhaseResult {
	for i := range s.Phases {
		if s.Phases[i].Name == name {
			return &s.Phases[i]
		}
	}
	return nil
}

// m9Reply is the budgeted key's price per reply: the steady phase's spend
// over its ledger requests; 0 if it admitted none.
func m9Reply(s *chaos.Summary) cost.Nanos {
	b := s.Phases[0].Budget
	if b.Admitted == 0 {
		return 0
	}
	return b.Spent / cost.Nanos(b.Admitted)
}

func m9BudgetRate(s *chaos.Summary) float64 {
	for _, st := range s.Streams {
		if st.Name == "budget" {
			return st.Rate
		}
	}
	return 0
}

// m9Bounds are PLAN.md's overshoot bounds for a run: the grace of the
// replicas up at the Redis kill, and that plus one second of the budget
// stream's spend. ok is false when the run cannot give them.
func m9Bounds(s *chaos.Summary) (graceBound, everysec cost.Nanos, ok bool) {
	grace, err := cost.ParseUSD(s.Meta.Budget.GraceUSD)
	kill := m9KillPhase(s)
	reply, rate := m9Reply(s), m9BudgetRate(s)
	if err != nil || kill == nil || reply == 0 || rate == 0 {
		return 0, 0, false
	}
	graceBound = grace * cost.Nanos(kill.ReplicasUp)
	return graceBound, graceBound + cost.Nanos(math.Round(rate*float64(reply))), true
}

func buildM9Chaos(s *chaos.Summary) (*M9Chaos, error) {
	const na = "–"
	c := &M9Chaos{Meta: s.Meta, Hardware: describeHardware(s.Hardware), Summary: s,
		Lost: na, ErrorRate: na, Errors: na, OtherErrors: na, RepeatHitRate: na, OutageSpent: na, OutageBound: na,
		AOFLoaded: na, Ready: na, Loading: na, Degraded: na, RestartErrors: na, AOFSize: na, DataFS: na,
		steady: s.Phases[0].Main}
	c.overshoot = s.Phases[len(s.Phases)-1].Budget.Overshoot
	c.Overshoot = m8USD(c.overshoot)
	c.SteadyRepeatHitRate = q6Pct(c.steady.RepeatHitRate)
	c.graceBound, c.bound, c.boundOK = m9Bounds(s)
	if p := m9KillPhase(s); p != nil {
		c.OutageSpent, c.OutageBound = m8USD(p.Budget.Spent), m8USD(p.Budget.GraceBound)
	}
	if p := m9Phase(s, M9RestartPhase); p != nil {
		c.restart = p
		c.ErrorRate, c.Errors = q6Pct2(p.Main.ErrorRate), fmt.Sprint(p.Main.Errors)
		c.OtherErrors = fmt.Sprintf("%d (limited %d, budget %d)", p.Limited.OtherErrors+p.Budget.OtherErrors, p.Limited.OtherErrors, p.Budget.OtherErrors)
		c.RepeatHitRate = q6Pct(p.Main.RepeatHitRate)
		if rc := p.Budget.Reconcile; rc != nil {
			c.Lost = m8USD(rc.Unreconciled)
		}
	}
	if r := s.Meta.Redis; r != nil {
		c.DataFS = r.DataFS
		if r.AOFBytes > 0 {
			c.AOFSize = m9Bytes(r.AOFBytes)
		} else if r.AppendOnly != "yes" {
			c.AOFSize = "no file"
		}
	}
	if st := s.Store; st != nil {
		c.logs = true
		c.Loading = "0"
		if len(st.Restarts) > 0 {
			rr := st.Restarts[0]
			c.ReadySeconds, c.AOFLoadedSeconds, c.DegradedSeconds = rr.ReadySeconds, rr.LoadedSeconds, rr.DegradedSeconds
			c.LoadingLines = rr.LoadingErrors
			c.Loading = fmt.Sprint(rr.LoadingErrors)
			c.Ready = m9Secs(rr.ReadySeconds, "no ready line")
			c.AOFLoaded = m9Secs(rr.LoadedSeconds, "no file loaded")
			c.Degraded = m9Secs(rr.DegradedSeconds, "not recovered")
			if len(rr.Recoveries) == 0 {
				c.Degraded = "none degraded"
			}
			c.RestartErrors = m9Classes(rr.Errors)
		}
		if len(st.Outages) > 0 {
			for _, cl := range st.Outages[0].Classes {
				v := cl.FirstSeconds
				if cl.Class == chaos.ErrClassDNS && c.FirstDNSSeconds == nil {
					c.FirstDNSSeconds = &v
				}
				if cl.Class == chaos.ErrClassTimeout {
					l := cl.LastSeconds
					c.LastTimeoutSeconds = &l
				}
			}
		}
	}
	return c, nil
}

// m9Secs formats seconds to the millisecond, or the reason it is absent.
func m9Secs(v *float64, missing string) string {
	if v == nil {
		return missing
	}
	return fmt.Sprintf("%.3f s", *v)
}

// m9Bytes formats a size in decimal megabytes.
func m9Bytes(n int64) string {
	return fmt.Sprintf("%.2f MB", float64(n)/1e6)
}

// m9Classes formats error classes as "timeout 119, dns 5", or "none".
func m9Classes(cs []chaos.ErrorClassCount) string {
	m := map[string]int{}
	for _, c := range cs {
		m[c.Class] += c.Lines
	}
	return q6Kinds(m)
}

func loadM9Bench(dir, mode string) (*M9Bench, error) {
	var meta Meta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	bdir := filepath.Join(dir, Q1BreakdownDir)
	runs, err := loadBreakdownRuns(bdir)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	b := &M9Bench{Meta: meta, DelayedFsync: "–", AOFSize: "–"}
	if bm := meta.Breakdown; bm != nil {
		b.RedisArgs, b.DataFS = bm.RedisArgs, bm.RedisDataFS
		if bm.RedisPersist != "" && bm.RedisPersist != mode {
			return nil, fmt.Errorf("results: %s: meta.json says redis_persist %q", dir, bm.RedisPersist)
		}
	}
	var gw [2]*bkRun
	directs := [2][]*bkRun{}
	windows := map[string]bool{}
	for _, r := range runs {
		k := m9Total
		if r.stream {
			k = m9TTFT
		}
		windows[fmt.Sprintf("%s s after %s s warm-up", num(r.res.Config.DurationSec), num(r.res.Config.WarmupSec))] = true
		switch r.config {
		case "direct":
			directs[k] = append(directs[k], r)
		case "budget":
			if gw[k] != nil {
				return nil, fmt.Errorf("results: %s has two budget %s runs", bdir, modeName(r.stream))
			}
			gw[k] = r
		default:
			return nil, fmt.Errorf("results: %s: unexpected config %s (bench-persist runs budget only)", bdir, r.name)
		}
	}
	b.Window = strings.Join(sortedKeys(windows), "; ")
	var spreads [2][2]string
	for k := range 2 {
		rs := directs[k]
		slices.SortFunc(rs, func(a, b *bkRun) int { return a.seq - b.seq })
		var all []float64
		lo50, hi50, lo99, hi99 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		for _, r := range rs {
			xs := pick(r)
			all = append(all, xs...)
			p50, p99 := m9Pct(xs, 50), m9Pct(xs, 99)
			lo50, hi50, lo99, hi99 = min(lo50, p50), max(hi50, p50), min(lo99, p99), max(hi99, p99)
		}
		spreads[k] = [2]string{"n/a", "n/a"}
		if len(rs) > 1 {
			b.spread[k], b.spreadOK[k] = [2]float64{hi50 - lo50, hi99 - lo99}, true
			spreads[k] = [2]string{msf(hi50 - lo50), msf(hi99 - lo99)}
		}
		if g := gw[k]; g != nil && len(rs) > 0 {
			rate := num(g.rate)
			pooled := newBoot(fmt.Sprintf("m9/%s/direct-%s-%s-pooled", mode, modeName(k == m9TTFT), rate), all)
			b.added[k] = sub(newBoot("m9/"+mode+"/"+g.name, pick(g)), pooled)
		}
	}
	b.SpreadTotalP50, b.SpreadTotalP99 = spreads[m9Total][0], spreads[m9Total][1]
	b.SpreadTTFTP50, b.SpreadTTFTP99 = spreads[m9TTFT][0], spreads[m9TTFT][1]
	b.TotalP50, b.TotalP99 = b.added[m9Total].ests()
	b.TTFTP50, b.TTFTP99 = b.added[m9TTFT].ests()

	var req, errs, delayed, size, hw []string
	for k, g := range gw {
		if g == nil {
			req, errs, delayed, size = append(req, "n/a"), append(errs, "n/a"), append(delayed, "n/a"), append(size, "n/a")
			continue
		}
		if k == m9Total || len(hw) == 0 {
			hw = append(hw, describeHardware(g.res.Hardware))
		}
		req, errs = append(req, strconv.Itoa(g.res.Requests)), append(errs, strconv.Itoa(g.res.Errors()))
		info, err := loadRedisInfo(filepath.Join(bdir, g.name+".redis-persistence.txt"))
		if err != nil {
			return nil, err
		}
		d, s := "n/a", "n/a"
		if info != nil {
			// Redis counts a delayed fsync only under everysec (a write
			// held back while the background fsync is late); under always
			// the fsync is inline and a slow disk shows as latency.
			if v, ok := info["aof_delayed_fsync"]; ok && mode == "everysec" {
				d = v
			}
			if v, err := strconv.ParseInt(info["aof_current_size"], 10, 64); err == nil {
				s = m9Bytes(v)
			} else if info["aof_enabled"] == "0" {
				s = "no file"
			}
		}
		delayed, size = append(delayed, d), append(size, s)
	}
	b.Requests, b.Errors = strings.Join(req, " / "), strings.Join(errs, " / ")
	b.DelayedFsync, b.AOFSize = strings.Join(delayed, " / "), strings.Join(size, " / ")
	if len(hw) > 0 {
		b.Hardware = hw[0]
	}
	return b, nil
}

// m9Pct is the nearest-rank percentile of xs, as newBoot computes it.
func m9Pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[rank(p, len(s))-1]
}

// loadRedisInfo reads a saved `INFO persistence` reply as field to value;
// nil if the file is missing.
func loadRedisInfo(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = v
		}
	}
	return out, nil
}

func buildM9(m *M9) {
	const na = "–"
	m.Budget, m.Grace, m.ReplicasAtKill, m.GraceBound = na, na, na, na
	m.BudgetRate, m.PricePerReply, m.EverysecBound = na, na, na
	m.DataFS, m.TimeScale, m.DurationSeconds, m.Persist = na, na, na, na
	m.BenchRate, m.BenchWindow, m.BenchDataFS = na, na, na
	byMode := map[string]*M9Run{}
	var fss, bfss []string
	for _, r := range m.Runs {
		byMode[r.Mode] = r
		if c := r.Chaos; c != nil {
			if c.DataFS != na && !slices.Contains(fss, c.DataFS) {
				fss = append(fss, c.DataFS)
			}
			if m.Budget == na {
				s := c.Summary
				if b, err := cost.ParseUSD(s.Meta.Budget.USD); err == nil {
					m.Budget = q6USD(b)
				}
				if g, err := cost.ParseUSD(s.Meta.Budget.GraceUSD); err == nil {
					m.Grace = q6USD(g)
				}
				if p := m9KillPhase(s); p != nil {
					m.ReplicasAtKill = fmt.Sprint(p.ReplicasUp)
				}
				m.BudgetRate = num(m9BudgetRate(s))
				m.PricePerReply = m8USD(m9Reply(s))
				if c.boundOK {
					m.GraceBound, m.EverysecBound = m8USD(c.graceBound), m8USD(c.bound)
				}
				m.TimeScale, m.DurationSeconds = num(s.Meta.TimeScale), num(s.Meta.DurationSeconds)
			}
		} else {
			m.ToMeasure = append(m.ToMeasure, fmt.Sprintf("M9 `%s` arm of the Q6 schedule: `MODES=%s make chaos-persist`, then `make results`.", r.Mode, r.Mode))
		}
		if b := r.Bench; b != nil {
			if b.DataFS != "" && !slices.Contains(bfss, b.DataFS) {
				bfss = append(bfss, b.DataFS)
			}
			if m.BenchWindow == na {
				m.BenchWindow = b.Window
				if b.Meta.Breakdown != nil {
					m.BenchRate = num(b.Meta.Breakdown.Rate)
				}
			}
		} else {
			m.ToMeasure = append(m.ToMeasure, fmt.Sprintf("M9 `%s` latency: `MODES=%s make bench-persist`, then `make results`.", r.Mode, r.Mode))
		}
	}
	if len(fss) > 0 {
		m.DataFS = strings.Join(fss, ", ")
	}
	if len(bfss) > 0 {
		m.BenchDataFS = strings.Join(bfss, ", ")
	}

	// Costs against none, and the drift across every mode's direct runs.
	none := byMode["none"].Bench
	var drift [2][2]float64
	var driftOK [2]bool
	for _, r := range m.Runs {
		b := r.Bench
		if b == nil {
			continue
		}
		for k := range 2 {
			if b.spreadOK[k] {
				driftOK[k] = true
				drift[k][0], drift[k][1] = max(drift[k][0], b.spread[k][0]), max(drift[k][1], b.spread[k][1])
			}
			if r.Mode != "none" && none != nil {
				b.cost[k] = b.added[k].minus(none.added[k])
			}
		}
		b.CostTotalP50, b.CostTotalP99 = b.cost[m9Total].ests()
		b.CostTTFTP50, b.CostTTFTP99 = b.cost[m9TTFT].ests()
	}
	m.DriftTotalP50, m.DriftTotalP99, m.DriftTTFTP50, m.DriftTTFTP99 = "n/a", "n/a", "n/a", "n/a"
	if driftOK[m9Total] {
		m.DriftTotalP50, m.DriftTotalP99 = msf(drift[m9Total][0]), msf(drift[m9Total][1])
	}
	if driftOK[m9TTFT] {
		m.DriftTTFTP50, m.DriftTTFTP99 = msf(drift[m9TTFT][0]), msf(drift[m9TTFT][1])
	}
	m.DriftMax = "n/a"
	if driftOK[m9Total] && driftOK[m9TTFT] {
		m.DriftMax = msf(max(drift[m9Total][0], drift[m9Total][1], drift[m9TTFT][0], drift[m9TTFT][1]))
	}

	m9Tables(m)
	m9Logs(m, byMode["none"].Chaos)

	// Hypotheses and the rule.
	in := m9Inputs{drift: drift, driftOK: driftOK}
	for _, r := range m.Runs {
		in.chaos = append(in.chaos, r.Chaos)
		in.cost = append(in.cost, m9Costs(r.Bench))
	}
	m.Checks = m9Checks(in)
	m.Rule, m.Default, m.DefaultReason, m.Decided, m.NoPass = m9Rule(in)
	m.Reading = m9Reading(m)
	m.Headline = m9Headline(m)
}

// m9Reading states, per mode, what the logs and the reconciliation
// recorded; the interpretation is written into the template by hand.
func m9Reading(m *M9) []string {
	var out []string
	for _, r := range m.Runs {
		c := r.Chaos
		if c == nil {
			continue
		}
		s := fmt.Sprintf("`%s`: overshoot %s at the end, %s of spend Redis did not hold at the snapshot ending `%s`", r.Mode, c.Overshoot, c.Lost, M9RestartPhase)
		if c.logs {
			s += fmt.Sprintf("; Redis ready %s after its start (append-only file: %s), %s replica lines with `LOADING`, degraded %s",
				m9Prose(c.Ready), m9Prose(c.AOFLoaded), c.Loading, m9Prose(c.Degraded))
		} else {
			s += "; no logs kept"
		}
		out = append(out, s+".")
	}
	if len(m.Slow) > 0 {
		var parts []string
		for _, c := range m.Slow {
			parts = append(parts, fmt.Sprintf("%s from %s s to %s s (%s lines)", c.Class, c.First, c.Last, c.Lines))
		}
		out = append(out, fmt.Sprintf("After `%s` in the `none` arm the replicas logged %s.", m.SlowEvent, strings.Join(parts, ", ")))
	}
	return out
}

// m9Prose turns a table cell such as "0.432 s" into prose; a reason
// ("not recovered") is kept as is.
func m9Prose(v string) string {
	if v == "–" {
		return "not recorded"
	}
	return v
}

// m9Headline is the README's line.
func m9Headline(m *M9) string {
	var over []string
	for _, r := range m.Runs {
		v := "–"
		if r.Chaos != nil {
			v = r.Chaos.Overshoot
		}
		name := "with `" + r.Mode + "`"
		if r.Mode == "none" {
			name = "with no append-only file"
		}
		over = append(over, v+" "+name)
	}
	s := "Budget across a Redis restart: overshoot " + strings.Join(over, ", ")
	for _, r := range m.Runs {
		if r.Mode != "none" && r.Bench != nil && r.Bench.CostTotalP99.ok {
			s += fmt.Sprintf("; `%s` adds %s ms at p99 (non-streaming total, against none)",
				r.Mode, r.Bench.CostTotalP99.Point())
		}
	}
	switch {
	case m.NoPass:
		s += "; no mode passed the rule, so the compose default stays `none`"
	case m.Decided:
		s += "; compose default by the pre-stated rule: `" + m.Default + "`"
	default:
		s += "; compose default not decided yet"
	}
	return s
}

// m9Costs are a bench run's costs against none, [metric][p50, p99], nil
// where not measured.
func m9Costs(b *M9Bench) [2][2]*float64 {
	var out [2][2]*float64
	if b == nil {
		return out
	}
	for k := range 2 {
		if d := b.cost[k]; d.ok {
			v50, v99 := d.v50, d.v99
			out[k] = [2]*float64{&v50, &v99}
		}
	}
	return out
}

func m9Tables(m *M9) {
	row := func(name string, get func(c *M9Chaos) string) M9Row {
		r := M9Row{Name: name}
		for _, run := range m.Runs {
			v := "–"
			if run.Chaos != nil {
				v = get(run.Chaos)
			}
			r.Values = append(r.Values, v)
		}
		return r
	}
	hasChaos, hasBench := false, false
	for _, r := range m.Runs {
		hasChaos = hasChaos || r.Chaos != nil
		hasBench = hasBench || r.Bench != nil
	}
	if hasChaos {
		m.ChaosRows = []M9Row{
			row("Overshoot at the end", func(c *M9Chaos) string { return c.Overshoot }),
			row("Spent while Redis was down (grace bound)", func(c *M9Chaos) string {
				return fmt.Sprintf("%s (%s)", c.OutageSpent, c.OutageBound)
			}),
			row("Spend Redis forgot at the restart (gap at the snapshot ending `"+M9RestartPhase+"`)", func(c *M9Chaos) string { return c.Lost }),
			row("Main stream's error rate after the restart (errors)", func(c *M9Chaos) string {
				return fmt.Sprintf("%s (%s)", c.ErrorRate, c.Errors)
			}),
			row("Other streams' errors other than 429 and 402 after the restart", func(c *M9Chaos) string { return c.OtherErrors }),
			row("Hit rate of repeats after the restart (steady phase)", func(c *M9Chaos) string {
				return fmt.Sprintf("%s (%s)", c.RepeatHitRate, c.SteadyRepeatHitRate)
			}),
			row("Append-only file load time Redis reported", func(c *M9Chaos) string { return c.AOFLoaded }),
			row("Redis start to ready", func(c *M9Chaos) string { return c.Ready }),
			row("Replica log lines with a `LOADING` error", func(c *M9Chaos) string { return c.Loading }),
			row("Degraded after the restart (until the last live replica component recovered)", func(c *M9Chaos) string { return c.Degraded }),
			row("Append-only file at the end", func(c *M9Chaos) string { return c.AOFSize }),
			row("Redis `/data` filesystem", func(c *M9Chaos) string { return c.DataFS }),
		}
	}
	if hasBench {
		brow := func(name string, get func(b *M9Bench) string) M9Row {
			r := M9Row{Name: name}
			for _, run := range m.Runs {
				v := "–"
				if run.Bench != nil {
					v = get(run.Bench)
				}
				r.Values = append(r.Values, v)
			}
			return r
		}
		costRow := func(name string, e func(b *M9Bench) Est) M9Row {
			return brow(name, func(b *M9Bench) string {
				if b == m.Runs[0].Bench {
					return "0 (by definition)"
				}
				return e(b).String()
			})
		}
		m.BenchRows = []M9Row{
			brow("Added p50, non-streaming total (ms)", func(b *M9Bench) string { return b.TotalP50.String() }),
			brow("Added p99, non-streaming total (ms)", func(b *M9Bench) string { return b.TotalP99.String() }),
			brow("Added p50, streaming TTFT (ms)", func(b *M9Bench) string { return b.TTFTP50.String() }),
			brow("Added p99, streaming TTFT (ms)", func(b *M9Bench) string { return b.TTFTP99.String() }),
			costRow("Cost against `none`, p50 total (ms)", func(b *M9Bench) Est { return b.CostTotalP50 }),
			costRow("Cost against `none`, p99 total (ms)", func(b *M9Bench) Est { return b.CostTotalP99 }),
			costRow("Cost against `none`, p50 TTFT (ms)", func(b *M9Bench) Est { return b.CostTTFTP50 }),
			costRow("Cost against `none`, p99 TTFT (ms)", func(b *M9Bench) Est { return b.CostTTFTP99 }),
			brow("Spread between the direct runs, total p50 / p99 (ms)", func(b *M9Bench) string {
				return b.SpreadTotalP50 + " / " + b.SpreadTotalP99
			}),
			brow("Spread between the direct runs, TTFT p50 / p99 (ms)", func(b *M9Bench) string {
				return b.SpreadTTFTP50 + " / " + b.SpreadTTFTP99
			}),
			brow("Gateway requests (non-streaming / streaming)", func(b *M9Bench) string { return b.Requests }),
			brow("Gateway errors (non-streaming / streaming)", func(b *M9Bench) string { return b.Errors }),
			brow("Redis delayed fsyncs after each run (counted under `everysec` only)", func(b *M9Bench) string { return b.DelayedFsync }),
			brow("Append-only file after each run", func(b *M9Bench) string { return b.AOFSize }),
		}
	}
}

// m9Logs fills Q6's slow stretch from the none arm and the restarts from
// every arm with logs.
func m9Logs(m *M9, none *M9Chaos) {
	if none != nil && none.Summary.Store != nil && len(none.Summary.Store.Outages) > 0 {
		o := none.Summary.Store.Outages[0]
		m.SlowEvent = o.Event
		m.SlowWindow = fmt.Sprintf("%.1f s", o.ToSeconds-o.AtSeconds)
		for _, c := range o.Classes {
			m.Slow = append(m.Slow, M9Class{
				Class: c.Class, Lines: fmt.Sprint(c.Lines), PoolLines: fmt.Sprint(c.PoolLines),
				First: fmt.Sprintf("%.3f", c.FirstSeconds), Last: fmt.Sprintf("%.3f", c.LastSeconds),
				Example: strings.ReplaceAll(c.Example, "|", `\|`),
			})
		}
	}
	for _, r := range m.Runs {
		c := r.Chaos
		if c == nil || c.Summary.Store == nil || len(c.Summary.Store.Restarts) == 0 {
			continue
		}
		rr := c.Summary.Store.Restarts[0]
		var parts []string
		byReplica := map[string][]string{}
		var replicas []string
		for _, rec := range rr.Recoveries {
			if _, ok := byReplica[rec.Replica]; !ok {
				replicas = append(replicas, rec.Replica)
			}
			v := "never"
			if rec.Seconds != nil {
				v = fmt.Sprintf("%.3f", *rec.Seconds)
			}
			byReplica[rec.Replica] = append(byReplica[rec.Replica], rec.Component+" "+v)
		}
		for _, rep := range replicas {
			parts = append(parts, rep+": "+strings.Join(byReplica[rep], ", "))
		}
		rec := strings.Join(parts, "; ")
		if rec == "" {
			rec = "none degraded"
		}
		m.Restarts = append(m.Restarts, M9Restart{
			Mode: r.Mode, At: fmt.Sprintf("%.2f", rr.AtSeconds), Ready: c.Ready, AOFLoaded: c.AOFLoaded,
			Degraded: c.Degraded, Recoveries: rec, Errors: c.RestartErrors,
		})
	}
}

// m9Inputs are what the judging needs, per mode in M9Modes order.
type m9Inputs struct {
	chaos []*M9Chaos
	// cost is each mode's cost against none, [metric][p50, p99].
	cost    [][2][2]*float64
	drift   [2][2]float64
	driftOK [2]bool
}

// m9Verdict judges one cost by PLAN.md's rule: no larger than the drift
// is unresolved; otherwise it holds under the threshold and fails by the
// excess.
func m9Verdict(costMS, driftMS, thresholdMS float64) (held string, excess float64) {
	switch {
	case math.Abs(costMS) <= driftMS:
		return "unresolved", 0
	case costMS < thresholdMS:
		return "yes", 0
	}
	return "no", costMS - thresholdMS
}

// m9Combine is "no" if any verdict is, else "unresolved" if any is, else
// "yes".
func m9Combine(vs ...string) string {
	switch {
	case slices.Contains(vs, "no"):
		return "no"
	case slices.Contains(vs, "unresolved"):
		return "unresolved"
	}
	return "yes"
}

var m9Metric = [2]string{"total", "TTFT"}
var m9Pctl = [2]string{"p50", "p99"}

// m9CostCheck judges a mode's cost at the given percentiles of both
// metrics against threshold; ok is false without the costs.
func m9CostCheck(in m9Inputs, i int, pcts []int, threshold float64) (obs, held string, ok bool) {
	var vs, parts []string
	for k := range 2 {
		for _, p := range pcts {
			c := in.cost[i][k][p]
			if c == nil || !in.driftOK[k] {
				return "", "", false
			}
			v, excess := m9Verdict(*c, in.drift[k][p], threshold)
			s := fmt.Sprintf("%s %s %s ms (drift %s)", m9Metric[k], m9Pctl[p], added(*c), msf(in.drift[k][p]))
			switch v {
			case "unresolved":
				s += ", unresolved"
			case "no":
				s += fmt.Sprintf(", over by %s ms", msf(excess))
			}
			vs, parts = append(vs, v), append(parts, s)
		}
	}
	return strings.Join(parts, "; "), m9Combine(vs...), true
}

func m9Checks(in m9Inputs) []Q6Check {
	held := func(ok bool) string {
		if ok {
			return "yes"
		}
		return "no"
	}
	idx := func(mode string) int { return slices.Index(M9Modes, mode) }
	var out []Q6Check
	ev, al, no := in.chaos[idx("everysec")], in.chaos[idx("always")], in.chaos[idx("none")]
	noneOver := ""
	if no != nil {
		noneOver = fmt.Sprintf("; `none`: %s", no.Overshoot)
	}
	if c := ev; c != nil {
		if c.boundOK {
			out = append(out, Q6Check{
				Hypothesis: fmt.Sprintf("`everysec`: overshoot at most the live replicas' grace plus one second of spend (%s)", m8USD(c.bound)),
				Observed: fmt.Sprintf("overshoot %s at the end against %s (%s grace bound + %s req/s × %s per reply)%s",
					c.Overshoot, m8USD(c.bound), m8USD(c.graceBound), num(m9BudgetRate(c.Summary)), m8USD(m9Reply(c.Summary)), noneOver),
				Held: held(c.overshoot <= c.bound),
			})
		}
		if p := c.restart; p != nil {
			other := p.Limited.OtherErrors + p.Budget.OtherErrors
			obs := fmt.Sprintf("main stream %s (%d errors), other streams %d errors other than 429 and 402 in `%s`", c.ErrorRate, p.Main.Errors, other, M9RestartPhase)
			var h string
			switch {
			case !c.logs:
				obs += "; the run kept no logs"
				h = "unresolved"
			case c.LoadingLines == 0:
				obs += "; no replica logged a `LOADING` error, so the path rests on the integration test"
				h = "not exercised"
			default:
				obs += fmt.Sprintf("; %d replica lines with a `LOADING` error", c.LoadingLines)
				h = held(p.Main.Errors == 0 && other == 0)
			}
			out = append(out, Q6Check{Hypothesis: "`everysec`: no client-visible errors while Redis answers `LOADING`", Observed: obs, Held: h})
			obs = fmt.Sprintf("%s of repeats after the restart (steady %s)", q6Pct(p.Main.RepeatHitRate), c.SteadyRepeatHitRate)
			if no != nil && no.restart != nil {
				obs += fmt.Sprintf("; `none`: %s", no.RepeatHitRate)
			}
			out = append(out, Q6Check{
				Hypothesis: "`everysec`: hit rate of repeats after the restart at 95% or more",
				Observed:   obs,
				Held:       held(p.Main.RepeatHitRate >= m9RepeatHitRate),
			})
		}
	}
	if obs, h, ok := m9CostCheck(in, idx("everysec"), []int{0, 1}, m9EverysecCostMS); ok {
		out = append(out, Q6Check{
			Hypothesis: "`everysec`: adds under 0.1 ms to p50 and p99 (non-streaming total and streaming TTFT, against `none`)",
			Observed:   obs, Held: h,
		})
	}
	if c := al; c != nil && c.boundOK {
		out = append(out, Q6Check{
			Hypothesis: fmt.Sprintf("`always`: overshoot at most the live replicas' grace (%s)", m8USD(c.graceBound)),
			Observed:   fmt.Sprintf("overshoot %s at the end against %s%s", c.Overshoot, m8USD(c.graceBound), noneOver),
			Held:       held(c.overshoot <= c.graceBound),
		})
	}
	if obs, h, ok := m9CostCheck(in, idx("always"), []int{1}, m9AlwaysCostMS); ok {
		out = append(out, Q6Check{
			Hypothesis: "`always`: adds under 0.2 ms at p99 (non-streaming total and streaming TTFT, against `none`)",
			Observed:   obs, Held: h,
		})
	}
	return out
}

// m9Rule applies PLAN.md's rule: in M9Modes order, the default is the
// first mode whose overshoot is at most the grace bound plus one second
// of spend and whose larger p99 cost is under 0.5 ms (an unresolved cost
// passes only if the drift is under 0.5 ms; none costs 0). If no mode
// passes, the default stays none. A mode not measured before the first
// pass leaves the rule undecided.
func m9Rule(in m9Inputs) (rows []M9RuleRow, def, reason string, decided, noPass bool) {
	blocked := ""
	for i, mode := range M9Modes {
		row := M9RuleRow{Mode: mode, Overshoot: "–", OvershootOK: "–", Cost: "–", CostOK: "–", Passes: "not measured"}
		c := in.chaos[i]
		measured := c != nil && c.boundOK
		overOK := false
		if measured {
			overOK = c.overshoot <= c.bound
			row.Overshoot = fmt.Sprintf("%s (bound %s)", c.Overshoot, m8USD(c.bound))
			row.OvershootOK = yesNo(overOK)
		}
		costOK := false
		if mode == "none" {
			row.Cost, row.CostOK, costOK = "0 (by definition)", "yes", true
		} else {
			costs := in.cost[i]
			ok := costs[m9Total][1] != nil && costs[m9TTFT][1] != nil && in.driftOK[m9Total] && in.driftOK[m9TTFT]
			if ok {
				costOK = true
				var parts []string
				for k := range 2 {
					v, d := *costs[k][1], in.drift[k][1]
					pass := v < m9RuleCostMS
					note := ""
					if math.Abs(v) <= d {
						pass, note = d < m9RuleCostMS, ", unresolved"
					}
					costOK = costOK && pass
					parts = append(parts, fmt.Sprintf("%s %s ms (drift %s%s)", m9Metric[k], added(v), msf(d), note))
				}
				row.Cost, row.CostOK = strings.Join(parts, "; "), yesNo(costOK)
			} else {
				measured = false
			}
		}
		if !measured {
			if !decided && blocked == "" {
				blocked = mode
			}
			rows = append(rows, row)
			continue
		}
		pass := overOK && costOK
		row.Passes = yesNo(pass)
		rows = append(rows, row)
		if pass && !decided && blocked == "" {
			def, decided = mode, true
			reason = fmt.Sprintf("`%s` is the first mode whose overshoot is within the bound and whose p99 cost passes", mode)
			if i > 0 {
				reason += "; every mode before it failed at least one of the two"
			}
		}
	}
	if !decided && blocked == "" {
		def, decided, noPass = "none", true, true
		reason = "no mode passes, so the default stays `none`"
	}
	if !decided {
		reason = fmt.Sprintf("undecided: `%s` was not measured", blocked)
	}
	return rows, def, reason, decided, noPass
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

// Complete reports whether every mode has both its chaos run and its
// bench run, so prose that compares the modes can be rendered.
func (m *M9) Complete() bool {
	if len(m.Runs) != len(M9Modes) {
		return false
	}
	for _, r := range m.Runs {
		if r == nil || r.Chaos == nil || r.Bench == nil {
			return false
		}
	}
	return true
}

// SlowClass is the row of Q6's slow stretch for an error class, nil if no
// replica logged that class.
func (m *M9) SlowClass(class string) *M9Class {
	for i := range m.Slow {
		if m.Slow[i].Class == class {
			return &m.Slow[i]
		}
	}
	return nil
}

// Run is the named mode's runs, nil if it was not measured.
func (m *M9) Run(mode string) *M9Run {
	for _, r := range m.Runs {
		if r != nil && r.Mode == mode {
			return r
		}
	}
	return nil
}

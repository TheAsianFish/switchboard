package results

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/chaos"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// M13Dir holds M13's runs: the flap schedule before and after the cache's
// single probe (before/, after/) and the Q6 schedule at two retry
// intervals, three runs each (retry-<interval>/run-<n>/).
const M13Dir = "m13-outage"

// M13Intervals are the retry intervals M13 compares, the default first.
var M13Intervals = []string{"1s", "250ms"}

// M13 is what makes an outage slow and what keeps the fleet degraded
// after it.
type M13 struct {
	Before, After *M13Arm
	// Bursts sets the two arms' bursts side by side by their order in an
	// outage.
	Bursts []M13BurstRow
	// Retry is one arm per interval of M13Intervals that has runs.
	Retry []*M13Retry
	// Rule is the decision on retry_interval's default, and Default the
	// default it leaves.
	Rule, Default string
	Checks        []M11Check
	Headline      string
	// RetrySum totals the retry arms' breaker and stale-error counts.
	RetrySum M13RetrySum
}

// M13RetrySum totals M13Retry's counts over both intervals.
type M13RetrySum struct {
	Runs, Tripped, Stale, StaleFailures, StaleDNS, PoolSize int
}

// M13Arm is one flap run, analysed by `switchboard chaos outage`.
type M13Arm struct {
	S        *chaos.OutageSummary
	Hardware string
	Rows     []M13OutageRow
	// MeanSlow is the slow requests per outage, MeanP99 the outage
	// phases' mean p99.
	MeanSlow, MeanOutageP99, MeanBackP99 string
	// SlowRequests and the component breakdown over the run.
	SlowByComponent string
	RepeatHitRate   string
	// Breaker is what the logs say about go-redis's dial breaker.
	Breaker M13Breaker
}

// M13OutageRow is one outage of a flap run.
type M13OutageRow struct {
	N                      int
	At                     string
	Slow                   int
	ByComponent            string
	Bursts                 int
	OutageP99, BackP99     string
	TimedOut               int
	FastReplicas           string
	FirstFast, DialsBefore string
}

// M13BurstRow is the k-th burst of every outage, both arms.
type M13BurstRow struct {
	Burst string
	// Per arm: the bursts found, their requests in total and per outage,
	// when they started (from the cut, range), and their components and
	// error classes.
	Before, After M13BurstCell
}

// M13BurstCell is one arm's k-th bursts.
type M13BurstCell struct {
	Outages     int
	Requests    int
	PerOutage   string
	Starts      string
	ByComponent string
	ByClass     string
	InFlight    int
	MedianWait  string
	// BeforeDone counts the requests whose longest store call started
	// before the cutting command returned; CallStarts is the range of
	// those calls' starts and Done the range of the commands' return
	// times, both from the command being issued.
	BeforeDone       int
	CallStarts, Done string
}

// M13Breaker summarises the per-replica dial counts against go-redis's
// breaker: PoolSize failed dials in a row and the pool answers every call
// with the last dial error at once, until a background dial succeeds.
type M13Breaker struct {
	PoolSize int
	// ReplicaOutages counts (replica, outage) pairs; Tripped those in
	// which the cache failed without waiting.
	ReplicaOutages, Tripped int
	// MinAtTrip and MaxAtTrip are the failed dials the replica had
	// accumulated (carried from earlier outages, plus this one's before
	// the first fast failure) when it first failed fast, over the trips
	// the count explains.
	MinAtTrip, MaxAtTrip int
	// Carried counts trips whose own outage's dials were below PoolSize
	// and whose carried count made up the rest. Late counts pairs that
	// reached PoolSize with no fast failure logged after (the breaker's
	// background dial resets the count once Redis is back, so nothing
	// carries from them). Restarted counts pairs whose process had been
	// restarted since the previous outage (the count starts at zero).
	// Unexplained lists the trips the count does not reach PoolSize for.
	Carried, Late, Restarted int
	Unexplained              []string
	// FirstFast is the range of first-fast times.
	FirstFast string
}

// M13Retry is one retry interval's runs of the Q6 schedule.
type M13Retry struct {
	Interval string
	Runs     []M13RetryRun
	// Ranges over the runs.
	AfterReady, Degraded, RepeatHitRate, TimedOut string
	// MeanAfterReady is the runs' mean degraded time after ready, s.
	MeanAfterReady string
	// Tripped counts runs in which some replica's cache failed without
	// waiting only after PoolSize failed dials (go-redis's breaker);
	// Stale counts runs whose replicas logged cache failures after Redis
	// was ready, StaleFailures and StaleDNS those failures and how many
	// were DNS errors.
	Tripped, Stale, StaleFailures, StaleDNS int
	poolSize                                int
	afterReady                              []float64
	timedOut                                int
}

// M13RetryRun is one run.
type M13RetryRun struct {
	Run                              string
	Commit, Date, Command, Hardware  string
	Ready, Degraded, AfterReady      string
	RepeatHitRate, BackRepeatHitRate string
	OutageSlow, TimedOut             int
	Recoveries                       string
}

func loadM13(resultsDir string) (*M13, error) {
	dir := filepath.Join(resultsDir, M13Dir)
	m := &M13{}
	var err error
	if m.Before, err = loadM13Arm(filepath.Join(dir, "before")); err != nil {
		return nil, err
	}
	if m.After, err = loadM13Arm(filepath.Join(dir, "after")); err != nil {
		return nil, err
	}
	for _, iv := range M13Intervals {
		r, err := loadM13Retry(filepath.Join(dir, "retry-"+iv), iv)
		if err != nil {
			return nil, err
		}
		if r != nil {
			m.Retry = append(m.Retry, r)
		}
	}
	if m.Before == nil && m.After == nil && len(m.Retry) == 0 {
		return nil, nil
	}
	m.Bursts = m13Bursts(m.Before, m.After)
	for _, r := range m.Retry {
		m.RetrySum.Runs += len(r.Runs)
		m.RetrySum.Tripped += r.Tripped
		m.RetrySum.Stale += r.Stale
		m.RetrySum.StaleFailures += r.StaleFailures
		m.RetrySum.StaleDNS += r.StaleDNS
		m.RetrySum.PoolSize = r.poolSize
	}
	m.Rule, m.Default = m13Rule(m.Retry)
	m.Checks = m13Checks(m)
	if m.Before != nil && m.After != nil {
		m.Headline = fmt.Sprintf("Slow requests in M8's flap schedule (eight 8 s Redis outages): %d before, %d of them cache lookups waiting out their 250 ms timeout; %d after one cache lookup per retry interval probes the store (%s → %s per outage); outage p99 %s → %s ms, which a slow 1%% still sets",
			m.Before.S.Run.SlowRequests, m.Before.S.Run.SlowByComponent[telemetry.StoreCacheLookup],
			m.After.S.Run.SlowRequests, m.Before.MeanSlow, m.After.MeanSlow, m.Before.MeanOutageP99, m.After.MeanOutageP99)
		if len(m.Retry) == len(M13Intervals) {
			m.Headline += fmt.Sprintf("; degraded after Redis is ready %s s at a 1 s retry interval, %s s at 250 ms", m.Retry[0].AfterReady, m.Retry[1].AfterReady)
		}
	}
	return m, nil
}

func loadM13Arm(dir string) (*M13Arm, error) {
	var s chaos.OutageSummary
	err := readJSON(filepath.Join(dir, chaos.OutageFile), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(s.Outages) == 0 {
		return nil, fmt.Errorf("results: m13: %s has no outages", dir)
	}
	a := &M13Arm{S: &s, Hardware: describeHardware(s.Hardware)}
	var slow, op99, bp99 float64
	for i, o := range s.Outages {
		row := M13OutageRow{N: i + 1, At: fmt.Sprintf("%.1f", o.AtSeconds), Slow: o.SlowRequests,
			ByComponent: m13Counts(o.SlowByComponent), Bursts: len(o.Bursts),
			OutageP99: msf(o.OutageP99MS), BackP99: msf(o.BackP99MS), TimedOut: o.TimedOut}
		fast := 0
		var firsts, dials []string
		for _, r := range o.Replicas {
			dials = append(dials, strconv.Itoa(r.DialsBefore))
			if r.FirstFastSeconds != nil {
				fast++
				firsts = append(firsts, fmt.Sprintf("%.2f", *r.FirstFastSeconds))
			} else {
				firsts = append(firsts, "–")
			}
		}
		row.FastReplicas = fmt.Sprintf("%d of %d", fast, len(o.Replicas))
		row.FirstFast, row.DialsBefore = strings.Join(firsts, " / "), strings.Join(dials, " / ")
		a.Rows = append(a.Rows, row)
		slow += float64(o.SlowRequests)
		op99 += o.OutageP99MS
		bp99 += o.BackP99MS
	}
	n := float64(len(s.Outages))
	a.MeanSlow = fmt.Sprintf("%.1f", slow/n)
	a.MeanOutageP99, a.MeanBackP99 = msf(op99/n), msf(bp99/n)
	a.SlowByComponent = m13Counts(s.Run.SlowByComponent)
	a.RepeatHitRate = pct1(s.Run.RepeatHitRate)
	a.Breaker = m13Breaker(&s)
	return a, nil
}

// m13PoolSize is go-redis's default PoolSize on the replicas (10 per
// GOMAXPROCS, 4 in every run here), which is also its dial breaker's
// threshold.
func m13PoolSize(s *chaos.OutageSummary) int {
	g := s.Hardware.GOMAXPROCS
	if g <= 0 {
		g = 4
	}
	return 10 * g
}

func m13Breaker(s *chaos.OutageSummary) M13Breaker {
	b := M13Breaker{PoolSize: m13PoolSize(s), MinAtTrip: math.MaxInt}
	carry := map[string]int{}
	lo, hi := math.Inf(1), math.Inf(-1)
	for i, o := range s.Outages {
		for _, r := range o.Replicas {
			b.ReplicaOutages++
			if r.Restarted {
				b.Restarted++
				carry[r.Replica] = 0
			}
			total := carry[r.Replica] + r.DialsBefore
			if r.FirstFastSeconds == nil {
				carry[r.Replica] = total
				if total >= b.PoolSize {
					// The breaker tripped too late for a fast failure
					// to show; its background dial starts the count
					// again once Redis is back.
					b.Late++
					carry[r.Replica] = 0
				}
				continue
			}
			b.Tripped++
			if total < b.PoolSize {
				b.Unexplained = append(b.Unexplained, fmt.Sprintf("%s in outage %d (%d carried + %d)", r.Replica, i+1, total-r.DialsBefore, r.DialsBefore))
				carry[r.Replica] = 0
				continue
			}
			if r.DialsBefore < b.PoolSize {
				b.Carried++
			}
			b.MinAtTrip, b.MaxAtTrip = min(b.MinAtTrip, total), max(b.MaxAtTrip, total)
			lo, hi = math.Min(lo, *r.FirstFastSeconds), math.Max(hi, *r.FirstFastSeconds)
			carry[r.Replica] = 0
		}
	}
	if b.Tripped == 0 {
		b.MinAtTrip, b.FirstFast = 0, "–"
	} else {
		b.FirstFast = fmt.Sprintf("%.2f–%.2f s", lo, hi)
	}
	return b
}

// m13OutageBursts are an outage's bursts from the cut on: a burst of
// requests scheduled before the cut (observed after it) has no place in
// the outage's order.
func m13OutageBursts(o chaos.RedisOutage) []chaos.Burst {
	var out []chaos.Burst
	for _, b := range o.Bursts {
		if b.FromSeconds >= 0 {
			out = append(out, b)
		}
	}
	return out
}

// m13Bursts sets the arms' k-th bursts side by side: the first three,
// then every later one together.
func m13Bursts(before, after *M13Arm) []M13BurstRow {
	if before == nil && after == nil {
		return nil
	}
	names := []string{"1st (the cut)", "2nd", "3rd", "4th and later"}
	cell := func(a *M13Arm, k int) M13BurstCell {
		var c M13BurstCell
		if a == nil {
			return c
		}
		comps, classes := map[string]int{}, map[string]int{}
		lo, hi := math.Inf(1), math.Inf(-1)
		clo, chi, dlo, dhi := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		var waits []float64
		for _, o := range a.S.Outages {
			found := false
			for j, b := range m13OutageBursts(o) {
				if j != k && (k != 3 || j <= 3) {
					continue
				}
				found = true
				c.Requests += b.Requests
				c.InFlight += b.InFlightAtCut
				c.BeforeDone += b.StartedBeforeDone
				if b.CallsFromSeconds != nil {
					clo, chi = math.Min(clo, *b.CallsFromSeconds), math.Max(chi, *b.CallsToSeconds)
				}
				dlo, dhi = math.Min(dlo, o.DoneSeconds), math.Max(dhi, o.DoneSeconds)
				lo, hi = math.Min(lo, b.FromSeconds), math.Max(hi, b.FromSeconds)
				for x, v := range b.ByComponent {
					comps[x] += v
				}
				for x, v := range b.ByClass {
					classes[x] += v
				}
				waits = append(waits, b.MedianWaitMS)
			}
			if found {
				c.Outages++
			}
		}
		c.PerOutage, c.Starts, c.MedianWait, c.CallStarts, c.Done = "–", "–", "–", "–", "–"
		if !math.IsInf(clo, 0) {
			c.CallStarts = fmt.Sprintf("+%.2f to +%.2f s", clo, chi)
		}
		if !math.IsInf(dlo, 0) {
			c.Done = fmt.Sprintf("+%.2f to +%.2f s", dlo, dhi)
		}
		if c.Outages > 0 {
			c.PerOutage = fmt.Sprintf("%.1f", float64(c.Requests)/float64(len(a.S.Outages)))
			c.Starts = fmt.Sprintf("+%.2f to +%.2f s", lo, hi)
			slices.Sort(waits)
			c.MedianWait = fmt.Sprintf("%.0f ms", waits[len(waits)/2])
		}
		c.ByComponent, c.ByClass = m13Counts(comps), m13Counts(classes)
		return c
	}
	var out []M13BurstRow
	for k, name := range names {
		out = append(out, M13BurstRow{Burst: name, Before: cell(before, k), After: cell(after, k)})
	}
	return out
}

func loadM13Retry(dir, interval string) (*M13Retry, error) {
	runs, err := filepath.Glob(filepath.Join(dir, "run-*"))
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	slices.Sort(runs)
	r := &M13Retry{Interval: interval}
	var degraded, hits []float64
	for _, d := range runs {
		var s chaos.Summary
		if err := readJSON(filepath.Join(d, chaos.SummaryFile), &s); err != nil {
			return nil, err
		}
		var o chaos.OutageSummary
		if err := readJSON(filepath.Join(d, chaos.OutageFile), &o); err != nil {
			return nil, fmt.Errorf("results: m13: %s: %w (run `switchboard chaos outage --dir %s`)", d, err, d)
		}
		if s.Meta.RetryInterval != interval {
			return nil, fmt.Errorf("results: m13: %s ran retry_interval %q, want %q", d, s.Meta.RetryInterval, interval)
		}
		if s.Store == nil || len(s.Store.Restarts) == 0 {
			return nil, fmt.Errorf("results: m13: %s has no Redis restart in its logs", d)
		}
		rs := s.Store.Restarts[0]
		run := M13RetryRun{Run: filepath.Base(d), Commit: s.Meta.Commit, Date: s.Meta.Date, Command: s.Meta.Command,
			Hardware: describeHardware(s.Hardware), Ready: "–", Degraded: "–", AfterReady: "–",
			RepeatHitRate: pct1(o.Run.RepeatHitRate)}
		if rs.ReadySeconds != nil {
			run.Ready = fmt.Sprintf("%.3f", *rs.ReadySeconds)
		}
		if rs.DegradedSeconds != nil {
			run.Degraded = fmt.Sprintf("%.3f", *rs.DegradedSeconds)
			degraded = append(degraded, *rs.DegradedSeconds)
			if rs.ReadySeconds != nil {
				v := *rs.DegradedSeconds - *rs.ReadySeconds
				run.AfterReady = fmt.Sprintf("%.3f", v)
				r.afterReady = append(r.afterReady, v)
			}
		}
		var parts []string
		for _, c := range rs.Recoveries {
			if c.Seconds != nil {
				parts = append(parts, fmt.Sprintf("%s %s %.2f", strings.TrimPrefix(c.Replica, "switchboard-"), c.Component, *c.Seconds))
			}
		}
		run.Recoveries = strings.Join(parts, ", ")
		for _, p := range s.Phases {
			if !p.RedisUp || p.FromSeconds < rs.AtSeconds {
				continue
			}
			if p.Main.RepeatOK > 0 && run.BackRepeatHitRate == "" {
				run.BackRepeatHitRate = pct1(p.Main.RepeatHitRate)
			}
		}
		if run.BackRepeatHitRate == "" {
			run.BackRepeatHitRate = "–"
		}
		for _, x := range o.Outages {
			run.OutageSlow += x.SlowRequests
			run.TimedOut += x.TimedOut
		}
		r.timedOut += run.TimedOut
		pool := m13PoolSize(&o)
		r.poolSize = pool
		tripped := false
		for _, x := range o.Outages {
			for _, rd := range x.Replicas {
				tripped = tripped || rd.FirstFastSeconds != nil && rd.DialsBefore >= pool
			}
		}
		if tripped {
			r.Tripped++
		}
		if len(o.AfterReady) > 0 && o.AfterReady[0].Failures > 0 {
			r.Stale++
			r.StaleFailures += o.AfterReady[0].Failures
			r.StaleDNS += o.AfterReady[0].ByClass[chaos.ErrClassDNS]
		}
		hits = append(hits, o.Run.RepeatHitRate)
		r.Runs = append(r.Runs, run)
	}
	r.AfterReady = m13Range(r.afterReady, "%.2f")
	if len(r.afterReady) > 0 {
		r.MeanAfterReady = fmt.Sprintf("%.2f", meanOf(r.afterReady))
	}
	r.Degraded = m13Range(degraded, "%.2f")
	var hp []float64
	for _, h := range hits {
		hp = append(hp, 100*h)
	}
	r.RepeatHitRate = m13Range(hp, "%.1f") + "%"
	var to []float64
	for _, run := range r.Runs {
		to = append(to, float64(run.TimedOut))
	}
	r.TimedOut = m13Range(to, "%.0f")
	return r, nil
}

// m13Rule applies PLAN.md's rule: the default moves to 250 ms only if the
// three-run ranges of degraded time after ready do not overlap and the
// outage phases show no more store calls timing out than at 1 s.
func m13Rule(retry []*M13Retry) (string, string) {
	if len(retry) != len(M13Intervals) || len(retry[0].afterReady) < 3 || len(retry[1].afterReady) < 3 {
		return "Not decided: the rule needs three runs at each interval.", "1s"
	}
	slow, fast := retry[0], retry[1]
	sep := slices.Max(fast.afterReady) < slices.Min(slow.afterReady)
	fewer := fast.timedOut <= slow.timedOut
	switch {
	case sep && fewer:
		return fmt.Sprintf("The ranges do not overlap (%s s at 250 ms against %s s at 1 s) and the outages timed out %d store calls at 250 ms against %d at 1 s: the default moves to 250 ms.",
			fast.AfterReady, slow.AfterReady, fast.timedOut, slow.timedOut), "250ms"
	case !sep:
		return fmt.Sprintf("The ranges overlap (%s s at 250 ms against %s s at 1 s), and the outages timed out %d store calls at 250 ms against %d at 1 s: the default stays 1 s.",
			fast.AfterReady, slow.AfterReady, fast.timedOut, slow.timedOut), "1s"
	default:
		return fmt.Sprintf("The ranges do not overlap (%s s at 250 ms against %s s at 1 s), but the outages timed out %d store calls at 250 ms against %d at 1 s: the default stays 1 s.",
			fast.AfterReady, slow.AfterReady, fast.timedOut, slow.timedOut), "1s"
	}
}

func m13Checks(m *M13) []M11Check {
	var out []M11Check
	b, a := m.Before, m.After
	if b != nil {
		cut, later := m.Bursts[0].Before, M13BurstCell{}
		for _, row := range m.Bursts[1:3] {
			later.Requests += row.Before.Requests
		}
		lookups := 0
		for _, o := range b.S.Outages {
			for j, br := range m13OutageBursts(o) {
				if j == 1 || j == 2 {
					lookups += br.ByComponent[telemetry.StoreCacheLookup]
				}
			}
		}
		out = append(out, M11Check{
			Hypothesis: "The bursts at +1.5 s and +3 s are cache lookups",
			Measured:   fmt.Sprintf("%d of their %d slow requests waited longest on a cache lookup", lookups, later.Requests),
			Held:       yesNo(later.Requests > 0 && lookups == later.Requests),
		})
		held := "not resolved"
		switch {
		case cut.Requests == 0:
			held = "no"
		case 2*cut.InFlight > cut.Requests:
			held = "yes"
		case 2*cut.BeforeDone < cut.Requests:
			held = "no"
		}
		out = append(out, M11Check{
			Hypothesis: "The burst at the cut is calls already in flight",
			Measured: fmt.Sprintf("of its %d slow requests, %d had their store call start before the disconnect command was issued, %d after it was issued but before it returned (%s), when the cut may or may not have happened yet, and %d after it returned, after the cut for certain",
				cut.Requests, cut.InFlight, cut.BeforeDone-cut.InFlight, cut.Done, cut.Requests-cut.BeforeDone),
			Held: held,
		})
	}
	if b != nil && a != nil {
		perOutage, _ := strconv.ParseFloat(a.MeanSlow, 64)
		out = append(out, M11Check{
			Hypothesis: "Slow requests per outage fall from about 108 to at most 25",
			Measured:   fmt.Sprintf("%s → %s (before arm %d in the run, M8's 865)", b.MeanSlow, a.MeanSlow, b.S.Run.SlowRequests),
			Held:       yesNo(perOutage <= 25),
		})
		out = append(out, M11Check{
			Hypothesis: "Slow requests in the run fall from 865 to at most 200",
			Measured:   fmt.Sprintf("%d → %d", b.S.Run.SlowRequests, a.S.Run.SlowRequests),
			Held:       yesNo(a.S.Run.SlowRequests <= 200),
		})
		out = append(out, M11Check{
			Hypothesis: "The outage phases' p99 may not move (reported either way)",
			Measured:   fmt.Sprintf("mean outage p99 %s → %s ms, mean back p99 %s → %s ms", b.MeanOutageP99, a.MeanOutageP99, b.MeanBackP99, a.MeanBackP99),
			Held:       "reported",
		})
		bb := b.Breaker
		out = append(out, M11Check{
			Hypothesis: "Three bursts, not eight, because go-redis answers from its last dial error after its third failed dial",
			Measured: fmt.Sprintf("before: in %d of %d replica-outages the cache first failed fast after %d to %d failed dials, %s after the cut (go-redis's breaker answers from its last dial error once PoolSize, %d, dials have failed)",
				bb.Tripped, bb.ReplicaOutages, bb.MinAtTrip, bb.MaxAtTrip, bb.FirstFast, bb.PoolSize),
			Held: yesNo(bb.Tripped > 0 && bb.MaxAtTrip <= 3),
		})
	}
	if len(m.Retry) == len(M13Intervals) {
		slow, fast := m.Retry[0], m.Retry[1]
		ok := len(fast.afterReady) > 0 && len(slow.afterReady) > 0 && slices.Max(fast.afterReady) < slices.Min(slow.afterReady)
		out = append(out, M11Check{
			Hypothesis: "Degraded time after Redis is ready falls from about 2.0 s to about 1.25 s at 250 ms",
			Measured:   fmt.Sprintf("%s s at 1 s (mean %s), %s s at 250 ms (mean %s), three runs each", slow.AfterReady, slow.MeanAfterReady, fast.AfterReady, fast.MeanAfterReady),
			Held:       yesNo(ok && meanOf(fast.afterReady) <= 1.25+0.25),
		})
	}
	return out
}

func meanOf(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func m13Range(xs []float64, format string) string {
	if len(xs) == 0 {
		return "–"
	}
	lo, hi := slices.Min(xs), slices.Max(xs)
	if fmt.Sprintf(format, lo) == fmt.Sprintf(format, hi) {
		return fmt.Sprintf(format, lo)
	}
	return fmt.Sprintf(format, lo) + "–" + fmt.Sprintf(format, hi)
}

// m13Counts formats a count map, largest first, components named in
// words.
func m13Counts(m map[string]int) string {
	if len(m) == 0 {
		return "–"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if m[a] != m[b] {
			return m[b] - m[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", strings.ReplaceAll(k, "_", " "), m[k]))
	}
	return strings.Join(parts, ", ")
}

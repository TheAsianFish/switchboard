package chaos

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/bench"
)

// M13 (PLAN.md): what makes an outage slow. Every replica logs each store
// call that waited 50 ms or longer ("store call slow", with its component,
// operation, wait, error and the request's id), and the load generator
// sends each request's id, so every slow request of a run can be joined to
// the store calls that made it slow.

// OutageFile is the M13 analysis of a run, beside summary.json.
const OutageFile = "outage.json"

// SlowLineMS is the line a request must be slower than to count as slow:
// Q6's committed steady-state limit (twice the steady phase's p99 in
// eval/results/q6-chaos/), fixed in PLAN.md M13 so every run is measured
// against the same line rather than against its own steady phase.
const SlowLineMS = 363.52

// BurstGapSeconds splits an outage's slow requests into bursts: a slow
// request scheduled more than this after the previous one starts a new
// burst.
const BurstGapSeconds = 0.4

// Components of a slow request, the store component it waited on most.
// ComponentNone is a slow request with no slow store call logged.
const (
	ComponentNone = "none"
)

// slowStoreMsg and cacheFailMsg are the gateway's messages for a slow
// store call and for a cache store failure.
const (
	slowStoreMsg = "store call slow"
	cacheFailMsg = "cache store failed; bypassing the cache"
)

// StoreCall is one slow store call from a replica's log.
type StoreCall struct {
	Replica   string `json:"replica"`
	Component string `json:"component"`
	Op        string `json:"op"`
	// AtSeconds is when the call ended (the line's time), StartSeconds
	// when it began (AtSeconds minus the wait), both from the run's start.
	AtSeconds    float64 `json:"at_seconds"`
	StartSeconds float64 `json:"start_seconds"`
	WaitMS       float64 `json:"wait_ms"`
	// Class is the error's class (ClassifyStoreError), "ok" for a call
	// that was slow but succeeded.
	Class     string `json:"class"`
	RequestID string `json:"request_id,omitempty"`
}

// CacheFailure is one cache store failure from a replica's log: the line
// the cache writes each time a lookup or fill fails and turns it off for
// the retry interval. Fast failures count here, where the slow-call lines
// do not show them.
type CacheFailure struct {
	Replica   string  `json:"replica"`
	Op        string  `json:"op"`
	AtSeconds float64 `json:"at_seconds"`
	// WaitMS is the failed call's wait; nil for builds before M13, which
	// did not log it.
	WaitMS *float64 `json:"wait_ms,omitempty"`
	Class  string   `json:"class"`
}

// OutageSummary is `switchboard chaos outage`'s output.
type OutageSummary struct {
	Meta     Meta           `json:"meta"`
	Hardware any            `json:"hardware"`
	LineMS   float64        `json:"line_ms"`
	GapS     float64        `json:"burst_gap_seconds"`
	Run      RunSlow        `json:"run"`
	Outages  []RedisOutage  `json:"outages"`
	Restarts []RedisRestart `json:"restarts,omitempty"`
}

// RunSlow counts the whole run.
type RunSlow struct {
	// Requests and SlowRequests are the main stream's successful requests
	// and those slower than the line.
	Requests     int `json:"requests"`
	SlowRequests int `json:"slow_requests"`
	// SlowByComponent attributes the slow requests (see RedisOutage).
	SlowByComponent map[string]int `json:"slow_by_component"`
	// StoreCalls counts every slow store call any replica logged, by
	// component; TimedOut those that failed on a timeout.
	StoreCalls         map[string]int `json:"slow_store_calls"`
	StoreCallsTimedOut int            `json:"slow_store_calls_timed_out"`
	// RepeatOK and RepeatHits are the main stream's successful repeats
	// and those the cache served, over the whole run.
	RepeatOK      int     `json:"repeat_ok"`
	RepeatHits    int     `json:"repeat_hits"`
	RepeatHitRate float64 `json:"repeat_hit_rate"`
}

// RedisOutage is one stretch from Redis going away (a kill or a disconnect)
// to the next time it goes away, or the run's end: the outage and the
// reachable stretch after it, since requests slowed by the outage can
// finish after Redis is back.
type RedisOutage struct {
	Event     string  `json:"event"`
	AtSeconds float64 `json:"at_seconds"`
	// BackSeconds is when Redis came back (start or connect) from the
	// event; ToSeconds the end of the stretch.
	BackSeconds *float64 `json:"back_seconds,omitempty"`
	ToSeconds   float64  `json:"to_seconds"`
	// P99MS is the main stream's p99 over the phases of the outage itself
	// (event to back) and of the reachable stretch after it.
	OutageP99MS float64 `json:"outage_p99_ms"`
	BackP99MS   float64 `json:"back_p99_ms"`
	// SlowRequests are the main stream's successful requests slower than
	// the line whose outcome was observed in the stretch; SlowByComponent
	// attributes each to the store component it waited on longest
	// (ComponentNone if it logged no slow store call).
	SlowRequests    int            `json:"slow_requests"`
	SlowByComponent map[string]int `json:"slow_by_component"`
	Bursts          []Burst        `json:"bursts"`
	// CacheRounds are the cache's store failures, every replica's, in
	// rounds: a failure more than BurstGapSeconds after the previous one
	// starts a new round.
	CacheRounds []CacheRound `json:"cache_rounds"`
	// PoolLines counts go-redis's "failed to dial" lines in the stretch.
	PoolLines int `json:"pool_lines"`
}

// Burst is a run of slow requests no more than BurstGapSeconds apart by
// scheduled time.
type Burst struct {
	// FromSeconds and ToSeconds are the first and last request's
	// scheduled time, from the outage's event.
	FromSeconds float64        `json:"from_seconds"`
	ToSeconds   float64        `json:"to_seconds"`
	Requests    int            `json:"requests"`
	ByComponent map[string]int `json:"by_component"`
	// ByClass counts the burst's requests by the class of the longest
	// store call each waited on.
	ByClass map[string]int `json:"by_class"`
	// InFlightAtCut counts requests whose longest store call started
	// before the outage's event: calls already in flight when Redis went.
	InFlightAtCut int `json:"in_flight_at_cut"`
	// Replicas counts the distinct replicas the burst's store calls came
	// from.
	Replicas int `json:"replicas"`
	// MedianWaitMS is the median of each request's longest store wait.
	MedianWaitMS float64 `json:"median_wait_ms"`
}

// CacheRound is a round of cache store failures.
type CacheRound struct {
	FromSeconds float64 `json:"from_seconds"`
	ToSeconds   float64 `json:"to_seconds"`
	Failures    int     `json:"failures"`
	// SlowFailures counts failures that waited at least 50 ms (nil waits,
	// from builds before M13, are not counted).
	SlowFailures int            `json:"slow_failures"`
	MaxWaitMS    float64        `json:"max_wait_ms"`
	ByClass      map[string]int `json:"by_class"`
}

// AnalyseOutage computes the M13 analysis of a run.
func AnalyseOutage(in Input) (*OutageSummary, error) {
	main := in.Streams[StreamMain]
	if main == nil {
		return nil, fmt.Errorf("chaos outage: no main stream")
	}
	if len(in.Logs) == 0 {
		return nil, fmt.Errorf("chaos outage: the run kept no logs")
	}
	// The phases, events and restarts come from the Q6 analysis.
	s, err := Analyse(in, DefaultRule)
	if err != nil {
		return nil, err
	}
	start := main.Result.StartedAt
	end := main.Result.Config.DurationSec
	calls, fails, pool := parseStoreLines(in.Logs, start)
	byID := map[string][]StoreCall{}
	for _, c := range calls {
		if c.RequestID != "" {
			byID[c.RequestID] = append(byID[c.RequestID], c)
		}
	}
	out := &OutageSummary{Meta: in.Meta, Hardware: main.Result.Hardware, LineMS: SlowLineMS, GapS: BurstGapSeconds}
	if s.Store != nil {
		out.Restarts = s.Store.Restarts
	}

	// Each slow request with its longest store call.
	type slowReq struct {
		at, observed float64
		longest      *StoreCall
	}
	var slow []slowReq
	out.Run.SlowByComponent = map[string]int{}
	for _, r := range main.Records {
		if r.Warmup || r.Outcome != bench.OutcomeOK || r.LatencyMS == nil {
			continue
		}
		out.Run.Requests++
		if r.Pool != nil {
			out.Run.RepeatOK++
			if isHit(r.Cache) {
				out.Run.RepeatHits++
			}
		}
		if *r.LatencyMS <= SlowLineMS {
			continue
		}
		sr := slowReq{at: r.At, observed: Observed(r)}
		for i, c := range byID[bench.RequestID(StreamMain, r.I)] {
			if sr.longest == nil || c.WaitMS > sr.longest.WaitMS {
				sr.longest = &byID[bench.RequestID(StreamMain, r.I)][i]
			}
		}
		slow = append(slow, sr)
		out.Run.SlowRequests++
		out.Run.SlowByComponent[componentOf(sr.longest)]++
	}
	if out.Run.RepeatOK > 0 {
		out.Run.RepeatHitRate = round6(float64(out.Run.RepeatHits) / float64(out.Run.RepeatOK))
	}
	out.Run.StoreCalls = map[string]int{}
	for _, c := range calls {
		out.Run.StoreCalls[c.Component]++
		if c.Class == ErrClassTimeout || c.Class == ErrClassDialTimeout {
			out.Run.StoreCallsTimedOut++
		}
	}

	// Redis going away, and coming back.
	type redisEvent struct {
		name string
		at   float64
		down bool
	}
	var evs []redisEvent
	for _, e := range s.Events {
		if e.Target != RedisTarget {
			continue
		}
		down := e.Action == ActionKill || e.Action == ActionDisconnect
		evs = append(evs, redisEvent{name: e.Name, at: e.AtSeconds, down: down})
	}
	for i, e := range evs {
		if !e.down {
			continue
		}
		o := RedisOutage{Event: e.name, AtSeconds: e.at, ToSeconds: end, SlowByComponent: map[string]int{}}
		for _, f := range evs[i+1:] {
			if f.down {
				o.ToSeconds = f.at
				break
			}
			if o.BackSeconds == nil {
				v := round3(f.at - e.at)
				o.BackSeconds = &v
			}
		}
		// The p99 of the phase the event starts, and of the phase that
		// starts when Redis is back.
		if pi := phaseAt(s.Phases, e.at); pi >= 0 {
			o.OutageP99MS = s.Phases[pi].Main.Latency.P99
		}
		if o.BackSeconds != nil {
			if pi := phaseAt(s.Phases, e.at+*o.BackSeconds); pi >= 0 {
				o.BackP99MS = s.Phases[pi].Main.Latency.P99
			}
		}
		var in []slowReq
		for _, sr := range slow {
			if sr.observed >= e.at && sr.observed < o.ToSeconds {
				in = append(in, sr)
			}
		}
		slices.SortStableFunc(in, func(a, b slowReq) int { return cmpFloat(a.at, b.at) })
		o.SlowRequests = len(in)
		var cur *Burst
		var waits []float64
		replicas := map[string]bool{}
		flush := func() {
			if cur == nil {
				return
			}
			cur.MedianWaitMS = round3(median(waits))
			cur.Replicas = len(replicas)
			o.Bursts = append(o.Bursts, *cur)
		}
		last := math.Inf(-1)
		for _, sr := range in {
			rel := sr.at - e.at
			if cur == nil || rel-last > BurstGapSeconds {
				flush()
				cur = &Burst{FromSeconds: round3(rel), ByComponent: map[string]int{}, ByClass: map[string]int{}}
				waits = nil
				replicas = map[string]bool{}
			}
			last = rel
			cur.ToSeconds = round3(rel)
			cur.Requests++
			comp := componentOf(sr.longest)
			cur.ByComponent[comp]++
			o.SlowByComponent[comp]++
			if sr.longest != nil {
				cur.ByClass[sr.longest.Class]++
				waits = append(waits, sr.longest.WaitMS)
				replicas[sr.longest.Replica] = true
				if sr.longest.StartSeconds < e.at {
					cur.InFlightAtCut++
				}
			}
		}
		flush()
		o.CacheRounds = cacheRounds(fails, e.at, o.ToSeconds)
		for _, p := range pool {
			if p >= e.at && p < o.ToSeconds {
				o.PoolLines++
			}
		}
		out.Outages = append(out.Outages, o)
	}
	return out, nil
}

// componentOf names the component of a slow request's longest store call.
func componentOf(c *StoreCall) string {
	if c == nil {
		return ComponentNone
	}
	return c.Component
}

// phaseAt is the index of the phase that starts at t (to the
// millisecond), or -1.
func phaseAt(ps []PhaseResult, t float64) int {
	for i, p := range ps {
		if math.Abs(p.FromSeconds-t) < 0.0015 {
			return i
		}
	}
	return -1
}

// cacheRounds groups the cache failures in [from, to) into rounds, with
// times from from.
func cacheRounds(fails []CacheFailure, from, to float64) []CacheRound {
	var out []CacheRound
	var cur *CacheRound
	last := math.Inf(-1)
	for _, f := range fails {
		if f.AtSeconds < from || f.AtSeconds >= to {
			continue
		}
		rel := f.AtSeconds - from
		if cur == nil || rel-last > BurstGapSeconds {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &CacheRound{FromSeconds: round3(rel), ByClass: map[string]int{}}
		}
		last = rel
		cur.ToSeconds = round3(rel)
		cur.Failures++
		cur.ByClass[f.Class]++
		if f.WaitMS != nil {
			if *f.WaitMS >= 50 {
				cur.SlowFailures++
			}
			cur.MaxWaitMS = max(cur.MaxWaitMS, *f.WaitMS)
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// storeLine is the part of a gateway's JSON log line the M13 analysis
// reads.
type storeLine struct {
	Msg       string   `json:"msg"`
	Component string   `json:"component"`
	Op        string   `json:"op"`
	WaitMS    *float64 `json:"wait_ms"`
	Error     string   `json:"error"`
	RequestID string   `json:"request_id"`
}

// parseStoreLines reads every replica's slow store calls, cache failures
// and go-redis pool lines (their times), each sorted by time.
func parseStoreLines(logs map[string][]LogLine, start interface{ UnixNano() int64 }) ([]StoreCall, []CacheFailure, []float64) {
	t0 := float64(start.UnixNano()) / 1e9
	var calls []StoreCall
	var fails []CacheFailure
	var pool []float64
	for replica, lines := range logs {
		if replica == RedisTarget || !strings.HasPrefix(replica, "switchboard") {
			continue
		}
		for _, l := range lines {
			at := l.Unix - t0
			if strings.HasPrefix(l.Text, goRedisPrefix) {
				pool = append(pool, at)
				continue
			}
			if !strings.HasPrefix(l.Text, "{") {
				continue
			}
			var sl storeLine
			if json.Unmarshal([]byte(l.Text), &sl) != nil {
				continue
			}
			class := "ok"
			if sl.Error != "" {
				class = ClassifyStoreError(sl.Error)
			}
			switch sl.Msg {
			case slowStoreMsg:
				if sl.WaitMS == nil {
					continue
				}
				calls = append(calls, StoreCall{
					Replica: replica, Component: sl.Component, Op: sl.Op,
					AtSeconds: round3(at), StartSeconds: round3(at - *sl.WaitMS/1000), WaitMS: *sl.WaitMS,
					Class: class, RequestID: sl.RequestID,
				})
			case cacheFailMsg:
				fails = append(fails, CacheFailure{Replica: replica, Op: sl.Op, AtSeconds: round3(at), WaitMS: sl.WaitMS, Class: class})
			}
		}
	}
	slices.SortStableFunc(calls, func(a, b StoreCall) int { return cmpFloat(a.AtSeconds, b.AtSeconds) })
	slices.SortStableFunc(fails, func(a, b CacheFailure) int { return cmpFloat(a.AtSeconds, b.AtSeconds) })
	slices.Sort(pool)
	return calls, fails, pool
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// DescribeOutage is a short text report of the analysis.
func DescribeOutage(o *OutageSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "chaos outage: %d slow requests (over %.2f ms) of %d; by component %s; repeats hit %.1f%%\n",
		o.Run.SlowRequests, o.LineMS, o.Run.Requests, counts(o.Run.SlowByComponent), 100*o.Run.RepeatHitRate)
	for _, x := range o.Outages {
		fmt.Fprintf(&b, "  %s at %.1f s: %d slow (%s), p99 %.1f / %.1f ms\n", x.Event, x.AtSeconds, x.SlowRequests,
			counts(x.SlowByComponent), x.OutageP99MS, x.BackP99MS)
		for _, br := range x.Bursts {
			fmt.Fprintf(&b, "    +%.2f..%.2f s: %d (%s; %s), %d in flight at the cut, median wait %.0f ms\n",
				br.FromSeconds, br.ToSeconds, br.Requests, counts(br.ByComponent), counts(br.ByClass), br.InFlightAtCut, br.MedianWaitMS)
		}
	}
	return b.String()
}

// counts formats a count map in key order.
func counts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+strconv.Itoa(m[k]))
	}
	return strings.Join(parts, ", ")
}

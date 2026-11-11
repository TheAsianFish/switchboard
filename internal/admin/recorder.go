package admin

import (
	"math"
	"slices"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Defaults for a Recorder.
const (
	// DefaultRecent is how many calls a replica keeps for the page.
	DefaultRecent = 200
	// DefaultWindow is the span of the per-replica rate and latency
	// figures.
	DefaultWindow = 10 * time.Second
	// maxWindowSamples caps the window's memory at very high rates; the
	// oldest samples go first, so the figures then cover less than the
	// window.
	maxWindowSamples = 50_000
)

// Recorder keeps one replica's view of its own calls: the most recent
// ones, a short window of latencies for rates and percentiles, and running
// totals. Observe is the pipeline's Policy.OnCall; it takes one mutex and
// appends, so it costs the request path well under a microsecond.
type Recorder struct {
	now        func() time.Time
	candidates []string
	window     time.Duration
	started    time.Time

	mu     sync.Mutex
	recent []Call // ring buffer of the last cap(recent) calls
	next   int    // ring position of the next write
	full   bool
	// samples are the window's calls, oldest first, from samples[head:].
	samples []sample
	head    int
	// dropped is the newest sample the cap dropped while still inside the
	// window; the window's figures cover only the time after it.
	dropped time.Time

	requests  int64
	outcomes  map[string]int64
	spent     cost.Nanos
	keys      map[string]*keyTotals
	lookups   map[string]int64
	saved     cost.Nanos
	decisions map[[2]string]int64
	degraded  map[string]bool
}

type sample struct {
	at       time.Time
	duration time.Duration
	// timed is false for a refusal, which has no measured duration.
	timed bool
	err   bool
	// lookup is 1 for a cache hit, 0 for a miss, -1 when the cache was
	// not consulted.
	lookup int8
}

// keyTotals are one key's running totals on one replica.
type keyTotals struct {
	Calls      int64 `json:"calls"`
	SpentNanos int64 `json:"spent_nanos"`
}

// RecorderOptions configure a Recorder. Zero fields take the defaults.
type RecorderOptions struct {
	// Recent is how many calls to keep.
	Recent int
	// Window is the span of the rate and latency figures.
	Window time.Duration
	// Candidates names the router's candidates in its weights' order, so
	// a decision's probabilities can be labelled.
	Candidates []string
	Now        func() time.Time
}

// NewRecorder returns an empty recorder.
func NewRecorder(o RecorderOptions) *Recorder {
	if o.Recent <= 0 {
		o.Recent = DefaultRecent
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Recorder{
		now: o.Now, candidates: slices.Clone(o.Candidates), window: o.Window, started: o.Now(),
		recent:    make([]Call, o.Recent),
		outcomes:  map[string]int64{},
		keys:      map[string]*keyTotals{},
		lookups:   map[string]int64{},
		decisions: map[[2]string]int64{},
		degraded:  map[string]bool{},
	}
}

// Observe records one finished call.
func (r *Recorder) Observe(c pipeline.Call) {
	call := r.call(c)
	s := sample{at: c.Time, duration: c.Duration, timed: c.Admitted, err: c.Outcome == telemetry.OutcomeError, lookup: -1}
	switch c.Cache {
	case cache.StatusExact, cache.StatusSemantic:
		s.lookup = 1
	case cache.StatusMiss:
		s.lookup = 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.recent[r.next] = call
	r.next = (r.next + 1) % len(r.recent)
	r.full = r.full || r.next == 0
	r.samples = append(r.samples, s)
	r.trim(c.Time)

	r.requests++
	r.outcomes[c.Outcome]++
	r.spent += c.Spent
	kt := r.keys[c.Key]
	if kt == nil {
		kt = &keyTotals{}
		r.keys[c.Key] = kt
	}
	kt.Calls++
	kt.SpentNanos += int64(c.Spent)
	if c.Cache != "" {
		r.lookups[string(c.Cache)]++
	}
	r.saved += c.Saved
	if c.Router != nil {
		r.decisions[[2]string{c.Router.Model, c.Router.Reason}]++
	}
}

// call converts a pipeline call to its API form.
func (r *Recorder) call(c pipeline.Call) Call {
	out := Call{
		Time: c.Time.UTC(), RequestID: c.RequestID, TraceID: c.TraceID, Key: c.Key,
		RequestedModel: clip(c.RequestedModel, maxModelLen), Model: c.Model, Provider: c.Provider, Stream: c.Stream,
		Outcome: c.Outcome, ErrorKind: c.ErrorKind, Cache: string(c.Cache),
		DurationMS: ms(c.Duration), TTFTMS: ms(c.TTFT),
		InputTokens: c.Usage.InputTokens, OutputTokens: c.Usage.OutputTokens,
		CostUSD: c.Spent.USD(), Attempts: c.Attempts, Hedged: c.Hedged,
	}
	if d := c.Router; d != nil {
		out.RouterReason = d.Reason
		for i, p := range d.Probs {
			if i < len(r.candidates) {
				if out.RouterProbs == nil {
					out.RouterProbs = make(map[string]float64, len(d.Probs))
				}
				out.RouterProbs[r.candidates[i]] = p
			}
		}
	}
	return out
}

// maxModelLen bounds the requested model kept per call. A request for a
// model that is not configured is refused before admission, so no limit
// applies to it, and its name is the client's own text: unbounded, 200 of
// them near the 10 MiB body cap would hold gigabytes in the ring and in
// every report published to Redis.
const maxModelLen = 128

// clip shortens s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// trim drops window samples older than the window before now, and the
// oldest beyond the cap. Called with mu held.
func (r *Recorder) trim(now time.Time) {
	cutoff := now.Add(-r.window)
	for r.head < len(r.samples) && (r.samples[r.head].at.Before(cutoff) || len(r.samples)-r.head > maxWindowSamples) {
		if at := r.samples[r.head].at; !at.Before(cutoff) {
			r.dropped = at
		}
		r.head++
	}
	if r.head > len(r.samples)/2 && r.head > 1024 {
		r.samples = append(r.samples[:0], r.samples[r.head:]...)
		r.head = 0
	}
}

// SetDegraded records whether component runs on its per-replica fallback.
// It is the degrade policies' OnState hook.
func (r *Recorder) SetDegraded(component string, degraded bool) {
	r.mu.Lock()
	r.degraded[component] = degraded
	r.mu.Unlock()
}

// report is what a replica publishes about itself: its Replica row and
// the totals the fleet view sums.
type report struct {
	Replica
	Keys       map[string]keyTotals `json:"keys"`
	Lookups    map[string]int64     `json:"lookups"`
	SavedNanos int64                `json:"saved_nanos"`
	SpentNanos int64                `json:"spent_nanos"`
	Decisions  []Decision           `json:"decisions"`
}

// snapshot copies the recorder's state into a report and returns the
// recent calls, newest first. The identity fields (ID, version, times,
// providers) are the caller's to fill.
func (r *Recorder) snapshot() (report, []Call) {
	now := r.now()
	r.mu.Lock()
	r.trim(now)
	durations := make([]float64, 0, len(r.samples)-r.head)
	w := Window{Requests: int64(len(r.samples) - r.head)}
	var hits, lookups int64
	for _, s := range r.samples[r.head:] {
		if s.timed {
			durations = append(durations, ms(s.duration))
		}
		if s.err {
			w.Errors++
		}
		if s.lookup >= 0 {
			lookups++
			hits += int64(s.lookup)
		}
	}
	rep := report{
		Replica: Replica{
			Requests: r.requests, Outcomes: copyMap(r.outcomes), SpentUSD: r.spent.USD(),
			Degraded: map[string]bool{},
		},
		Keys:       make(map[string]keyTotals, len(r.keys)),
		Lookups:    copyMap(r.lookups),
		SavedNanos: int64(r.saved),
		SpentNanos: int64(r.spent),
	}
	for k, v := range r.degraded {
		rep.Degraded[k] = v
	}
	for k, v := range r.keys {
		rep.Keys[k] = *v
	}
	for k, n := range r.decisions {
		rep.Decisions = append(rep.Decisions, Decision{Model: k[0], Reason: k[1], Count: n})
	}
	recent := make([]Call, 0, len(r.recent))
	n := r.next
	if r.full {
		n = len(r.recent)
	}
	for i := 1; i <= n; i++ {
		recent = append(recent, r.recent[(r.next-i+len(r.recent))%len(r.recent)])
	}
	dropped := r.dropped
	r.mu.Unlock()

	sortDecisions(rep.Decisions)
	// The window covers the last Window, or less on a replica that
	// started more recently or whose sample cap dropped part of it.
	from := now.Add(-r.window)
	if r.started.After(from) {
		from = r.started
	}
	if dropped.After(from) {
		from = dropped
	}
	w.Seconds = max(now.Sub(from), time.Second).Seconds()
	w.RPS = float64(w.Requests) / w.Seconds
	if w.Requests > 0 {
		w.ErrorRate = float64(w.Errors) / float64(w.Requests)
	}
	if len(durations) > 0 {
		sort.Float64s(durations)
		w.P50MS, w.P99MS = quantile(durations, 0.50), quantile(durations, 0.99)
	}
	if lookups > 0 {
		w.CacheHitRate = float64(hits) / float64(lookups)
	}
	rep.Window = w
	return rep, recent
}

// quantile is the nearest-rank quantile of sorted xs.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func sortDecisions(ds []Decision) {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].Count != ds[j].Count {
			return ds[i].Count > ds[j].Count
		}
		if ds[i].Model != ds[j].Model {
			return ds[i].Model < ds[j].Model
		}
		return ds[i].Reason < ds[j].Reason
	})
}

func copyMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

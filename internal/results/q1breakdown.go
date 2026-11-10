package results

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/bench"
)

// Q1BreakdownDir is the subdirectory of Q1Dir holding the per-feature
// breakdown runs.
const Q1BreakdownDir = "breakdown"

// Q1Configs are the breakdown configurations in the order q1.sh runs and
// stacks them: each switches on its own feature on top of the ones before.
var Q1Configs = []string{"none", "limits", "budget", "exact", "tracing", "semantic", "router"}

// q1Resamples is the number of bootstrap resamples per interval, and
// q1Seed fixes them so a re-render is byte-stable.
const (
	q1Resamples = 1000
	q1Seed      = 20261110
)

// BreakdownMeta is meta.json's description of the breakdown part of a run.
type BreakdownMeta struct {
	// Rate is the arrival rate of the configs that do not embed; EmbedRate
	// that of the configs that do (semantic cache, router).
	Rate      float64 `json:"rate"`
	EmbedRate float64 `json:"embed_rate"`
	Duration  string  `json:"duration"`
	Warmup    string  `json:"warmup"`
	// CacheDirective is the X-Switchboard-Cache value every request sent,
	// "" if the generator could not send one.
	CacheDirective string `json:"cache_directive"`
	// Redis and Jaeger are the container images used, "" if none was.
	Redis  string      `json:"redis"`
	Jaeger string      `json:"jaeger"`
	Ollama *OllamaMeta `json:"ollama"`
	// Configs lists every configuration, measured or not.
	Configs []ConfigMeta `json:"configs"`
}

// OllamaMeta describes the embedder the semantic and router configs used.
type OllamaMeta struct {
	// Source is "local" (OLLAMA_URL) or "docker <image>".
	Source     string `json:"source"`
	Version    string `json:"version"`
	EmbedModel string `json:"embed_model"`
	// CapacityRPS is the embeddings per second Ollama served in q1.sh's
	// probe: CapacityRequests short prompts, CapacityConcurrency at a time.
	CapacityRPS         float64 `json:"capacity_rps"`
	CapacityRequests    int     `json:"capacity_requests"`
	CapacityConcurrency int     `json:"capacity_concurrency"`
}

// ConfigMeta is one breakdown configuration.
type ConfigMeta struct {
	Name string `json:"name"`
	// Features are the features switched on, cumulative.
	Features []string `json:"features"`
	Rate     float64  `json:"rate"`
	Model    string   `json:"model"`
	// Status is "measured", "skipped" (Reason says why) or "not selected".
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// GatewayRun is the <run>.gateway.json file q1.sh writes beside each
// gateway run: the gateway process's resource use over the measured window
// and a probe request sent before the load.
type GatewayRun struct {
	Config        string  `json:"config"`
	WindowSeconds float64 `json:"window_seconds"`
	// CPUSeconds is user plus system time from /proc/<pid>/stat over the
	// window; PeakRSSKB is VmHWM from /proc/<pid>/status at the end of the
	// run (the process starts fresh for every run).
	CPUSeconds float64 `json:"gateway_cpu_seconds"`
	PeakRSSKB  int64   `json:"gateway_peak_rss_kb"`
	// RedisCPUSeconds and OllamaCPUSeconds are the containers' cgroup CPU
	// time over the same window, nil when not measured.
	RedisCPUSeconds  *float64 `json:"redis_cpu_seconds"`
	OllamaCPUSeconds *float64 `json:"ollama_cpu_seconds"`
	Probe            Probe    `json:"probe"`
}

// Probe is what the gateway said about one request sent before the load.
type Probe struct {
	HTTPStatus    int    `json:"http_status"`
	CacheStatus   string `json:"cache_status"`
	TraceID       string `json:"trace_id"`
	TraceInJaeger *bool  `json:"trace_in_jaeger"`
	Model         string `json:"model"`
}

// Q1Breakdown is the per-feature part of the Q1 section.
type Q1Breakdown struct {
	Meta BreakdownMeta
	// Rate and EmbedRate are BreakdownMeta's rates for prose.
	Rate, EmbedRate string
	Windows         []string
	// MaxSendDelayP99 is the worst p99 send delay of any breakdown run, ms.
	MaxSendDelayP99 string
	Rows            []BreakdownRow
	Steps           []StepRow
	Baselines       []BaselineRow
	Checks          []CheckRow
	// Reading is prose generated from the numbers above.
	Reading   []string
	Resamples int

	machines []string
}

// Est is a difference in ms with its bootstrap 95% interval.
type Est struct {
	ok        bool
	v, lo, hi float64
}

// String is "+0.52 [+0.31, +0.70]", or "n/a".
func (e Est) String() string {
	if !e.ok {
		return "n/a"
	}
	return fmt.Sprintf("%s [%s, %s]", added(e.v), added(e.lo), added(e.hi))
}

// BreakdownRow is one configuration's overhead against the direct baseline
// at its rate: total latency for non-streamed requests, TTFT for streamed.
type BreakdownRow struct {
	Config, Features, Rate, Model string
	TotalP50, TotalP99            Est
	TTFTP50, TTFTP99              Est
}

// StepRow is what one configuration adds over the one before it.
type StepRow struct {
	Stage, Rate string
	// Across names the earlier config when it ran at another rate, so the
	// step subtracts two overheads measured against different baselines.
	Across             string
	TotalP50, TotalP99 Est
	TTFTP50, TTFTP99   Est
}

// BaselineRow shows the direct runs at one rate and mode.
type BaselineRow struct {
	Rate, Mode, Metric string
	// Runs are "p50 / p99" per direct run in order.
	Runs []string
	// SpreadP50 and SpreadP99 are the largest minus the smallest run.
	SpreadP50, SpreadP99 string
	PooledP50, PooledP99 string
	Samples              int
}

// CheckRow is what one gateway run did, from the gateway's own counters
// and the probe, and what it cost in CPU and memory.
type CheckRow struct {
	Config, Mode, Rate string
	// Cores is gateway CPU seconds over wall seconds in the measured
	// window; RSS its peak resident set in MB.
	Cores, RSS              string
	RedisCores, OllamaCores string
	Requests, Errors        string
	Served, Cache, Router   string
	// Layers is each cache layer's mean time per lookup and lookup count,
	// from switchboard_cache_lookup_seconds.
	Layers                 string
	ProbeCache, ProbeModel string
	ProbeTrace             string
	cacheBad               string
}

// bkRun is one parsed breakdown result file.
type bkRun struct {
	name   string // file name without .json
	config string // a Q1Configs name, or "direct"
	stream bool
	rate   float64
	rateS  string
	seq    int // direct runs: 1, 2, ...
	res    bench.Result
	gw     *GatewayRun
	met    metrics
}

// hardware lists the machines the breakdown runs reported; nil-safe.
func (q *Q1Breakdown) hardware() []string {
	if q == nil {
		return nil
	}
	return q.machines
}

// loadBreakdown reads dir. Without breakdown result files it returns nil.
func loadBreakdown(dir string, meta *BreakdownMeta) (*Q1Breakdown, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	var runs []*bkRun
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".gateway.json") {
			continue
		}
		r, err := loadBreakdownRun(dir, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	if len(runs) == 0 {
		return nil, nil
	}
	if meta == nil {
		return nil, fmt.Errorf("results: %s has runs but meta.json has no breakdown", dir)
	}
	return buildBreakdown(*meta, runs)
}

// loadBreakdownRun reads <name>.json, named <config>-<mode>-<rate> or
// direct-<mode>-<rate>-<n>, and its .gateway.json and .metrics.txt.
func loadBreakdownRun(dir, name string) (*bkRun, error) {
	path := filepath.Join(dir, name+".json")
	parts := strings.Split(name, "-")
	r := &bkRun{name: name}
	bad := fmt.Errorf("results: %s: name is not <config>-<nonstream|stream>-<rate>.json or direct-<mode>-<rate>-<n>.json", path)
	switch {
	case len(parts) == 4 && parts[0] == "direct":
		n, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, bad
		}
		r.seq = n
	case len(parts) == 3 && slices.Contains(Q1Configs, parts[0]):
	default:
		return nil, bad
	}
	if parts[1] != "nonstream" && parts[1] != "stream" {
		return nil, bad
	}
	r.config, r.stream, r.rateS = parts[0], parts[1] == "stream", parts[2]
	rate, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return nil, fmt.Errorf("results: %s: bad rate in name: %w", path, err)
	}
	r.rate = rate
	if err := readJSON(path, &r.res); err != nil {
		return nil, err
	}
	if r.res.Config.Stream != r.stream || r.res.Config.Rate != rate {
		return nil, fmt.Errorf("results: %s: name says stream=%v rate=%v, file says stream=%v rate=%v",
			path, r.stream, rate, r.res.Config.Stream, r.res.Config.Rate)
	}
	if r.res.Samples == nil {
		return nil, fmt.Errorf("results: %s: no samples (q1.sh runs the breakdown with --samples)", path)
	}
	if r.config == "direct" {
		return r, nil
	}
	gw, err := loadGatewayRun(filepath.Join(dir, name+".gateway.json"))
	if err != nil {
		return nil, err
	}
	r.gw = gw
	met, err := loadMetrics(filepath.Join(dir, name+".metrics.txt"))
	if err != nil {
		return nil, err
	}
	r.met = met
	return r, nil
}

// loadGatewayRun reads a sidecar; a missing one gives nil.
func loadGatewayRun(path string) (*GatewayRun, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var g GatewayRun
	if err := readJSON(path, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// metrics are the gateway's counters after a run: series name and sorted
// label pairs ("k=v,k=v") to value.
type metrics map[string]map[string]float64

// loadMetrics parses the Prometheus text q1.sh saved; a missing file
// gives nil. Only plain `name{labels} value` lines are expected.
func loadMetrics(path string) (metrics, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	m := metrics{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			return nil, fmt.Errorf("results: %s: bad line %q", path, line)
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			return nil, fmt.Errorf("results: %s: bad value in %q", path, line)
		}
		series, labels := line[:i], ""
		if j := strings.IndexByte(series, '{'); j >= 0 {
			labels = strings.TrimSuffix(series[j+1:], "}")
			series = series[:j]
		}
		var pairs []string
		for _, p := range splitLabels(labels) {
			k, val, _ := strings.Cut(p, "=")
			pairs = append(pairs, k+"="+strings.Trim(val, `"`))
		}
		slices.Sort(pairs)
		if m[series] == nil {
			m[series] = map[string]float64{}
		}
		m[series][strings.Join(pairs, ",")] += v
	}
	return m, nil
}

// splitLabels splits `a="x",b="y"` at the commas between pairs.
func splitLabels(s string) []string {
	var out []string
	quoted, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// by sums a series by one label: label value to count.
func (m metrics) by(series string, label ...string) map[string]float64 {
	out := map[string]float64{}
	for labels, v := range m[series] {
		var key []string
		for _, want := range label {
			for _, p := range strings.Split(labels, ",") {
				if k, val, _ := strings.Cut(p, "="); k == want {
					key = append(key, val)
				}
			}
		}
		out[strings.Join(key, " ")] += v
	}
	return out
}

// counts renders a label-to-count map as "a 3, b 2", largest first.
func counts(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if m[a] != m[b] {
			if m[a] > m[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %s", k, num(m[k]))
	}
	return strings.Join(parts, ", ")
}

// boot is one sample set with its point percentiles and bootstrap
// replicates of them.
type boot struct {
	p50, p99 float64
	b50, b99 []float64
	n        int
}

// newBoot computes the nearest-rank p50 and p99 of xs and q1Resamples
// bootstrap replicates of each, seeded by name. Resampling n values with
// replacement from sorted xs and taking the k-th smallest is the same as
// taking xs at the k-th smallest of n uniform indices, so each replicate
// counts index draws instead of sorting.
func newBoot(name string, xs []float64) *boot {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	b := &boot{n: n}
	if n == 0 {
		return b
	}
	k50, k99 := rank(50, n), rank(99, n)
	b.p50, b.p99 = s[k50-1], s[k99-1]
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	rng := rand.New(rand.NewPCG(q1Seed, h.Sum64()))
	cnt := make([]int32, n)
	b.b50, b.b99 = make([]float64, q1Resamples), make([]float64, q1Resamples)
	for r := range q1Resamples {
		clear(cnt)
		for range n {
			cnt[rng.IntN(n)]++
		}
		seen, got50 := 0, false
		for i, c := range cnt {
			seen += int(c)
			if !got50 && seen >= k50 {
				b.b50[r], got50 = s[i], true
			}
			if seen >= k99 {
				b.b99[r] = s[i]
				break
			}
		}
	}
	return b
}

// rank is the 1-based nearest-rank index of the p-th percentile of n
// values, as bench.Percentile computes it.
func rank(p float64, n int) int {
	return min(max(int(math.Ceil(p/100*float64(n)-1e-9)), 1), n)
}

// diff is a - b, point and per replicate; nil inputs give a zero diff.
type diff struct {
	ok       bool
	v50, v99 float64
	r50, r99 []float64
}

func sub(a, b *boot) diff {
	if a == nil || b == nil || a.n == 0 || b.n == 0 {
		return diff{}
	}
	d := diff{ok: true, v50: a.p50 - b.p50, v99: a.p99 - b.p99,
		r50: make([]float64, q1Resamples), r99: make([]float64, q1Resamples)}
	for i := range q1Resamples {
		d.r50[i], d.r99[i] = a.b50[i]-b.b50[i], a.b99[i]-b.b99[i]
	}
	return d
}

func (d diff) minus(e diff) diff {
	if !d.ok || !e.ok {
		return diff{}
	}
	out := diff{ok: true, v50: d.v50 - e.v50, v99: d.v99 - e.v99,
		r50: make([]float64, q1Resamples), r99: make([]float64, q1Resamples)}
	for i := range q1Resamples {
		out.r50[i], out.r99[i] = d.r50[i]-e.r50[i], d.r99[i]-e.r99[i]
	}
	return out
}

// est turns a point and its replicates into a 95% percentile interval.
func est(ok bool, v float64, reps []float64) Est {
	if !ok {
		return Est{}
	}
	s := slices.Clone(reps)
	slices.Sort(s)
	return Est{ok: true, v: v, lo: s[rank(2.5, len(s))-1], hi: s[rank(97.5, len(s))-1]}
}

func (d diff) ests() (p50, p99 Est) {
	return est(d.ok, d.v50, d.r50), est(d.ok, d.v99, d.r99)
}

// groupKey is a direct baseline: one rate and mode.
type groupKey struct {
	rate   float64
	stream bool
}

func modeName(stream bool) string {
	if stream {
		return "stream"
	}
	return "nonstream"
}

func pick(r *bkRun) []float64 {
	if r.stream {
		return r.res.Samples.TTFTMS
	}
	return r.res.Samples.LatencyMS
}

func buildBreakdown(meta BreakdownMeta, runs []*bkRun) (*Q1Breakdown, error) {
	q := &Q1Breakdown{
		Meta: meta, Rate: num(meta.Rate), EmbedRate: num(meta.EmbedRate),
		Resamples: q1Resamples,
	}
	windows, machines := map[string]bool{}, map[string]bool{}
	var maxDelay float64
	directs := map[groupKey][]*bkRun{}
	gws := map[string]*bkRun{} // "<config>-<mode>"
	for _, r := range runs {
		windows[fmt.Sprintf("%s s after %s s warm-up", num(r.res.Config.DurationSec), num(r.res.Config.WarmupSec))] = true
		maxDelay = max(maxDelay, r.res.SendDelay.P99)
		machines[describeHardware(r.res.Hardware)] = true
		if r.config == "direct" {
			k := groupKey{r.rate, r.stream}
			directs[k] = append(directs[k], r)
			continue
		}
		key := r.config + "-" + modeName(r.stream)
		if prev, dup := gws[key]; dup {
			return nil, fmt.Errorf("results: breakdown has %s and %s for one config and mode", prev.name, r.name)
		}
		gws[key] = r
	}
	q.Windows, q.machines = sortedKeys(windows), sortedKeys(machines)
	q.MaxSendDelayP99 = fmt.Sprintf("%.3f", maxDelay)

	// Pooled baselines and their rows, by rate then mode.
	pooled := map[groupKey]*boot{}
	gkeys := make([]groupKey, 0, len(directs))
	for k := range directs {
		gkeys = append(gkeys, k)
	}
	slices.SortFunc(gkeys, func(a, b groupKey) int {
		if a.rate != b.rate {
			if a.rate > b.rate {
				return -1
			}
			return 1
		}
		if a.stream == b.stream {
			return 0
		}
		if a.stream {
			return 1
		}
		return -1
	})
	var maxSpread50, maxSpread99 float64
	for _, k := range gkeys {
		rs := directs[k]
		slices.SortFunc(rs, func(a, b *bkRun) int { return a.seq - b.seq })
		var all []float64
		row := BaselineRow{Rate: num(k.rate), Mode: "non-streaming", Metric: "total"}
		if k.stream {
			row.Mode, row.Metric = "streaming", "TTFT"
		}
		lo50, hi50, lo99, hi99 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		for _, r := range rs {
			xs := pick(r)
			all = append(all, xs...)
			b := newBoot(r.name, xs)
			row.Runs = append(row.Runs, msf(b.p50)+" / "+msf(b.p99))
			lo50, hi50 = min(lo50, b.p50), max(hi50, b.p50)
			lo99, hi99 = min(lo99, b.p99), max(hi99, b.p99)
		}
		p := newBoot(fmt.Sprintf("direct-%s-%s-pooled", modeName(k.stream), num(k.rate)), all)
		pooled[k] = p
		row.PooledP50, row.PooledP99, row.Samples = msf(p.p50), msf(p.p99), p.n
		row.SpreadP50, row.SpreadP99 = "n/a", "n/a"
		if len(rs) > 1 {
			row.SpreadP50, row.SpreadP99 = msf(hi50-lo50), msf(hi99-lo99)
			maxSpread50, maxSpread99 = max(maxSpread50, hi50-lo50), max(maxSpread99, hi99-lo99)
		}
		q.Baselines = append(q.Baselines, row)
	}

	// Overheads and steps in config order.
	type over struct{ total, ttft diff }
	byConfig := map[string]ConfigMeta{}
	for _, c := range meta.Configs {
		byConfig[c.Name] = c
	}
	var prev string
	prevOver := over{}
	prevRate := 0.0
	var steps []stepNote
	var cacheNotes, ollamaNotes []string
	for _, name := range Q1Configs {
		ns, st := gws[name+"-nonstream"], gws[name+"-stream"]
		if ns == nil && st == nil {
			continue
		}
		cm := byConfig[name]
		rate := cm.Rate
		if ns != nil {
			rate = ns.rate
		} else if st != nil {
			rate = st.rate
		}
		var o over
		if ns != nil {
			o.total = sub(newBoot(ns.name, pick(ns)), pooled[groupKey{rate, false}])
		}
		if st != nil {
			o.ttft = sub(newBoot(st.name, pick(st)), pooled[groupKey{rate, true}])
		}
		row := BreakdownRow{Config: name, Features: strings.Join(cm.Features, ", "), Rate: num(rate), Model: cm.Model}
		if row.Features == "" {
			row.Features = "none (passthrough)"
		}
		row.TotalP50, row.TotalP99 = o.total.ests()
		row.TTFTP50, row.TTFTP99 = o.ttft.ests()
		q.Rows = append(q.Rows, row)

		step := StepRow{Stage: "+ " + name, Rate: num(rate)}
		s := o
		if prev == "" {
			step.Stage = name + " (against direct)"
		} else {
			s = over{o.total.minus(prevOver.total), o.ttft.minus(prevOver.ttft)}
			if prevRate != rate {
				step.Across = fmt.Sprintf("%s at %s req/s", prev, num(prevRate))
			}
		}
		step.TotalP50, step.TotalP99 = s.total.ests()
		step.TTFTP50, step.TTFTP99 = s.ttft.ests()
		q.Steps = append(q.Steps, step)
		steps = append(steps, stepNote{step.Stage, step.TotalP50})
		prev, prevOver, prevRate = name, o, rate

		for _, r := range []*bkRun{ns, st} {
			if r == nil {
				continue
			}
			c := checkRow(r, cm.Features)
			if g := r.gw; g != nil && g.OllamaCPUSeconds != nil && r.rate > 0 && g.WindowSeconds > 0 {
				// Requests in the window are the rate times its length.
				perReq := *g.OllamaCPUSeconds / (r.rate * g.WindowSeconds) * 1000
				ollamaNotes = append(ollamaNotes, fmt.Sprintf("`%s` %s %.0f ms", name, modeName(r.stream), perReq))
			}
			if c.cacheBad != "" {
				cacheNotes = append(cacheNotes, fmt.Sprintf("`%s` %s: %s", name, modeName(r.stream), c.cacheBad))
			}
			q.Checks = append(q.Checks, c)
		}
	}
	q.Reading = reading(q, steps, maxSpread50, maxSpread99, cacheNotes)
	if len(ollamaNotes) > 0 {
		// Ollama's CPU per request tells how many embeddings a request
		// cost: about the same for semantic and router means one each.
		q.Reading = append(q.Reading, "Ollama CPU per request (container CPU over the window divided by rate times window): "+
			strings.Join(ollamaNotes, ", ")+".")
	}
	return q, nil
}

// stepNote is a step's p50 for the generated prose.
type stepNote struct {
	stage string
	p50   Est
}

func reading(q *Q1Breakdown, steps []stepNote, spread50, spread99 float64, cacheNotes []string) []string {
	var out []string
	if len(q.Baselines) > 0 {
		out = append(out, fmt.Sprintf("Between the two direct runs at one rate and mode, p50 moved by up to %s ms and p99 by up to %s ms; a step smaller than that is within the baseline's own drift, whatever its interval says.",
			msf(spread50), msf(spread99)))
	}
	var above, within []string
	best, bestV := "", math.Inf(-1)
	for _, s := range steps {
		if !s.p50.ok {
			continue
		}
		if s.p50.lo > 0 {
			above = append(above, fmt.Sprintf("%s (%s ms)", s.stage, added(s.p50.v)))
		} else {
			within = append(within, s.stage)
		}
		if s.p50.v > bestV {
			best, bestV = s.stage, s.p50.v
		}
	}
	if best != "" {
		out = append(out, fmt.Sprintf("The largest step at p50 (non-streaming total) is %s: %s ms.", best, added(bestV)))
	}
	if len(above) > 0 {
		out = append(out, "Steps whose 95% interval at p50 (non-streaming total) lies above zero: "+strings.Join(above, ", ")+".")
	}
	if len(within) > 0 {
		out = append(out, "Steps whose interval at p50 (non-streaming total) includes zero or lies below it, so not distinguishable from noise in this run: "+strings.Join(within, ", ")+".")
	}
	if len(cacheNotes) > 0 {
		out = append(out, "Cache lookups other than clean misses: "+strings.Join(cacheNotes, "; ")+".")
	}
	return out
}

func checkRow(r *bkRun, features []string) CheckRow {
	const na = "n/a"
	c := CheckRow{Config: r.config, Mode: "non-streaming", Rate: r.rateS,
		Cores: na, RSS: na, RedisCores: na, OllamaCores: na, Served: na, Cache: "-", Router: "-", Layers: "-",
		ProbeCache: "-", ProbeModel: "-", ProbeTrace: "-"}
	if r.stream {
		c.Mode = "streaming"
	}
	c.Requests, c.Errors = strconv.Itoa(r.res.Requests), strconv.Itoa(r.res.Errors())
	if g := r.gw; g != nil {
		c.Cores, c.RSS = cores(g.CPUSeconds, g.WindowSeconds), mb(g.PeakRSSKB)
		if g.RedisCPUSeconds != nil {
			c.RedisCores = cores(*g.RedisCPUSeconds, g.WindowSeconds)
		}
		if g.OllamaCPUSeconds != nil {
			c.OllamaCores = cores(*g.OllamaCPUSeconds, g.WindowSeconds)
		}
		p := g.Probe
		if p.CacheStatus != "" {
			c.ProbeCache = "`" + p.CacheStatus + "`"
		}
		if p.Model != "" {
			c.ProbeModel = "`" + p.Model + "`"
		}
		switch {
		case p.TraceInJaeger != nil && *p.TraceInJaeger:
			c.ProbeTrace = "in Jaeger"
		case p.TraceInJaeger != nil:
			c.ProbeTrace = "id, not in Jaeger"
		case p.TraceID != "":
			c.ProbeTrace = "id only"
		}
		if p.HTTPStatus != 200 {
			c.ProbeCache += fmt.Sprintf(" (HTTP %d)", p.HTTPStatus)
		}
	}
	if r.met != nil {
		c.Served = counts(r.met.by("switchboard_requests_total", "outcome"))
		lookups := r.met.by("switchboard_cache_lookups_total", "result")
		errs := r.met.by("switchboard_cache_errors_total", "op")
		if len(lookups) > 0 || len(errs) > 0 {
			c.Cache = counts(lookups)
			if len(errs) > 0 {
				c.Cache += "; errors: " + counts(errs)
			}
		}
		var bad []string
		for k, v := range lookups {
			if k != "miss" && v > 0 {
				bad = append(bad, fmt.Sprintf("%s %s", k, num(v)))
			}
		}
		for k, v := range errs {
			bad = append(bad, fmt.Sprintf("%s errors %s", k, num(v)))
		}
		n := r.met.by("switchboard_cache_lookup_seconds_count", "layer")
		sum := r.met.by("switchboard_cache_lookup_seconds_sum", "layer")
		var layers []string
		for _, l := range []string{"exact", "semantic"} {
			if n[l] > 0 {
				layers = append(layers, fmt.Sprintf("%s %.2f ms (%s)", l, sum[l]/n[l]*1000, num(n[l])))
			}
		}
		if len(layers) > 0 {
			c.Layers = strings.Join(layers, ", ")
		}
		// After a failed embedding the gateway bypasses the semantic layer
		// for a while; those lookups still count as exact misses.
		var total float64
		for _, v := range lookups {
			total += v
		}
		if slices.Contains(features, "semantic") && n["semantic"] < total {
			bad = append(bad, fmt.Sprintf("semantic layer ran for %s of %s lookups", num(n["semantic"]), num(total)))
		}
		slices.Sort(bad)
		c.cacheBad = strings.Join(bad, ", ")
		routed := r.met.by("switchboard_router_decisions_total", "model", "reason")
		if len(routed) > 0 {
			c.Router = strings.ReplaceAll(counts(routed), " target ", " ")
		}
	}
	return c
}

func cores(cpu, wall float64) string {
	if wall <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", cpu/wall)
}

func mb(kb int64) string {
	if kb <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", float64(kb)/1024)
}

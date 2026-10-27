package results

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/cacheeval"
)

// Q3Dir is the subdirectory written by `make eval-cache`.
const Q3Dir = "q3-cache"

// gridFloor is the lowest threshold the sweep tries; a threshold there
// may be met lower down too.
const gridFloor = 0.5

// Q3 is the semantic cache section. Sweep and Index are nil when their
// files are missing; Q3 itself is nil when both are.
type Q3 struct {
	Sweep *Q3Sweep
	Index *Q3Index
}

// Q3Sweep is the threshold sweep from summary.json.
type Q3Sweep struct {
	Target string // "1%", as few decimals as needed
	Step   string // "0.0025"
	// FineFrom and FineStep describe the finer grid at the top, e.g.
	// "0.99" and "0.0001".
	FineFrom, FineStep string
	Models             []string
	Data               cacheeval.DataMeta
	Sets               []Q3Set
	Rows               []Q3Row
	Baseline           []Q3Baseline
	Joints             []Q3Joint
	Latency            []Q3Latency
	Spreads            []Q3Spread
	Chosen             cacheeval.Chosen
	// ChosenThreshold is Chosen.Threshold to four decimals.
	ChosenThreshold string
	// MinNegatives is the fewest negatives with no false hit whose Wilson
	// upper bound is at most the target.
	MinNegatives int
	// AtFloor is true when some threshold shown is the grid's lowest.
	AtFloor                         bool
	Commit, Date, Command, Hardware string
}

// Q3Spread is where one pair set's cosines fall under one model, each
// cell "p10 / median / p90 (max)" to three decimals.
type Q3Spread struct {
	Model, Set, Positives, Negatives string
}

// Q3Set is one pair set's size.
type Q3Set struct {
	Name                 string
	Positives, Negatives int
}

// Q3Row is one curve's operating points.
type Q3Row struct {
	Model, Set, Guard    string
	Positives, Negatives int
	// Point is the lowest threshold with observed false-hit rate at most
	// the target, Upper the lowest with its Wilson upper bound at most it;
	// "none" when the sweep has none. The rates are "—" then.
	Point, PointHit, PointFalse string
	Upper, UpperHit, UpperFalse string
}

// Q3Baseline is the exact-match cache on one set.
type Q3Baseline struct {
	Set                                      string
	ExactHit, ExactFalse, NormHit, NormFalse string
}

// Q3Joint is one threshold for every set.
type Q3Joint struct {
	Model, Guard, Threshold string
	// PerSet is "set hit rate" for each set, joined with "; ".
	PerSet, Mean string
}

// Q3Latency is one model's single-prompt embedding latency in ms.
type Q3Latency struct {
	Model          string
	Dims, Calls    int
	Warmups        int
	P50, P99, Mean string
}

// Q3Index is the FLAT against HNSW check from the index*.json files.
type Q3Index struct {
	Rows                           []Q3IndexRow
	Redis, Vectors, Commits, Dates []string
	Hardware                       []string
	Queries                        []int
}

// Q3IndexRow is one algorithm on one data set.
type Q3IndexRow struct {
	Algorithm, Params            string
	Entries, Dims, Contexts      int
	Build                        string
	UnfilteredP50, UnfilteredP99 string
	FilteredP50, FilteredP99     string
	Recall, FilteredRecall       string
	SizeMB                       string
}

func loadQ3(dir string) (*Q3, error) {
	q := &Q3{}
	var s cacheeval.Summary
	switch err := readJSON(filepath.Join(dir, cacheeval.SummaryFile), &s); {
	case err == nil:
		q.Sweep = buildQ3Sweep(s)
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}
	idx, err := loadQ3Index(dir)
	if err != nil {
		return nil, err
	}
	q.Index = idx
	if q.Sweep == nil && q.Index == nil {
		return nil, nil
	}
	return q, nil
}

func buildQ3Sweep(s cacheeval.Summary) *Q3Sweep {
	q := &Q3Sweep{
		Target: strconv.FormatFloat(100*s.Target, 'f', -1, 64) + "%", Step: strconv.FormatFloat(s.Step, 'f', -1, 64),
		FineFrom: strconv.FormatFloat(s.FineFrom, 'f', -1, 64), FineStep: strconv.FormatFloat(s.FineStep, 'f', -1, 64),
		Models: s.Models, Data: s.Data, Chosen: s.Chosen,
		ChosenThreshold: threshold(s.Chosen.Threshold),
		MinNegatives:    minNegatives(s.Target),
		Commit:          s.Commit, Command: s.Command, Hardware: describeHardware(s.Hardware),
	}
	if len(s.Date) >= 10 {
		q.Date = s.Date[:10]
	}
	seen := map[string]bool{}
	for _, c := range s.Curves {
		if !seen[c.Set] {
			seen[c.Set] = true
			q.Sets = append(q.Sets, Q3Set{Name: c.Set, Positives: c.Positives, Negatives: c.Negatives})
		}
		row := Q3Row{Model: c.Model, Set: c.Set, Guard: onOff(c.Guard), Positives: c.Positives, Negatives: c.Negatives}
		row.Point, row.PointHit, row.PointFalse = operating(c.AtPoint)
		row.Upper, row.UpperHit, row.UpperFalse = operating(c.AtUpper)
		q.AtFloor = q.AtFloor || atFloor(c.AtPoint) || atFloor(c.AtUpper)
		q.Rows = append(q.Rows, row)
	}
	for _, b := range s.Baselines {
		q.Baseline = append(q.Baseline, Q3Baseline{
			Set: b.Set, ExactHit: rate(b.ExactHitRate), ExactFalse: rate(b.ExactFalseHitRate),
			NormHit: rate(b.NormalisedHitRate), NormFalse: rate(b.NormalisedFalseHitRate),
		})
	}
	for _, j := range s.Joints {
		row := Q3Joint{Model: j.Model, Guard: onOff(j.Guard), Threshold: "none", PerSet: "—", Mean: "—"}
		if j.Found {
			row.Threshold, row.Mean = threshold(j.Threshold), pct1(j.MeanHitRate)
			q.AtFloor = q.AtFloor || j.Threshold == gridFloor
			var parts []string
			for i, p := range j.PerSet {
				name := strconv.Itoa(i)
				if i < len(s.Sets) {
					name = s.Sets[i]
				}
				parts = append(parts, "`"+name+"` "+rate(p.HitRate))
			}
			row.PerSet = strings.Join(parts, "; ")
		}
		q.Joints = append(q.Joints, row)
	}
	for _, sp := range s.Spreads {
		q.Spreads = append(q.Spreads, Q3Spread{Model: sp.Model, Set: sp.Set, Positives: spread(sp.Positives), Negatives: spread(sp.Negatives)})
	}
	for _, l := range s.Latency {
		q.Latency = append(q.Latency, Q3Latency{
			Model: l.Model, Dims: l.Dims, Calls: l.Calls, Warmups: l.Warmups,
			P50: msf(l.P50Ms), P99: msf(l.P99Ms), Mean: msf(l.MeanMs),
		})
	}
	return q
}

func loadQ3Index(dir string) (*Q3Index, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "index*.json"))
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	if len(paths) == 0 {
		if _, err := os.Stat(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("results: %w", err)
		}
		return nil, nil
	}
	var results []cacheeval.IndexResult
	for _, p := range paths {
		var r cacheeval.IndexResult
		if err := readJSON(p, &r); err != nil {
			return nil, err
		}
		if len(r.Runs) == 0 {
			return nil, fmt.Errorf("results: %s is not an index result", p)
		}
		results = append(results, r)
	}
	slices.SortStableFunc(results, func(a, b cacheeval.IndexResult) int {
		if a.Dims != b.Dims {
			return a.Dims - b.Dims
		}
		return a.Entries - b.Entries
	})
	q := &Q3Index{}
	redis, vectors, commits, dates, hardware := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	queries := map[int]bool{}
	for _, r := range results {
		redis[r.RedisVersion], vectors[r.Vectors], commits[r.Commit], hardware[describeHardware(r.Hardware)] = true, true, true, true
		if len(r.Date) >= 10 {
			dates[r.Date[:10]] = true
		}
		queries[r.Queries] = true
		for _, run := range r.Runs {
			q.Rows = append(q.Rows, Q3IndexRow{
				Algorithm: run.Algorithm, Params: params(run.Params),
				Entries: r.Entries, Dims: r.Dims, Contexts: r.Contexts,
				Build:         fmt.Sprintf("%.1f", run.BuildSeconds),
				UnfilteredP50: msf(run.Unfiltered.P50Ms), UnfilteredP99: msf(run.Unfiltered.P99Ms),
				FilteredP50: msf(run.Filtered.P50Ms), FilteredP99: msf(run.Filtered.P99Ms),
				Recall: fmt.Sprintf("%.3f", run.RecallAt1), FilteredRecall: fmt.Sprintf("%.3f", run.FilteredRecallAt1), SizeMB: fmt.Sprintf("%.1f", run.IndexSizeMB),
			})
		}
	}
	q.Redis, q.Vectors, q.Commits, q.Dates, q.Hardware = sortedKeys(redis), sortedKeys(vectors), sortedKeys(commits), sortedKeys(dates), sortedKeys(hardware)
	for n := range queries {
		q.Queries = append(q.Queries, n)
	}
	slices.Sort(q.Queries)
	return q, nil
}

// params renders HNSW parameters as "M 16, EF_CONSTRUCTION 200"; FLAT has
// none.
func params(p map[string]int) string {
	if len(p) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, p[k])
	}
	return strings.Join(parts, ", ")
}

// operating renders an operating point: threshold, hit rate, false-hit
// rate.
func operating(o cacheeval.Operating) (string, string, string) {
	if !o.Found {
		return "none", "—", "—"
	}
	t := threshold(o.Threshold)
	if o.Threshold == gridFloor {
		t += "*"
	}
	return t, rate(o.HitRate), rate(o.FalseHitRate)
}

func atFloor(o cacheeval.Operating) bool { return o.Found && o.Threshold == gridFloor }

// rate renders a proportion as "62.3% (58.1–66.4)", the Wilson 95%
// interval in brackets; with no samples it is "n/a".
func rate(r cacheeval.Rate) string {
	if r.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s (%s–%s)", pct1(r.P), pctNum(r.Low), pctNum(r.High))
}

func pct1(v float64) string { return pctNum(v) + "%" }

// pctNum is v as a percentage with one decimal; a value that rounds to
// zero is "0.0", never "-0.0".
func pctNum(v float64) string {
	v = math.Round(v*1000) / 10
	if v == 0 {
		v = 0
	}
	return fmt.Sprintf("%.1f", v)
}

func threshold(v float64) string { return fmt.Sprintf("%.4f", v) }

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// minNegatives is the fewest negatives, all rejected, whose Wilson upper
// bound is at most target: z²/(n+z²) <= target.
func minNegatives(target float64) int {
	if target <= 0 || target >= 1 {
		return 0
	}
	for n := 1; ; n++ {
		if cacheeval.Wilson(0, n).High <= target {
			return n
		}
	}
}

func spread(q cacheeval.Quantiles) string {
	return fmt.Sprintf("%.3f / %.3f / %.3f (%.3f)", q.P10, q.P50, q.P90, q.Max)
}

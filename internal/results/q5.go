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

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/routereval"
)

// Q5Dir is the subdirectory written by `make eval-router`.
const Q5Dir = routereval.ResultsDir

// q5ClaudeModels are the candidates PLAN.md labels when a key is present,
// cheapest first; the To-measure estimate prices them.
var q5ClaudeModels = []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5"}

// q5MaxRules is how many dataset rules the baselines table lists in full;
// above it only the most and the least accurate are shown.
const q5MaxRules = 9

// Q5 is the router section. Every number is pre-formatted.
type Q5 struct {
	// CostUnit is "GFLOPs" or "$"; CostAxis is "GFLOPs per 1,000
	// requests" or "$ per 1,000 requests". USD is true for dollars.
	CostUnit, CostAxis string
	USD                bool
	// Params lists the candidates' parameter counts for the GFLOPs unit,
	// "`m` 1.5 B; ...", in candidate order; empty when none are recorded.
	Params string
	// Live is true when a priced (Claude) candidate is in the summary.
	Live bool

	Questions int
	// Datasets is "300 GSM8K and 300 MMLU".
	Datasets             string
	Folds, Bootstrap     int
	Seed                 uint64
	AccuracyRule         string // "95%"
	Cheapest, Best       string
	Candidates           []Q5Candidate
	Baselines            []Q5Baseline
	RulesShown, RulesAll int
	// ShareHeader names the share columns' candidates, "`a` / `b` / `c`".
	ShareHeader string
	Variants    []Q5Variant
	Calibration []Q5Calibration
	Operating   Q5Operating
	// Truncated is the replies cut by the output cap, "`a` 3, `b` 1", and
	// TruncatedTotal their sum; empty and 0 when none were.
	Truncated      string
	TruncatedTotal int

	Commit, Date, Command, Hardware string
	Embedding, Ollama               string
	LabelledFrom, LabelledTo        string
	LabelTime, Spent                string

	// Estimate prices the Claude candidates' labels; nil when the
	// summary has no local token counts to estimate from.
	Estimate *Q5Estimate
}

// Q5Candidate is one candidate model answering every question.
type Q5Candidate struct {
	Model, Accuracy, GSM8K, MMLU, Cost string
	P50, P95, OutputTokens, Spent      string
	Truncated                          int
}

// Q5Baseline is one policy in the baselines table; the last two columns
// compare it with always sending to the best candidate.
type Q5Baseline struct {
	Policy, Accuracy, Cost, OfBestAccuracy, OfBestCost string
}

// Q5Variant is one router configuration's held-out frontier.
type Q5Variant struct {
	Name, Description, AUCGain string
	Rows                       []Q5FrontierRow
}

// Q5FrontierRow is one quality target.
type Q5FrontierRow struct {
	Target, Accuracy, Cost, LineAccuracy, Gain, Share string
	// Operating is true on the shipped operating point.
	Operating bool
}

// Q5Calibration is one candidate's correctness predictions under one
// variant, on held-out folds.
type Q5Calibration struct {
	Variant, Model, AUROC, Brier, L2 string
}

// Q5Operating is the shipped quality target.
type Q5Operating struct {
	Variant, Target, Accuracy, Cost string
	// Sentence is "X% of `best`'s accuracy at Y% of its cost; gain over
	// the line +G points (95% CI lo to hi)".
	Sentence string
	Met      bool
	// ShareByDataset is "GSM8K: `a` 10% / ...; MMLU: ...".
	ShareByDataset string
}

// Q5Estimate is what labelling the eval set with the Claude candidates
// would cost at list prices, from one local candidate's token counts.
type Q5Estimate struct {
	From   string // the local candidate whose tokens are priced
	Models []Q5Price
	Total  string
}

// Q5Price is one model's estimated labelling cost.
type Q5Price struct{ Model, USD string }

func loadQ5(dir string) (*Q5, error) {
	path := filepath.Join(dir, "summary.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	var s routereval.Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("results: %s: %w", path, err)
	}
	var meta routereval.Meta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return nil, err
	}
	return buildQ5(path, s, meta)
}

func buildQ5(path string, s routereval.Summary, meta routereval.Meta) (*Q5, error) {
	if len(s.Candidates) == 0 {
		return nil, fmt.Errorf("results: %s has no candidates", path)
	}
	byModel := map[string]routereval.CandidateResult{}
	for _, c := range s.Candidates {
		byModel[c.Model] = c
	}
	cheapest, ok := byModel[s.Cheapest]
	if !ok {
		return nil, fmt.Errorf("results: %s: cheapest candidate %q is not a candidate", path, s.Cheapest)
	}
	best, ok := byModel[s.Best]
	if !ok {
		return nil, fmt.Errorf("results: %s: best candidate %q is not a candidate", path, s.Best)
	}
	if s.CostUnit != routereval.CostUSD && s.CostUnit != routereval.CostGFLOPs {
		return nil, fmt.Errorf("results: %s: unknown cost unit %q", path, s.CostUnit)
	}
	q := &Q5{
		CostUnit: "GFLOPs", CostAxis: "GFLOPs per 1,000 requests", USD: s.CostUnit == routereval.CostUSD,
		Questions: s.Questions, Datasets: q5Datasets(s.ByDataset),
		Folds: s.Folds, Bootstrap: s.Bootstrap, Seed: s.Seed,
		AccuracyRule: strconv.FormatFloat(100*s.AccuracyRule, 'f', -1, 64) + "%",
		Cheapest:     s.Cheapest, Best: s.Best,
		Commit: meta.Commit, Command: meta.Command, Hardware: describeHardware(meta.Hardware),
		Embedding: meta.Embedding, Ollama: meta.Ollama,
		LabelledFrom: day(meta.LabelledFrom), LabelledTo: day(meta.LabelledTo), Date: day(meta.Date),
		LabelTime: hoursMinutes(meta.LabelSeconds), Spent: dollars(cost.Nanos(meta.Nanodollars)),
		RulesAll: len(s.DatasetRules),
	}
	if q.USD {
		q.CostUnit, q.CostAxis = "$", "$ per 1,000 requests"
	}
	if meta.Dimensions > 0 {
		q.Embedding += fmt.Sprintf(" (%d dimensions)", meta.Dimensions)
	}
	c1k := func(v float64) string { return q5Cost(v, q.USD) }

	var params, shareNames, truncated []string
	for _, c := range s.Candidates {
		if _, priced := cost.Defaults[c.Model]; priced {
			q.Live = true
		}
		if p, ok := meta.Params[c.Model]; ok {
			params = append(params, fmt.Sprintf("`%s` %s B", c.Model, strconv.FormatFloat(p, 'f', -1, 64)))
		}
		shareNames = append(shareNames, "`"+c.Model+"`")
		if c.Truncated > 0 {
			truncated = append(truncated, fmt.Sprintf("`%s` %d", c.Model, c.Truncated))
			q.TruncatedTotal += c.Truncated
		}
		q.Candidates = append(q.Candidates, Q5Candidate{
			Model: c.Model, Accuracy: q5Acc(c.Point),
			GSM8K: q5Dataset(c.ByDataset, routereval.DatasetGSM8K), MMLU: q5Dataset(c.ByDataset, routereval.DatasetMMLU),
			Cost: c1k(c.CostPer1K), P50: secs(c.LatencyP50Ms), P95: secs(c.LatencyP95Ms),
			OutputTokens: strconv.FormatFloat(c.MeanOutputTokens, 'f', 1, 64),
			Truncated:    c.Truncated, Spent: dollars(cost.Nanos(c.Nanodollars)),
		})
	}
	q.Params = strings.Join(params, "; ")
	q.ShareHeader = strings.Join(shareNames, " / ")
	q.Truncated = strings.Join(truncated, ", ")

	ofBest := func(p routereval.Point) Q5Baseline {
		b := Q5Baseline{Accuracy: q5Acc(p), Cost: c1k(p.CostPer1K), OfBestAccuracy: "—", OfBestCost: "—"}
		if best.Accuracy > 0 {
			b.OfBestAccuracy = pct1(p.Accuracy / best.Accuracy)
		}
		if best.CostPer1K > 0 {
			b.OfBestCost = pct1(p.CostPer1K / best.CostPer1K)
		}
		return b
	}
	add := func(policy string, p routereval.Point) {
		b := ofBest(p)
		b.Policy = policy
		q.Baselines = append(q.Baselines, b)
	}
	add("always the cheapest (`"+s.Cheapest+"`)", cheapest.Point)
	add("always the best (`"+s.Best+"`)", best.Point)
	add("oracle (cheapest correct candidate)", s.Oracle)
	op := s.OperatingPoint
	if op.Variant != "" {
		add(fmt.Sprintf("`%s` at target %s (operating point, held out)", op.Variant, target(op.Target)), op.Point)
	}
	rules := slices.Clone(s.DatasetRules)
	slices.SortStableFunc(rules, func(a, b routereval.DatasetRule) int {
		switch {
		case a.Accuracy > b.Accuracy:
			return -1
		case a.Accuracy < b.Accuracy:
			return 1
		}
		return 0
	})
	if len(rules) > q5MaxRules {
		rules = []routereval.DatasetRule{rules[0], rules[len(rules)-1]}
	}
	q.RulesShown = len(rules)
	for _, r := range rules {
		add(fmt.Sprintf("knows the dataset: GSM8K to `%s`, MMLU to `%s`", r.GSM8K, r.MMLU), r.Point)
	}

	for _, v := range s.Variants {
		qv := Q5Variant{Name: v.Name, Description: v.Description, AUCGain: points(100 * v.AUCGain)}
		for _, f := range v.Frontier {
			qv.Rows = append(qv.Rows, Q5FrontierRow{
				Target: target(f.Target), Accuracy: q5Acc(f.Point), Cost: c1k(f.CostPer1K),
				LineAccuracy: pct1(f.LineAccuracy), Gain: points(100 * f.Gain),
				Share:     q5Share(s.Candidates, f.Share),
				Operating: v.Name == op.Variant && f.Target == op.Target,
			})
		}
		q.Variants = append(q.Variants, qv)
		for _, c := range s.Candidates {
			cal := Q5Calibration{Variant: v.Name, Model: c.Model, AUROC: "—", Brier: "—", L2: "—"}
			if a, ok := v.AUROC[c.Model]; ok {
				cal.AUROC = strconv.FormatFloat(a, 'f', 3, 64)
			}
			if b, ok := v.Brier[c.Model]; ok {
				cal.Brier = strconv.FormatFloat(b, 'f', 3, 64)
			}
			if l, ok := s.L2[v.Name][c.Model]; ok {
				cal.L2 = strconv.FormatFloat(l, 'g', -1, 64)
			}
			q.Calibration = append(q.Calibration, cal)
		}
	}

	if op.Variant != "" {
		q.Operating = Q5Operating{
			Variant: op.Variant, Target: target(op.Target), Accuracy: q5Acc(op.Point), Cost: c1k(op.CostPer1K), Met: op.Met,
			Sentence: fmt.Sprintf("%s of `%s`'s accuracy at %s of its cost; gain over the line %s points (95%% CI %s to %s)",
				pct1(op.PctBestAccuracy/100), s.Best, pct1(op.PctBestCost/100),
				points(100*op.Gain), points(100*op.GainCI[0]), points(100*op.GainCI[1])),
			ShareByDataset: q5ShareByDataset(s.Candidates, op.ShareByDataset),
		}
	}
	q.Estimate = q5Estimate(s, meta)
	return q, nil
}

// q5Estimate prices the largest local candidate's tokens over the eval
// set at each Claude model's list price. It needs the candidate's
// parameter count (to know which is largest) and its input token count.
func q5Estimate(s routereval.Summary, meta routereval.Meta) *Q5Estimate {
	from, size := "", 0.0
	for _, c := range s.Candidates {
		if _, priced := cost.Defaults[c.Model]; priced {
			continue
		}
		if p := meta.Params[c.Model]; p > size {
			from, size = c.Model, p
		}
	}
	var in, out float64
	for _, c := range s.Candidates {
		if c.Model == from {
			in, out = c.MeanInputTokens, c.MeanOutputTokens
		}
	}
	if from == "" || in <= 0 || out <= 0 || s.Questions <= 0 {
		return nil
	}
	usage := core.Usage{
		InputTokens:  int(math.Round(in * float64(s.Questions))),
		OutputTokens: int(math.Round(out * float64(s.Questions))),
	}
	e := &Q5Estimate{From: from}
	var total cost.Nanos
	for _, m := range q5ClaudeModels {
		p, ok := cost.Defaults[m]
		if !ok {
			continue
		}
		// Each question is a short request, so the base rates apply; Of
		// on the summed usage would read it as one long prompt.
		n := cost.Price{Base: p.Base}.Of(usage)
		total += n
		e.Models = append(e.Models, Q5Price{Model: m, USD: fmt.Sprintf("$%.2f", n.USD())})
	}
	e.Total = fmt.Sprintf("$%.2f", total.USD())
	return e
}

// q5Acc is an accuracy with its Wilson interval, "62.3% (58.1–66.4)".
func q5Acc(p routereval.Point) string {
	return fmt.Sprintf("%s (%s–%s)", pct1(p.Accuracy), pctNum(p.AccuracyCI[0]), pctNum(p.AccuracyCI[1]))
}

func q5Dataset(by map[string]float64, name string) string {
	if v, ok := by[name]; ok {
		return pct1(v)
	}
	return "—"
}

// q5Cost formats a cost per 1,000 requests: dollars to four decimals, or
// GFLOPs as a whole number with thousands separators (two decimals below
// 100).
func q5Cost(v float64, usd bool) string {
	if usd {
		return fmt.Sprintf("$%.4f", v)
	}
	if v < 100 {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return thousands(strconv.FormatFloat(v, 'f', 0, 64))
}

func thousands(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// q5Share is the share of requests per candidate, in candidate order,
// "10% / 30% / 60%".
func q5Share(cands []routereval.CandidateResult, share map[string]float64) string {
	parts := make([]string, len(cands))
	for i, c := range cands {
		parts[i] = strconv.FormatFloat(math.Round(100*share[c.Model]), 'f', 0, 64) + "%"
	}
	return strings.Join(parts, " / ")
}

func q5ShareByDataset(cands []routereval.CandidateResult, by map[string]map[string]float64) string {
	var parts []string
	for _, d := range q5DatasetOrder(by) {
		parts = append(parts, q5DatasetName(d)+" "+q5Share(cands, by[d]))
	}
	return strings.Join(parts, "; ")
}

// q5DatasetOrder is GSM8K, MMLU, then any other dataset by name.
func q5DatasetOrder[V any](by map[string]V) []string {
	var out []string
	for _, d := range []string{routereval.DatasetGSM8K, routereval.DatasetMMLU} {
		if _, ok := by[d]; ok {
			out = append(out, d)
		}
	}
	var rest []string
	for d := range by {
		if d != routereval.DatasetGSM8K && d != routereval.DatasetMMLU {
			rest = append(rest, d)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

func q5DatasetName(d string) string {
	switch d {
	case routereval.DatasetGSM8K:
		return "GSM8K"
	case routereval.DatasetMMLU:
		return "MMLU"
	}
	return d
}

func q5Datasets(by map[string]int) string {
	var parts []string
	for _, d := range q5DatasetOrder(by) {
		parts = append(parts, fmt.Sprintf("%d %s", by[d], q5DatasetName(d)))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// points is a difference in accuracy points with an explicit sign; one
// that rounds to zero is "+0.0".
func points(v float64) string {
	v = math.Round(v*10) / 10
	if v == 0 {
		v = 0
	}
	return fmt.Sprintf("%+.1f", v)
}

func target(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func secs(ms float64) string { return strconv.FormatFloat(ms/1000, 'f', 2, 64) }

// day is the date part of an RFC 3339 timestamp.
func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// hoursMinutes renders a duration in seconds as "2 h 05 min" or "13 min".
func hoursMinutes(sec float64) string {
	m := int(math.Round(sec / 60))
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d min", m/60, m%60)
}

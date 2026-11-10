package routereval

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/router"
)

// Experiment settings fixed before the first run; PLAN.md states them.
const (
	defaultFolds     = 5
	defaultSeed      = 1
	defaultBootstrap = 2000
	// AccuracyRule picks the shipped quality target: the lowest target
	// whose held-out accuracy is at least this share of the best
	// candidate's.
	AccuracyRule = 0.95
	// targetStep is the quality-target grid's spacing, from 0 to 1.
	targetStep = 0.01
)

// DefaultParams are the local candidates' parameter counts in billions,
// from the Qwen2.5 model cards, for the GFLOPs cost unit.
var DefaultParams = map[string]float64{"qwen2.5:0.5b": 0.49, "qwen2.5:1.5b": 1.54, "qwen2.5:3b": 3.09}

// Variants are the router configurations cross-validated, in report
// order; the first is the one the gateway ships.
var Variants = []struct {
	Name, Description string
	Features          Features
	Embedding         bool
}{
	{"router", "cheap text features and the prompt embedding", WithEmbedding, true},
	{"router-cheap", "the nine cheap text features alone", CheapOnly, false},
}

// Options configures a run of the experiment.
type Options struct {
	Folds, Bootstrap int
	Seed             uint64
	// Params are parameter counts in billions per candidate; when every
	// candidate has one and not every candidate has a price, the cost
	// unit is GFLOPs.
	Params map[string]float64
}

func runMain(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eval router run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	questions := fs.String("questions", "eval/router/data/questions.jsonl", "the eval set")
	recordings := fs.String("recordings", "eval/recordings/router", "directory of per-model recordings")
	models := fs.String("models", "qwen2.5:0.5b,qwen2.5:1.5b,qwen2.5:3b", "comma-separated candidates")
	embedModel := fs.String("embedding", "nomic-embed-text", "embedding model whose committed vectors are the features")
	out := fs.String("out", filepath.Join("eval/results", ResultsDir), "directory for summary.json, meta.json, weights.json and frontier.svg")
	weightsOut := fs.String("weights-out", "internal/router/weights.json", "second copy of the weights, embedded in the binary as the default ('' to skip)")
	command := fs.String("command", "make eval-router", "command recorded in meta.json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opt := Options{Folds: defaultFolds, Bootstrap: defaultBootstrap, Seed: defaultSeed, Params: DefaultParams}
	qs, err := readJSONL[Question](*questions)
	if err != nil {
		return err
	}
	names := strings.Split(*models, ",")
	labels := map[string][]Label{}
	for _, m := range names {
		ls, err := readJSONL[Label](RecordingPath(*recordings, m))
		if err != nil {
			return err
		}
		labels[m] = ls
	}
	embs, err := readJSONL[Embedding](EmbeddingsPath(filepath.Dir(*questions), *embedModel))
	if err != nil {
		return err
	}
	t, unit, regraded, err := BuildTable(qs, names, labels, embs, opt.Params)
	if err != nil {
		return err
	}
	if regraded > 0 {
		_, _ = fmt.Fprintf(stdout, "%d recorded grades differ from the current grader; the current grader's are used\n", regraded)
	}
	start := time.Now()
	s, w, err := Run(t, unit, opt, stdout)
	if err != nil {
		return err
	}
	FillCandidateStats(s, t, labels)
	w.Embedding, w.Dimensions = *embedModel, len(t.Embedding[0])
	w.Source = "switchboard eval router run (" + gitCommit() + ")"
	wb, err := w.Marshal()
	if err != nil {
		return err
	}
	// label.sh records the Ollama version that served the local labels.
	ollama, _ := os.ReadFile(filepath.Join(*recordings, "ollama-version"))
	meta := buildMeta(t, labels, opt, *embedModel, strings.TrimSpace(string(ollama)), *command)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "summary.json"), s); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "meta.json"), meta); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "weights.json"), wb, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "frontier.svg"), PlotFrontier(*s), 0o644); err != nil {
		return err
	}
	if *weightsOut != "" {
		if err := os.WriteFile(*weightsOut, wb, 0o644); err != nil {
			return err
		}
	}
	op := s.OperatingPoint
	_, _ = fmt.Fprintf(stdout, "%d questions, %d candidates, cost in %s, %s\n", s.Questions, len(s.Candidates), s.CostUnit, time.Since(start).Round(time.Second))
	_, _ = fmt.Fprintf(stdout, "operating point: target %.2f, accuracy %.1f%% at %.1f%% of %s's cost (%.1f%% of its accuracy), gain over the line %+.1f points (95%% CI %+.1f to %+.1f)\n",
		op.Target, 100*op.Accuracy, op.PctBestCost, s.Best, op.PctBestAccuracy, 100*op.Gain, 100*op.GainCI[0], 100*op.GainCI[1])
	_, _ = fmt.Fprintf(stdout, "wrote %s\n", *out)
	return nil
}

// BuildTable joins the eval set with the recordings and embeddings. Every
// candidate must have answered every question: a policy compared on a
// subset its rivals did not face would not be comparable. Grades are
// recomputed from the recorded replies with the current grader, so a
// grader fix applies without relabelling; regraded counts the changes.
func BuildTable(qs []Question, models []string, labels map[string][]Label, embs []Embedding, params map[string]float64) (t *Table, unit string, regraded int, err error) {
	if len(models) < 2 {
		return nil, "", 0, errors.New("a router needs at least two candidates")
	}
	unit = CostUSD
	for _, m := range models {
		if !priced(m) {
			unit = CostGFLOPs
		}
	}
	if unit == CostGFLOPs {
		for _, m := range models {
			if params[m] <= 0 {
				return nil, "", 0, fmt.Errorf("candidate %q has no price and no parameter count: no common cost unit", m)
			}
		}
	}
	byID := map[string]map[string]Label{}
	for m, ls := range labels {
		for _, l := range ls {
			if byID[l.ID] == nil {
				byID[l.ID] = map[string]Label{}
			}
			byID[l.ID][m] = l
		}
	}
	vec := map[string][]float32{}
	for _, e := range embs {
		vec[e.ID] = e.Vector
	}
	t = &Table{Models: models}
	var missing []string
	for _, q := range qs {
		row, ok := byID[q.ID]
		var correct []bool
		var costs []float64
		for _, m := range models {
			l, has := row[m]
			if !ok || !has {
				missing = append(missing, q.ID+" "+m)
				continue
			}
			_, c := Grade(q, l.Output)
			if c != l.Correct {
				regraded++
			}
			correct = append(correct, c)
			if unit == CostUSD {
				costs = append(costs, float64(l.Nanodollars)/1e9)
			} else {
				costs = append(costs, 2*params[m]*float64(l.InputTokens+l.OutputTokens))
			}
		}
		v, hasVec := vec[q.ID]
		if !hasVec {
			missing = append(missing, q.ID+" embedding")
		}
		if len(correct) != len(models) || !hasVec {
			continue
		}
		_, user := Prompt(q)
		t.Questions = append(t.Questions, q)
		t.Correct = append(t.Correct, correct)
		t.Cost = append(t.Cost, costs)
		t.Cheap = append(t.Cheap, router.Cheap(user))
		t.Embedding = append(t.Embedding, v)
	}
	if len(missing) > 0 {
		return nil, "", 0, fmt.Errorf("%d labels or embeddings are missing (first: %s); run eval/router/label.sh and eval router embed", len(missing), missing[0])
	}
	return t, unit, regraded, nil
}

// Run cross-validates every variant, computes the baselines, the line and
// the operating point, and trains the shipped weights on the whole set.
func Run(t *Table, unit string, opt Options, log io.Writer) (*Summary, *router.Weights, error) {
	n := len(t.Questions)
	s := &Summary{
		CostUnit: unit, Questions: n, ByDataset: map[string]int{},
		Folds: opt.Folds, Seed: opt.Seed, Bootstrap: opt.Bootstrap, AccuracyRule: AccuracyRule,
		L2: map[string]map[string]float64{},
	}
	for _, q := range t.Questions {
		s.ByDataset[q.Dataset]++
	}
	// The line runs from the cheapest candidate (the more accurate on a
	// tie) to the most accurate (the cheaper on a tie).
	cheap, best := 0, 0
	for m := range t.Models {
		pt := t.point(constant(n, m))
		s.Candidates = append(s.Candidates, CandidateResult{Model: t.Models[m], Point: pt})
		c, b := s.Candidates[cheap].Point, s.Candidates[best].Point
		if pt.CostPer1K < c.CostPer1K || (pt.CostPer1K == c.CostPer1K && pt.Accuracy > c.Accuracy) {
			cheap = m
		}
		if pt.Accuracy > b.Accuracy || (pt.Accuracy == b.Accuracy && pt.CostPer1K < b.CostPer1K) {
			best = m
		}
	}
	s.Cheapest, s.Best = t.Models[cheap], t.Models[best]
	line := t.lineFor(cheap, best, nil)

	oracle := make([]int, n)
	for q := range n {
		oracle[q] = cheap
		for m := range t.Models {
			if t.Correct[q][m] && (!t.Correct[q][oracle[q]] || t.Cost[q][m] < t.Cost[q][oracle[q]]) {
				oracle[q] = m
			}
		}
	}
	s.Oracle = t.point(oracle)
	for _, g := range t.Models {
		for _, mm := range t.Models {
			pol := make([]int, n)
			for q := range n {
				pol[q] = slices.Index(t.Models, mm)
				if t.Questions[q].Dataset == DatasetGSM8K {
					pol[q] = slices.Index(t.Models, g)
				}
			}
			s.DatasetRules = append(s.DatasetRules, DatasetRule{GSM8K: g, MMLU: mm, Point: t.point(pol)})
		}
	}

	targets := targetGrid()
	var shipped *Prediction
	for vi, v := range Variants {
		start := time.Now()
		p, err := CrossValidate(t, v.Features, opt.Folds, opt.Seed)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", v.Name, err)
		}
		if vi == 0 {
			shipped = p
		}
		out := Variant{Name: v.Name, Description: v.Description, Brier: map[string]float64{}, AUROC: map[string]float64{}}
		var prev []int
		for _, tg := range targets {
			choice := p.Route(tg)
			if slices.Equal(choice, prev) {
				continue // the same policy as the previous target
			}
			prev = choice
			out.Frontier = append(out.Frontier, t.frontierPoint(tg, choice, line))
		}
		out.AUCGain = AUCGain(out.Frontier, line)
		s.L2[v.Name] = map[string]float64{}
		for m, name := range t.Models {
			probs := make([]float64, n)
			ys := make([]bool, n)
			var brier float64
			for q := range n {
				probs[q], ys[q] = p.Prob[q][m], t.Correct[q][m]
				y := 0.0
				if ys[q] {
					y = 1
				}
				brier += (probs[q] - y) * (probs[q] - y)
			}
			out.Brier[name] = round6(brier / float64(n))
			// A candidate that is always right or always wrong has no
			// AUROC; JSON has no NaN, so it is recorded as 0.
			if a := AUROC(probs, ys); !math.IsNaN(a) {
				out.AUROC[name] = round6(a)
			}
			col := make([]float64, len(p.L2))
			for f := range p.L2 {
				col[f] = p.L2[f][m]
			}
			s.L2[v.Name][name] = mode(col)
		}
		s.Variants = append(s.Variants, out)
		if log != nil {
			_, _ = fmt.Fprintf(log, "%s: %d frontier points, mean gain over the line %+.2f points, %s\n",
				v.Name, len(out.Frontier), 100*out.AUCGain, time.Since(start).Round(time.Second))
		}
	}

	s.OperatingPoint = t.operatingPoint(shipped, targets, cheap, best, line, opt)
	s.OperatingPoint.Variant = Variants[0].Name

	w, err := trainShipped(t, best, s.OperatingPoint.Target, unit, opt.Seed)
	if err != nil {
		return nil, nil, err
	}
	return s, w, nil
}

func targetGrid() []float64 {
	var out []float64
	for i := 0; i <= int(math.Round(1/targetStep)); i++ {
		out = append(out, round6(float64(i)*targetStep))
	}
	return out
}

// point evaluates a policy over the whole set with its Wilson interval.
func (t *Table) point(choice []int) Point {
	acc, c := t.Evaluate(choice, nil)
	k := int(math.Round(acc * float64(len(choice))))
	return Point{Accuracy: round6(acc), AccuracyCI: roundPair(Wilson(k, len(choice))), CostPer1K: round6(c)}
}

func (t *Table) frontierPoint(target float64, choice []int, line Line) FrontierPoint {
	pt := t.point(choice)
	share := map[string]float64{}
	for _, m := range choice {
		share[t.Models[m]]++
	}
	for k := range share {
		share[k] = round6(share[k] / float64(len(choice)))
	}
	la := line.At(pt.CostPer1K)
	return FrontierPoint{Target: target, Point: pt, LineAccuracy: round6(la), Gain: round6(pt.Accuracy - la), Share: share}
}

// operatingPoint applies the rule fixed before the run (the lowest target
// whose held-out accuracy reaches AccuracyRule of the best candidate's)
// and bootstraps the gain over the line. Each resample re-applies the
// rule, so the interval includes the uncertainty of choosing the target
// on the same questions it is scored on.
func (t *Table) operatingPoint(p *Prediction, targets []float64, cheap, best int, line Line, opt Options) OperatingPoint {
	n := len(t.Questions)
	choices := make([][]int, len(targets))
	for i, tg := range targets {
		choices[i] = p.Route(tg)
	}
	pick := func(sample []int, l Line) (int, bool) {
		bestAcc, _ := t.Evaluate(constant(n, best), sample)
		top, topAcc := 0, -1.0
		for i := range targets {
			acc, _ := t.Evaluate(choices[i], sample)
			if acc >= AccuracyRule*bestAcc-1e-12 {
				return i, true
			}
			if acc > topAcc {
				top, topAcc = i, acc
			}
		}
		return top, false
	}
	i, met := pick(nil, line)
	op := OperatingPoint{FrontierPoint: t.frontierPoint(targets[i], choices[i], line), Met: met}
	bestPt := t.point(constant(n, best))
	op.PctBestAccuracy = round6(100 * op.Accuracy / bestPt.Accuracy)
	op.PctBestCost = round6(100 * op.CostPer1K / bestPt.CostPer1K)
	op.ShareByDataset = map[string]map[string]float64{}
	count := map[string]float64{}
	for q, m := range choices[i] {
		d := t.Questions[q].Dataset
		if op.ShareByDataset[d] == nil {
			op.ShareByDataset[d] = map[string]float64{}
		}
		op.ShareByDataset[d][t.Models[m]]++
		count[d]++
	}
	for d, sh := range op.ShareByDataset {
		for m := range sh {
			sh[m] = round6(sh[m] / count[d])
		}
	}

	rng := rand.New(rand.NewPCG(opt.Seed, 0xb007))
	gains := make([]float64, opt.Bootstrap)
	sample := make([]int, n)
	for b := range opt.Bootstrap {
		for k := range sample {
			sample[k] = rng.IntN(n)
		}
		l := t.lineFor(cheap, best, sample)
		j, _ := pick(sample, l)
		acc, c := t.Evaluate(choices[j], sample)
		gains[b] = acc - l.At(c)
	}
	sort.Float64s(gains)
	op.GainCI = [2]float64{round6(quantile(gains, 0.025)), round6(quantile(gains, 0.975))}
	return op
}

// trainShipped fits the gateway's weights on every question, with the L2
// inner cross-validation chooses on the whole set.
func trainShipped(t *Table, best int, target float64, unit string, seed uint64) (*router.Weights, error) {
	n := len(t.Questions)
	rows := make([]int, n)
	for i := range rows {
		rows[i] = i
	}
	f, _, err := fit(t, WithEmbedding, rows, seed)
	if err != nil {
		return nil, fmt.Errorf("shipped weights: %w", err)
	}
	order, _ := rank(t, rows)
	w := &router.Weights{
		Version: router.WeightsVersion, Features: slices.Clone(router.CheapFeatureNames),
		Scaler: f.scaler, CostUnit: unit, Fallback: t.Models[best], QualityTarget: target,
	}
	for _, m := range order {
		acc, c := t.Evaluate(constant(n, m), nil)
		w.Candidates = append(w.Candidates, router.Candidate{
			Model: t.Models[m], Cost: round6(c / 1000), Accuracy: round6(acc), Logistic: f.logit[m],
		})
	}
	return w, nil
}

func buildMeta(t *Table, labels map[string][]Label, opt Options, embedModel, ollama, command string) Meta {
	m := Meta{
		Commit: gitCommit(), Date: time.Now().UTC().Format(time.RFC3339), Command: command,
		Embedding: embedModel, Dimensions: len(t.Embedding[0]), Hardware: bench.DetectHardware(),
		Ollama: ollama, Params: map[string]float64{},
	}
	var from, to string
	for _, name := range t.Models {
		if p, ok := opt.Params[name]; ok && !priced(name) {
			m.Params[name] = p
		}
		for _, l := range labels[name] {
			m.LabelSeconds += l.TotalMillis / 1000
			m.Nanodollars += l.Nanodollars
			if from == "" || l.At < from {
				from = l.At
			}
			if l.At > to {
				to = l.At
			}
		}
	}
	m.LabelSeconds = math.Round(m.LabelSeconds)
	m.LabelledFrom, m.LabelledTo = from, to
	return m
}

// FillCandidateStats adds the per-candidate numbers that come from the
// raw labels rather than the table: per-dataset accuracy, latency, token
// counts, truncations and spend.
func FillCandidateStats(s *Summary, t *Table, labels map[string][]Label) {
	inSet := map[string]Question{}
	for _, q := range t.Questions {
		inSet[q.ID] = q
	}
	for i := range s.Candidates {
		c := &s.Candidates[i]
		m := slices.Index(t.Models, c.Model)
		right, total := map[string]float64{}, map[string]float64{}
		for q := range t.Questions {
			d := t.Questions[q].Dataset
			total[d]++
			if t.Correct[q][m] {
				right[d]++
			}
		}
		c.ByDataset = map[string]float64{}
		for _, d := range slices.Sorted(maps.Keys(total)) {
			c.ByDataset[d] = round6(right[d] / total[d])
		}
		var lat []float64
		var in, out float64
		for _, l := range labels[c.Model] {
			if _, ok := inSet[l.ID]; !ok {
				continue
			}
			lat = append(lat, l.TotalMillis)
			in += float64(l.InputTokens)
			out += float64(l.OutputTokens)
			if l.StopReason == "length" {
				c.Truncated++
			}
			c.Nanodollars += l.Nanodollars
		}
		sort.Float64s(lat)
		c.LatencyP50Ms = round6(quantile(lat, 0.5))
		c.LatencyP95Ms = round6(quantile(lat, 0.95))
		if len(lat) > 0 {
			c.MeanInputTokens = round6(in / float64(len(lat)))
			c.MeanOutputTokens = round6(out / float64(len(lat)))
		}
	}
}

// quantile is the nearest-rank quantile of sorted values.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// mode is the most frequent value, the smallest on a tie.
func mode(v []float64) float64 {
	count := map[float64]int{}
	for _, x := range v {
		count[x]++
	}
	best, bestN := math.Inf(1), 0
	for x, k := range count {
		if k > bestN || (k == bestN && x < best) {
			best, bestN = x, k
		}
	}
	return best
}

// round6 keeps the raw files readable and byte-stable across platforms
// whose last-bit float results differ.
func round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 6, 64), 64)
	return r
}

func roundPair(p [2]float64) [2]float64 { return [2]float64{round6(p[0]), round6(p[1])} }

// priced reports whether the gateway's price table knows the model.
func priced(model string) bool {
	_, ok := cost.Defaults[model]
	return ok
}

func gitCommit() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	c := strings.TrimSpace(string(out))
	if err := exec.CommandContext(ctx, "git", "diff", "--quiet", "HEAD", "--", ".", ":(exclude)eval/results", ":(exclude)internal/router/weights.json").Run(); err != nil {
		c += "-dirty"
	}
	return c
}

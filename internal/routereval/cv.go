package routereval

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"

	"github.com/TheAsianFish/switchboard/internal/router"
)

// Table is the labelled eval set in the shape the experiment needs: one
// row per question, one column per candidate.
type Table struct {
	Questions []Question
	Models    []string
	// Correct[q][m] and Cost[q][m] are candidate m's grade and cost on
	// question q.
	Correct [][]bool
	Cost    [][]float64
	// Cheap and Embedding are question q's features; Embedding may be nil
	// for every row (no embedding variant).
	Cheap     [][]float64
	Embedding [][]float32
}

// Features is a variant's feature vector for row q.
type Features func(t *Table, q int) []float64

// WithEmbedding is the full router: cheap features and the embedding.
func WithEmbedding(t *Table, q int) []float64 { return router.Vector(t.Cheap[q], t.Embedding[q]) }

// CheapOnly leaves the embedding out.
func CheapOnly(t *Table, q int) []float64 { return router.Vector(t.Cheap[q], nil) }

// L2Grid is the regularisation strengths inner cross-validation chooses
// from. The loss is summed over rows, so these are 1/C in scikit-learn's
// terms.
var L2Grid = []float64{0.1, 1, 10, 100, 1000, 10000}

// Folds assigns each row to one of k folds, stratified by dataset: each
// dataset's rows are shuffled with the seed and dealt round robin, so every
// fold has the same mix of GSM8K and MMLU.
func Folds(qs []Question, idx []int, k int, seed uint64) []int {
	rng := rand.New(rand.NewPCG(seed, 0x5f0))
	byDataset := map[string][]int{}
	var names []string
	for _, i := range idx {
		d := qs[i].Dataset
		if _, ok := byDataset[d]; !ok {
			names = append(names, d)
		}
		byDataset[d] = append(byDataset[d], i)
	}
	slices.Sort(names)
	fold := make([]int, len(qs))
	for i := range fold {
		fold[i] = -1
	}
	next := 0
	for _, d := range names {
		rows := byDataset[d]
		for _, p := range rng.Perm(len(rows)) {
			fold[rows[p]] = next % k
			next++
		}
	}
	return fold
}

// Prediction is the held-out output of cross-validation for one variant.
type Prediction struct {
	// Prob[q][m] is the held-out P(correct) of candidate m on question q.
	Prob [][]float64
	// Order[q] lists candidate indices from cheapest to dearest, by mean
	// cost on question q's training folds; Fallback[q] is the most
	// accurate candidate there. Both are what the router would have had
	// when q was unseen.
	Order    [][]int
	Fallback []int
	// L2[f][m] is the strength inner cross-validation chose.
	L2 [][]float64
}

// CrossValidate trains on k−1 folds and predicts the k-th, for every
// fold. Every choice the model makes (feature scaling, regularisation,
// candidate order, fallback) is fitted on the training folds alone.
func CrossValidate(t *Table, feat Features, k int, seed uint64) (*Prediction, error) {
	n, nm := len(t.Questions), len(t.Models)
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	fold := Folds(t.Questions, all, k, seed)
	p := &Prediction{Prob: make([][]float64, n), Order: make([][]int, n), Fallback: make([]int, n), L2: make([][]float64, k)}
	for f := range k {
		var train, test []int
		for i := range n {
			if fold[i] == f {
				test = append(test, i)
			} else {
				train = append(train, i)
			}
		}
		models, l2, err := fit(t, feat, train, seed+uint64(f)+1)
		if err != nil {
			return nil, fmt.Errorf("fold %d: %w", f, err)
		}
		p.L2[f] = l2
		order, fallback := rank(t, train)
		for _, q := range test {
			x := models.scaler.Apply(feat(t, q))
			p.Prob[q] = make([]float64, nm)
			for m := range nm {
				p.Prob[q][m] = models.logit[m].Prob(x)
			}
			p.Order[q], p.Fallback[q] = order, fallback
		}
	}
	return p, nil
}

type fitted struct {
	scaler router.Scaler
	logit  []router.Logistic
}

// fit trains one regression per candidate on rows, each with the L2
// strength that minimises held-out log loss in an inner cross-validation
// over rows.
func fit(t *Table, feat Features, rows []int, seed uint64) (fitted, []float64, error) {
	X := make([][]float64, len(rows))
	for i, q := range rows {
		X[i] = feat(t, q)
	}
	sc := router.FitScaler(X)
	for i := range X {
		X[i] = sc.Apply(X[i])
	}
	out := fitted{scaler: sc, logit: make([]router.Logistic, len(t.Models))}
	l2s := make([]float64, len(t.Models))
	for m := range t.Models {
		y := make([]bool, len(rows))
		for i, q := range rows {
			y[i] = t.Correct[q][m]
		}
		l2, err := chooseL2(t, rows, X, y, seed)
		if err != nil {
			return out, nil, fmt.Errorf("%s: %w", t.Models[m], err)
		}
		lg, err := router.Train(X, y, router.TrainOptions{L2: l2})
		if err != nil {
			return out, nil, fmt.Errorf("%s: %w", t.Models[m], err)
		}
		out.logit[m], l2s[m] = lg, l2
	}
	return out, l2s, nil
}

// innerFolds is the number of folds of the inner cross-validation that
// picks L2.
const innerFolds = 5

// chooseL2 picks the grid value with the lowest summed held-out log loss.
// The inner folds reuse the outer scaling: refitting it per inner fold
// would change the scale of L2 between the search and the final fit.
func chooseL2(t *Table, rows []int, xs [][]float64, y []bool, seed uint64) (float64, error) {
	pos := make([]int, len(rows))
	for i := range pos {
		pos[i] = i
	}
	qs := make([]Question, len(rows))
	for i, q := range rows {
		qs[i] = t.Questions[q]
	}
	fold := Folds(qs, pos, innerFolds, seed)
	best, bestLoss := L2Grid[0], math.Inf(1)
	for _, l2 := range L2Grid {
		var loss float64
		for f := range innerFolds {
			var tx [][]float64
			var ty []bool
			for i := range rows {
				if fold[i] != f {
					tx, ty = append(tx, xs[i]), append(ty, y[i])
				}
			}
			lg, err := router.Train(tx, ty, router.TrainOptions{L2: l2})
			if err != nil {
				return 0, err
			}
			for i := range rows {
				if fold[i] == f {
					loss += logLoss(lg.Prob(xs[i]), y[i])
				}
			}
		}
		if loss < bestLoss {
			best, bestLoss = l2, loss
		}
	}
	return best, nil
}

func logLoss(p float64, y bool) float64 {
	const eps = 1e-12
	p = math.Min(math.Max(p, eps), 1-eps)
	if y {
		return -math.Log(p)
	}
	return -math.Log(1 - p)
}

// rank orders candidates by mean cost on rows (cheapest first, ties by
// position) and names the most accurate one (ties to the cheaper).
func rank(t *Table, rows []int) (order []int, fallback int) {
	nm := len(t.Models)
	mean := make([]float64, nm)
	acc := make([]float64, nm)
	for _, q := range rows {
		for m := range nm {
			mean[m] += t.Cost[q][m]
			if t.Correct[q][m] {
				acc[m]++
			}
		}
	}
	order = make([]int, nm)
	for m := range order {
		order[m] = m
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(mean[a], mean[b]) })
	fallback = order[0]
	for _, m := range order {
		if acc[m] > acc[fallback] {
			fallback = m
		}
	}
	return order, fallback
}

// Route applies the serving rule to held-out predictions: for each
// question, the cheapest candidate whose P(correct) is at least target,
// else the fallback.
func (p *Prediction) Route(target float64) []int {
	out := make([]int, len(p.Prob))
	for q := range p.Prob {
		out[q] = p.Fallback[q]
		if target >= 1 {
			continue // as the router: 1 or more always means the fallback
		}
		for _, m := range p.Order[q] {
			if p.Prob[q][m] >= target {
				out[q] = m
				break
			}
		}
	}
	return out
}

// Evaluate is the accuracy and cost of sending row q to choice[q], over
// the rows in sample (every row when sample is nil).
func (t *Table) Evaluate(choice []int, sample []int) (acc, costPer1K float64) {
	n := len(choice)
	if sample != nil {
		n = len(sample)
	}
	var right, spent float64
	for i := range n {
		q := i
		if sample != nil {
			q = sample[i]
		}
		if t.Correct[q][choice[q]] {
			right++
		}
		spent += t.Cost[q][choice[q]]
	}
	return right / float64(n), 1000 * spent / float64(n)
}

// Line is the random-mix line: sending a share s of requests to the best
// candidate and the rest to the cheapest gives, in expectation, accuracy
// and cost on the straight line between the two.
type Line struct {
	CheapAcc, CheapCost, BestAcc, BestCost float64
}

// At is the line's accuracy at a cost, held at its ends outside them.
func (l Line) At(cost float64) float64 {
	if l.BestCost <= l.CheapCost {
		return math.Max(l.CheapAcc, l.BestAcc)
	}
	s := (cost - l.CheapCost) / (l.BestCost - l.CheapCost)
	s = math.Min(1, math.Max(0, s))
	return l.CheapAcc + s*(l.BestAcc-l.CheapAcc)
}

// constant is a policy sending every question to candidate m.
func constant(n, m int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = m
	}
	return out
}

// lineFor builds the line between candidates cheap and best on sample.
func (t *Table) lineFor(cheap, best int, sample []int) Line {
	n := len(t.Questions)
	ca, cc := t.Evaluate(constant(n, cheap), sample)
	ba, bc := t.Evaluate(constant(n, best), sample)
	return Line{CheapAcc: ca, CheapCost: cc, BestAcc: ba, BestCost: bc}
}

// Reference is an accuracy-against-cost curve a router is measured
// against: the random-mix line, or the candidates' hull.
type Reference interface {
	At(cost float64) float64
}

// Hull is the upper hull of the candidates' points: for every cost, the
// best accuracy a random mix of two candidates reaches. With two
// candidates it is the line; with more, a candidate above the line
// between the cheapest and the best lifts it, and a router that gains
// over the line only by sending traffic to that candidate gains nothing
// over the hull.
type Hull struct {
	Cost, Acc []float64
}

// NewHull builds the hull of points (cost, accuracy).
func NewHull(cost, acc []float64) Hull {
	idx := make([]int, len(cost))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		if c := cmp.Compare(cost[a], cost[b]); c != 0 {
			return c
		}
		return cmp.Compare(acc[b], acc[a])
	})
	var h Hull
	for _, i := range idx {
		x, y := cost[i], acc[i]
		if n := len(h.Cost); n > 0 && (y <= h.Acc[n-1]) {
			continue // dearer and no more accurate: never worth mixing in
		}
		// Drop vertices that fall on or under the chord to the new point.
		for n := len(h.Cost); n >= 2; n = len(h.Cost) {
			x1, y1, x2, y2 := h.Cost[n-2], h.Acc[n-2], h.Cost[n-1], h.Acc[n-1]
			if (y2-y1)*(x-x1) > (y-y1)*(x2-x1) {
				break
			}
			h.Cost, h.Acc = h.Cost[:n-1], h.Acc[:n-1]
		}
		h.Cost, h.Acc = append(h.Cost, x), append(h.Acc, y)
	}
	return h
}

// At is the hull's accuracy at a cost, held at its ends outside them.
func (h Hull) At(cost float64) float64 {
	n := len(h.Cost)
	switch {
	case n == 0:
		return 0
	case cost <= h.Cost[0]:
		return h.Acc[0]
	case cost >= h.Cost[n-1]:
		return h.Acc[n-1]
	}
	for i := 1; i < n; i++ {
		if cost <= h.Cost[i] {
			return h.Acc[i-1] + (h.Acc[i]-h.Acc[i-1])*(cost-h.Cost[i-1])/(h.Cost[i]-h.Cost[i-1])
		}
	}
	return h.Acc[n-1]
}

// AUCGain is the mean of frontier minus ref over the line's cost range,
// with the frontier drawn through its points in cost order (the most
// accurate point where several share a cost).
func AUCGain(pts []FrontierPoint, l Line, ref Reference) float64 {
	if len(pts) == 0 || l.BestCost <= l.CheapCost {
		return 0
	}
	type xy struct{ x, y float64 }
	var curve []xy
	for _, p := range pts {
		curve = append(curve, xy{p.CostPer1K, p.Accuracy})
	}
	slices.SortStableFunc(curve, func(a, b xy) int { return cmp.Compare(a.x, b.x) })
	merged := curve[:1]
	for _, c := range curve[1:] {
		if c.x == merged[len(merged)-1].x {
			merged[len(merged)-1].y = math.Max(merged[len(merged)-1].y, c.y)
			continue
		}
		merged = append(merged, c)
	}
	f := func(x float64) float64 {
		if x <= merged[0].x {
			return merged[0].y
		}
		for i := 1; i < len(merged); i++ {
			if x <= merged[i].x {
				a, b := merged[i-1], merged[i]
				return a.y + (b.y-a.y)*(x-a.x)/(b.x-a.x)
			}
		}
		return merged[len(merged)-1].y
	}
	// A fine fixed grid keeps the integral simple and deterministic.
	const steps = 1000
	var sum float64
	for i := range steps + 1 {
		x := l.CheapCost + (l.BestCost-l.CheapCost)*float64(i)/steps
		w := 1.0
		if i == 0 || i == steps {
			w = 0.5
		}
		sum += w * (f(x) - ref.At(x))
	}
	return sum / steps
}

// hullFor builds the candidates' hull on sample.
func (t *Table) hullFor(sample []int) Hull {
	n := len(t.Questions)
	cost := make([]float64, len(t.Models))
	acc := make([]float64, len(t.Models))
	for m := range t.Models {
		acc[m], cost[m] = t.Evaluate(constant(n, m), sample)
	}
	return NewHull(cost, acc)
}

// Wilson is the Wilson score 95% interval of k successes in n.
func Wilson(k, n int) [2]float64 {
	if n == 0 {
		return [2]float64{0, 1}
	}
	const z = 1.959963984540054
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return [2]float64{math.Max(0, centre-half), math.Min(1, centre+half)}
}

// AUROC is the area under the ROC curve of scores against labels: the
// chance a random positive scores above a random negative, ties counting
// half. It is NaN when either class is empty.
func AUROC(score []float64, label []bool) float64 {
	idx := make([]int, len(score))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmp.Compare(score[a], score[b]) })
	// Mann-Whitney U with average ranks for ties.
	var pos, neg, rankSum float64
	for i := 0; i < len(idx); {
		j := i
		for j < len(idx) && score[idx[j]] == score[idx[i]] {
			j++
		}
		avg := float64(i+j+1) / 2 // ranks i+1 .. j
		for k := i; k < j; k++ {
			if label[idx[k]] {
				rankSum += avg
				pos++
			} else {
				neg++
			}
		}
		i = j
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	return (rankSum - pos*(pos+1)/2) / (pos * neg)
}

// costRidge is the regularisation of the per-candidate cost regression,
// on standardised cheap features.
const costRidge = 1.0

// PredictCosts estimates each candidate's cost on every question from its
// cheap features, out of fold: a ridge regression per candidate, fitted on
// the training folds. The serving rule orders candidates by their mean
// cost; this asks whether knowing that a candidate is cheap on this
// question (a multiple-choice reply is a few tokens, a worked solution
// hundreds) would route better.
func PredictCosts(t *Table, k int, seed uint64) ([][]float64, error) {
	n, nm := len(t.Questions), len(t.Models)
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	fold := Folds(t.Questions, all, k, seed)
	out := make([][]float64, n)
	for f := range k {
		var train []int
		for i := range n {
			if fold[i] != f {
				train = append(train, i)
			}
		}
		xs := make([][]float64, len(train))
		for i, q := range train {
			xs[i] = t.Cheap[q]
		}
		sc := router.FitScaler(xs)
		for i := range xs {
			xs[i] = sc.Apply(xs[i])
		}
		coef := make([][]float64, nm)
		for m := range nm {
			y := make([]float64, len(train))
			for i, q := range train {
				y[i] = t.Cost[q][m]
			}
			c, err := ridge(xs, y, costRidge)
			if err != nil {
				return nil, fmt.Errorf("fold %d, %s: %w", f, t.Models[m], err)
			}
			coef[m] = c
		}
		for q := range n {
			if fold[q] != f {
				continue
			}
			x := sc.Apply(t.Cheap[q])
			out[q] = make([]float64, nm)
			for m := range nm {
				v := coef[m][0]
				for j, xj := range x {
					v += coef[m][j+1] * xj
				}
				out[q][m] = math.Max(v, 0)
			}
		}
	}
	return out, nil
}

// ridge solves min ‖y − b − Xw‖² + l2‖w‖² (intercept b unpenalised) by
// the normal equations; X has a handful of columns.
func ridge(xs [][]float64, y []float64, l2 float64) ([]float64, error) {
	d := len(xs[0]) + 1
	a := make([][]float64, d)
	for i := range a {
		a[i] = make([]float64, d+1)
	}
	for r, x := range xs {
		row := append([]float64{1}, x...)
		for i := range d {
			for j := range d {
				a[i][j] += row[i] * row[j]
			}
			a[i][d] += row[i] * y[r]
		}
	}
	for i := 1; i < d; i++ {
		a[i][i] += l2
	}
	// Gaussian elimination with partial pivoting.
	for c := range d {
		p := c
		for r := c + 1; r < d; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[p][c]) {
				p = r
			}
		}
		if math.Abs(a[p][c]) < 1e-12 {
			return nil, errors.New("ridge: singular system")
		}
		a[c], a[p] = a[p], a[c]
		for r := range d {
			if r == c {
				continue
			}
			f := a[r][c] / a[c][c]
			for j := c; j <= d; j++ {
				a[r][j] -= f * a[c][j]
			}
		}
	}
	w := make([]float64, d)
	for i := range d {
		w[i] = a[i][d] / a[i][i]
	}
	return w, nil
}

// RouteUtility sends each question to the candidate with the highest
// P(correct) − weight × predicted cost / scale, ties to the cheaper by
// predicted cost. Weight 0 picks the likeliest to be right; a large weight
// the cheapest on this question.
func (p *Prediction) RouteUtility(costHat [][]float64, weight, scale float64) []int {
	out := make([]int, len(p.Prob))
	for q := range p.Prob {
		best, bestU := -1, math.Inf(-1)
		for m := range p.Prob[q] {
			u := p.Prob[q][m] - weight*costHat[q][m]/scale
			if u > bestU || (u == bestU && costHat[q][m] < costHat[q][best]) {
				best, bestU = m, u
			}
		}
		out[q] = best
	}
	return out
}

// CostWeights is the cost-weight grid swept for the cost-aware rule: 0,
// then log-spaced from 10⁻³ to 10².
func CostWeights() []float64 {
	out := []float64{0}
	for i := 0; i <= 100; i++ {
		out = append(out, round6(math.Pow(10, -3+5*float64(i)/100)))
	}
	return out
}

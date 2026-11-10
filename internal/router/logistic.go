package router

import (
	"errors"
	"fmt"
	"math"
)

// Scaler standardises features: (x - Mean) / Std per column. Without it
// the L2 penalty would punish features by their units rather than by how
// much they matter, and log-counts would swamp the unit-length embedding.
type Scaler struct {
	Mean []float64 `json:"mean"`
	Std  []float64 `json:"std"`
}

// FitScaler computes each column's mean and population standard
// deviation. A constant column gets Std 1, so it scales to 0 everywhere
// and carries no weight, instead of dividing by zero.
func FitScaler(rows [][]float64) Scaler {
	if len(rows) == 0 {
		return Scaler{Mean: []float64{}, Std: []float64{}}
	}
	d := len(rows[0])
	s := Scaler{Mean: make([]float64, d), Std: make([]float64, d)}
	n := float64(len(rows))
	for _, row := range rows {
		for j := range d {
			s.Mean[j] += row[j]
		}
	}
	for j := range d {
		s.Mean[j] /= n
	}
	for _, row := range rows {
		for j := range d {
			dv := row[j] - s.Mean[j]
			s.Std[j] += dv * dv
		}
	}
	for j := range d {
		s.Std[j] = math.Sqrt(s.Std[j] / n)
		// A spread this small relative to the mean is rounding noise in a
		// constant column, not signal.
		if s.Std[j] <= 1e-12*math.Max(1, math.Abs(s.Mean[j])) {
			s.Std[j] = 1
		}
	}
	return s
}

// Apply returns a new, scaled copy of x. x must have the scaler's length.
func (s Scaler) Apply(x []float64) []float64 {
	out := make([]float64, len(x))
	for j := range x {
		out[j] = (x[j] - s.Mean[j]) / s.Std[j]
	}
	return out
}

// Logistic is a fitted logistic regression on scaled features.
type Logistic struct {
	Weights []float64 `json:"weights"`
	Bias    float64   `json:"bias"`
}

// Prob is sigmoid(w·x + b).
func (m Logistic) Prob(x []float64) float64 {
	return sigmoid(m.score(x))
}

func (m Logistic) score(x []float64) float64 {
	z := m.Bias
	for j, w := range m.Weights {
		z += w * x[j]
	}
	return z
}

// sigmoid is written in two branches so that exp never overflows.
func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

// softplus is log(1 + e^z) without overflow.
func softplus(z float64) float64 {
	return math.Max(z, 0) + math.Log1p(math.Exp(-math.Abs(z)))
}

// TrainOptions configures Train.
type TrainOptions struct {
	// L2 is the penalty strength λ in Σ logloss + λ/2·||w||². The loss is
	// summed over rows, not averaged, so λ = 1/C in scikit-learn's terms
	// and the same λ regularises less as the training set grows. Must be
	// >= 0; 0 can diverge on separable data and is only for tests.
	L2 float64
	// MaxIter caps the Newton iterations; 0 means 100.
	MaxIter int
	// Tol stops the fit when the largest parameter change in an iteration
	// falls below it; 0 means 1e-8.
	Tol float64
}

// ErrBadTrainingData is returned by Train for inputs it cannot fit.
var ErrBadTrainingData = errors.New("router: bad training data")

// Train fits an L2-regularised logistic regression on already-scaled rows,
// predicting y. The bias is not penalised.
//
// It uses Newton's method with a backtracking line search: it converges
// in a handful of iterations to the exact optimum, so the result does not
// depend on a learning rate or on how long it ran. How the Newton system
// is solved depends on the shape of the data (n rows, d features):
//
//   - With at least as many rows as features (or L2 = 0), the (d+1)×(d+1)
//     Hessian is formed and Cholesky-factored.
//   - With fewer rows than features, the usual case here (a few hundred
//     prompts, hundreds of embedding dimensions), the system is solved
//     through the Woodbury identity in the n×n row space, with the
//     unpenalised bias handled as a bordered system. The n×n Gram matrix
//     XXᵀ is computed once per call, so an iteration costs O(n³ + nd)
//     instead of O(nd² + d³).
//
// If a system is not numerically positive definite, growing jitter is
// added to its diagonal, and if that fails too, the step is a plain
// gradient step.
//
// With all labels the same there is nothing to separate: Train returns
// zero weights and the bias at the logit of the smoothed rate
// (k + 0.5) / (n + 1), so the prediction is confident but never 0 or 1.
//
// The result is deterministic: the same input gives bit-identical output.
func Train(rows [][]float64, y []bool, opt TrainOptions) (Logistic, error) {
	wide := len(rows) > 0 && opt.L2 > 0 && len(rows) < len(rows[0])
	return train(rows, y, opt, wide)
}

// train is Train with the choice of Newton solver made by the caller, so
// tests can check that the two solvers agree.
func train(rows [][]float64, y []bool, opt TrainOptions, wide bool) (Logistic, error) {
	n := len(rows)
	if n == 0 {
		return Logistic{}, fmt.Errorf("%w: no rows", ErrBadTrainingData)
	}
	if len(y) != n {
		return Logistic{}, fmt.Errorf("%w: %d rows but %d labels", ErrBadTrainingData, n, len(y))
	}
	if opt.L2 < 0 || math.IsNaN(opt.L2) || math.IsInf(opt.L2, 0) {
		return Logistic{}, fmt.Errorf("%w: L2 %v", ErrBadTrainingData, opt.L2)
	}
	if opt.MaxIter <= 0 {
		opt.MaxIter = 100
	}
	if opt.Tol <= 0 {
		opt.Tol = 1e-8
	}
	d := len(rows[0])
	for i, row := range rows {
		if len(row) != d {
			return Logistic{}, fmt.Errorf("%w: row %d has %d features, row 0 has %d", ErrBadTrainingData, i, len(row), d)
		}
		for j, v := range row {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return Logistic{}, fmt.Errorf("%w: row %d feature %d is %v", ErrBadTrainingData, i, j, v)
			}
		}
	}

	pos := 0
	for _, v := range y {
		if v {
			pos++
		}
	}
	if pos == 0 || pos == n {
		rate := (float64(pos) + 0.5) / (float64(n) + 1)
		return Logistic{Weights: make([]float64, d), Bias: math.Log(rate / (1 - rate))}, nil
	}

	// theta is (w, b); the bias is the last entry.
	p := d + 1
	theta := make([]float64, p)
	rate := float64(pos) / float64(n)
	theta[d] = math.Log(rate / (1 - rate))

	obj := func(th []float64) float64 {
		m := Logistic{Weights: th[:d], Bias: th[d]}
		var f float64
		for i, row := range rows {
			z := m.score(row)
			f += softplus(z)
			if y[i] {
				f -= z
			}
		}
		for j := range d {
			f += opt.L2 / 2 * th[j] * th[j]
		}
		return f
	}

	var solve newtonSolver
	if wide {
		solve = newDualSolver(rows, opt.L2)
	} else {
		solve = &primalSolver{rows: rows, l2: opt.L2, hess: make([]float64, p*p)}
	}

	grad := make([]float64, p)
	s := make([]float64, n)
	step := make([]float64, p)
	trial := make([]float64, p)
	f := obj(theta)
	for range opt.MaxIter {
		// Gradient Σ (p_i - y_i)·x̃_i + λ[w;0], where x̃ is x with a
		// trailing 1, and the Hessian's row weights s_i = p_i(1 - p_i).
		clear(grad)
		m := Logistic{Weights: theta[:d], Bias: theta[d]}
		for i, row := range rows {
			pi := sigmoid(m.score(row))
			r := pi
			if y[i] {
				r--
			}
			s[i] = pi * (1 - pi)
			for j, v := range row {
				grad[j] += r * v
			}
			grad[d] += r
		}
		for j := range d {
			grad[j] += opt.L2 * theta[j]
		}

		if !solve.step(s, grad, step) {
			copy(step, grad)
		}
		for j := range step {
			step[j] = -step[j]
		}

		// Backtrack until the objective does not increase. A Newton step
		// on a convex objective is a descent direction, so this ends.
		t := 1.0
		var ft float64
		for range 60 {
			for j := range theta {
				trial[j] = theta[j] + t*step[j]
			}
			ft = obj(trial)
			if ft <= f {
				break
			}
			t /= 2
		}
		if ft > f {
			break // no progress possible in floating point
		}
		maxChange := 0.0
		for j := range theta {
			maxChange = math.Max(maxChange, math.Abs(trial[j]-theta[j]))
		}
		copy(theta, trial)
		f = ft
		if maxChange < opt.Tol {
			break
		}
	}

	for _, v := range theta {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Logistic{}, fmt.Errorf("%w: fit diverged", ErrBadTrainingData)
		}
	}
	w := make([]float64, d)
	copy(w, theta[:d])
	return Logistic{Weights: w, Bias: theta[d]}, nil
}

// newtonSolver solves H·step = g for the Newton step, given the row
// weights s_i = p_i(1 - p_i) that define H = Σ s_i·x̃_i x̃_iᵀ + λ·diag(1..1,0).
// It reports false when it cannot.
type newtonSolver interface {
	step(s, g, out []float64) bool
}

// primalSolver forms the (d+1)×(d+1) Hessian.
type primalSolver struct {
	rows [][]float64
	l2   float64
	hess []float64
	l    []float64
}

func (ps *primalSolver) step(s, g, out []float64) bool {
	d := len(ps.rows[0])
	p := d + 1
	h := ps.hess
	clear(h)
	// Only the upper triangle is accumulated, then mirrored.
	for i, row := range ps.rows {
		si := s[i]
		for a, xa := range row {
			if xa == 0 {
				continue
			}
			sa := si * xa
			ha := h[a*p:]
			for b := a; b < d; b++ {
				ha[b] += sa * row[b]
			}
			ha[d] += sa
		}
		h[d*p+d] += si
	}
	for j := range d {
		h[j*p+j] += ps.l2
	}
	for a := range p {
		for b := a + 1; b < p; b++ {
			h[b*p+a] = h[a*p+b]
		}
	}
	if ps.l == nil {
		ps.l = make([]float64, p*p)
	}
	if !factor(h, ps.l, p) {
		return false
	}
	solveCholesky(ps.l, g, out, p)
	return true
}

// dualSolver solves the Newton system in the n-dimensional row space,
// for n < d. Write the Hessian as the bordered matrix
//
//	H = [A  c; cᵀ  σ₀],  A = XᵀSX + λI,  c = XᵀS1,  σ₀ = Σ s_i.
//
// By Woodbury, with Z = S^½X,
//
//	A⁻¹v = (v − Zᵀ(λI + ZZᵀ)⁻¹Zv) / λ,
//
// and ZZᵀ = S^½ K S^½ for the fixed Gram matrix K = XXᵀ, so each
// iteration factors only an n×n matrix. The bordered system is then
// solved by its Schur complement σ₀ − cᵀA⁻¹c.
type dualSolver struct {
	rows [][]float64
	l2   float64
	k    []float64 // n×n Gram matrix XXᵀ
	m    []float64 // n×n λI + ZZᵀ
	l    []float64 // its Cholesky factor
	sq   []float64 // √s_i
	t    []float64 // n-vector scratch
	q    []float64 // n-vector scratch
	c    []float64
	u    []float64
	v    []float64
}

func newDualSolver(rows [][]float64, l2 float64) *dualSolver {
	n, d := len(rows), len(rows[0])
	ds := &dualSolver{
		rows: rows, l2: l2,
		k: make([]float64, n*n), m: make([]float64, n*n), l: make([]float64, n*n),
		sq: make([]float64, n), t: make([]float64, n), q: make([]float64, n),
		c: make([]float64, d), u: make([]float64, d), v: make([]float64, d),
	}
	for i := range n {
		for j := 0; j <= i; j++ {
			kij := dot(rows[i], rows[j])
			ds.k[i*n+j] = kij
			ds.k[j*n+i] = kij
		}
	}
	return ds
}

func (ds *dualSolver) step(s, g, out []float64) bool {
	n, d := len(ds.rows), len(ds.rows[0])
	var sigma0 float64
	for i, si := range s {
		ds.sq[i] = math.Sqrt(si)
		sigma0 += si
	}
	for i := range n {
		for j := range n {
			ds.m[i*n+j] = ds.sq[i] * ds.sq[j] * ds.k[i*n+j]
		}
		ds.m[i*n+i] += ds.l2
	}
	if !factor(ds.m, ds.l, n) {
		return false
	}

	clear(ds.c)
	for i, row := range ds.rows {
		for j, x := range row {
			ds.c[j] += s[i] * x
		}
	}
	ds.applyAinv(g[:d], ds.u)
	ds.applyAinv(ds.c, ds.v)

	schur := sigma0 - dot(ds.c, ds.v)
	if !(schur > 1e-12*sigma0) {
		return false
	}
	db := (g[d] - dot(ds.c, ds.u)) / schur
	for j := range d {
		out[j] = ds.u[j] - ds.v[j]*db
	}
	out[d] = db
	return true
}

// applyAinv sets out = A⁻¹·in using the current factor of λI + ZZᵀ.
func (ds *dualSolver) applyAinv(in, out []float64) {
	for i, row := range ds.rows {
		ds.t[i] = ds.sq[i] * dot(row, in)
	}
	solveCholesky(ds.l, ds.t, ds.q, len(ds.rows))
	copy(out, in)
	for i, row := range ds.rows {
		a := ds.sq[i] * ds.q[i]
		for j, x := range row {
			out[j] -= a * x
		}
	}
	for j := range out {
		out[j] /= ds.l2
	}
}

// dot is the inner product of a and b (len(b) >= len(a)). Four
// independent partial sums let the CPU overlap the multiply-adds; this
// loop dominates training time. The summation order is fixed, so results
// are still bit-for-bit reproducible.
func dot(a, b []float64) float64 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

// factor Cholesky-factors h (p×p) into l, adding growing jitter to the
// diagonal when h is not numerically positive definite. It reports false
// when no jitter up to h's largest diagonal entry helps.
func factor(h, l []float64, p int) bool {
	scale := 0.0
	for a := range p {
		scale = math.Max(scale, h[a*p+a])
	}
	if scale == 0 {
		scale = 1
	}
	for jitter := 0.0; jitter <= scale; {
		if cholesky(h, l, p, jitter) {
			return true
		}
		if jitter == 0 {
			jitter = 1e-10 * scale
		} else {
			jitter *= 10
		}
	}
	return false
}

// cholesky factors h + jitter·I = L·Lᵀ into the lower triangle of l.
func cholesky(h, l []float64, p int, jitter float64) bool {
	clear(l)
	for i := range p {
		for j := 0; j <= i; j++ {
			sum := h[i*p+j]
			if i == j {
				sum += jitter
			}
			sum -= dot(l[i*p:i*p+j], l[j*p:j*p+j])
			if i == j {
				if sum <= 0 || math.IsNaN(sum) {
					return false
				}
				l[i*p+i] = math.Sqrt(sum)
			} else {
				l[i*p+j] = sum / l[j*p+j]
			}
		}
	}
	return true
}

// solveCholesky solves L·Lᵀ·x = b.
func solveCholesky(l, b, x []float64, p int) {
	for i := range p {
		sum := b[i]
		for k := range i {
			sum -= l[i*p+k] * x[k]
		}
		x[i] = sum / l[i*p+i]
	}
	for i := p - 1; i >= 0; i-- {
		sum := x[i]
		for k := i + 1; k < p; k++ {
			sum -= l[k*p+i] * x[k]
		}
		x[i] = sum / l[i*p+i]
	}
}

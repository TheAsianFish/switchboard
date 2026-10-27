package cacheeval

import (
	"fmt"
	"math"
	"slices"
)

// z95 is the standard normal quantile for a two-sided 95% interval.
const z95 = 1.959964

// The threshold grid: thresholdMin to fineFrom in steps of thresholdStep,
// then to 1 in steps of fineStep. Paraphrases and near-misses of one
// prompt both score above 0.97 with small embedding models, so the top
// of the range is where any separation shows and needs the finer grid.
const (
	thresholdMin    = 0.5
	thresholdStep   = 0.0025
	thresholdSteps  = 196 // (fineFrom - thresholdMin) / thresholdStep
	fineFrom        = 0.99
	fineStep        = 0.0001
	fineSteps       = 100 // (1 - fineFrom) / fineStep
	thresholdPoints = thresholdSteps + fineSteps + 1
)

// ChosenRule is the selection rule for the shipped default, in words.
const ChosenRule = "For each embedding model, the joint threshold with the number guard on (the lowest threshold whose Wilson 95% upper bound on the false-hit rate is at most 1% on every pair set); the model with the higher mean hit rate at its joint threshold; ties go to the smaller model (fewer dimensions)."

// noModelRule is appended to ChosenRule when no model meets it.
const noModelRule = " No model met the target on every pair set, so semantic matching ships off (threshold 1, no embedding model) and only the exact cache serves hits."

// Wilson returns k of n with its Wilson score 95% interval, rounded to
// six decimals. n = 0 gives p 0 and the uninformative interval [0, 1].
func Wilson(k, n int) Rate {
	p, low, high := wilson(k, n)
	return Rate{K: k, N: n, P: round6(p), Low: round6(low), High: round6(high)}
}

// wilson is Wilson unrounded, for comparisons against the target.
func wilson(k, n int) (p, low, high float64) {
	if n == 0 {
		return 0, 0, 1
	}
	nf := float64(n)
	p = float64(k) / nf
	z2 := z95 * z95
	den := 1 + z2/nf
	centre := (p + z2/(2*nf)) / den
	half := z95 * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / den
	low, high = math.Max(0, centre-half), math.Min(1, centre+half)
	// The bounds at the ends are exact; keep float error out of them.
	if k == 0 {
		low = 0
	}
	if k == n {
		high = 1
	}
	return p, low, high
}

// Thresholds is the sweep grid: 0.5000 to 0.9900 in steps of 0.0025,
// then to 1.0000 in steps of 0.0001. Each value is computed from its
// integer step and rounded to four decimals, so the grid has no
// accumulated floating-point drift.
func Thresholds() []float64 {
	out := make([]float64, 0, thresholdPoints)
	for i := range thresholdSteps {
		out = append(out, math.Round((thresholdMin+float64(i)*thresholdStep)*1e4)/1e4)
	}
	for i := range fineSteps + 1 {
		out = append(out, math.Round((fineFrom+float64(i)*fineStep)*1e4)/1e4)
	}
	return out
}

// hit reports whether a semantic cache with threshold t (and the number
// guard if guard) would serve one side's answer for the other.
func hit(p SimPair, t float64, guard bool) bool {
	return p.Cos >= t && (!guard || p.NumbersEqual)
}

// SweepCurve sweeps the threshold grid over one model's pairs for one set.
// target is the false-hit rate the operating points must meet.
func SweepCurve(model, set string, pairs []SimPair, guard bool, target float64) Curve {
	c := Curve{Model: model, Set: set, Guard: guard}
	for _, p := range pairs {
		if p.Same {
			c.Positives++
		} else {
			c.Negatives++
		}
	}
	for _, t := range Thresholds() {
		var hits, falseHits int
		for _, p := range pairs {
			if !hit(p, t, guard) {
				continue
			}
			if p.Same {
				hits++
			} else {
				falseHits++
			}
		}
		pt := SweepPoint{Threshold: t, HitRate: Wilson(hits, c.Positives), FalseHitRate: Wilson(falseHits, c.Negatives)}
		c.Points = append(c.Points, pt)
		fhr, _, upper := wilson(falseHits, c.Negatives)
		if !c.AtPoint.Found && fhr <= target {
			c.AtPoint = Operating{Found: true, SweepPoint: pt}
		}
		if !c.AtUpper.Found && upper <= target {
			c.AtUpper = Operating{Found: true, SweepPoint: pt}
		}
	}
	return c
}

// BaselineOf is the exact-match cache on one set: trimmed and normalised.
func BaselineOf(set string, pairs []SimPair) Baseline {
	var pos, neg, exactHit, exactFalse, normHit, normFalse int
	for _, p := range pairs {
		if p.Same {
			pos++
			exactHit += b2i(p.Exact)
			normHit += b2i(p.Norm)
		} else {
			neg++
			exactFalse += b2i(p.Exact)
			normFalse += b2i(p.Norm)
		}
	}
	return Baseline{
		Set:                    set,
		ExactHitRate:           Wilson(exactHit, pos),
		ExactFalseHitRate:      Wilson(exactFalse, neg),
		NormalisedHitRate:      Wilson(normHit, pos),
		NormalisedFalseHitRate: Wilson(normFalse, neg),
	}
}

// JointOf combines one model's curves (one per set, same guard) into one
// threshold: the highest per-set AtUpper threshold, so the target holds
// on every set. It is not found if any set has no AtUpper threshold.
func JointOf(model string, guard bool, curves []Curve) Joint {
	j := Joint{Model: model, Guard: guard}
	if len(curves) == 0 {
		return j
	}
	for _, c := range curves {
		if !c.AtUpper.Found {
			return j
		}
		j.Threshold = math.Max(j.Threshold, c.AtUpper.Threshold)
	}
	var sum float64
	for _, c := range curves {
		i := slices.IndexFunc(c.Points, func(p SweepPoint) bool { return p.Threshold == j.Threshold })
		if i < 0 {
			// Every curve is swept over the same grid, so this cannot
			// happen for curves SweepCurve built.
			return Joint{Model: model, Guard: guard}
		}
		j.PerSet = append(j.PerSet, c.Points[i])
		sum += c.Points[i].HitRate.P
	}
	j.Found = true
	j.MeanHitRate = round6(sum / float64(len(curves)))
	return j
}

// Choose applies ChosenRule to the guarded joints. dims gives each model's
// vector length; models fixes the order for a tie the rule does not break.
func Choose(joints []Joint, dims map[string]int, models []string) Chosen {
	var best *Joint
	for _, m := range models {
		for i := range joints {
			j := &joints[i]
			if j.Model != m || !j.Guard || !j.Found {
				continue
			}
			if best == nil || j.MeanHitRate > best.MeanHitRate ||
				(j.MeanHitRate == best.MeanHitRate && dims[j.Model] < dims[best.Model]) {
				best = j
			}
		}
	}
	if best == nil {
		return Chosen{Threshold: 1, Guard: true, Rule: ChosenRule + noModelRule}
	}
	return Chosen{Model: best.Model, Threshold: best.Threshold, Guard: true, Rule: ChosenRule}
}

// Summarise computes the whole Q3 summary from the raw similarity files.
// models and sets fix the output order; every (model, set) must have
// exactly one Sims. The result depends only on its arguments.
func Summarise(sims []Sims, models, sets []string, target float64, latency []Latency, data DataMeta, info RunInfo) (Summary, error) {
	s := Summary{
		RunInfo: info, Target: target, Step: thresholdStep, FineFrom: fineFrom, FineStep: fineStep,
		Models: models, Sets: sets, Latency: latency, Data: data,
	}
	find := func(model, set string) (*Sims, error) {
		var found *Sims
		for i := range sims {
			if sims[i].Model == model && sims[i].Set == set {
				if found != nil {
					return nil, fmt.Errorf("summarise: two similarity files for %s on %s", model, set)
				}
				found = &sims[i]
			}
		}
		if found == nil {
			return nil, fmt.Errorf("summarise: no similarity file for %s on %s", model, set)
		}
		return found, nil
	}
	dims := map[string]int{}
	for mi, m := range models {
		for _, guard := range []bool{false, true} {
			var curves []Curve
			for _, set := range sets {
				sm, err := find(m, set)
				if err != nil {
					return Summary{}, err
				}
				dims[m] = sm.Dims
				if mi == 0 && !guard {
					// The baseline does not depend on the model; the
					// first model's pairs carry the flags.
					s.Baselines = append(s.Baselines, BaselineOf(set, sm.Pairs))
				}
				curves = append(curves, SweepCurve(m, set, sm.Pairs, guard, target))
				if !guard {
					s.Spreads = append(s.Spreads, SpreadOf(m, set, sm.Pairs))
				}
			}
			s.Curves = append(s.Curves, curves...)
			s.Joints = append(s.Joints, JointOf(m, guard, curves))
		}
	}
	s.Chosen = Choose(s.Joints, dims, models)
	return s, nil
}

// SpreadOf summarises where the positives' and negatives' cosines fall.
func SpreadOf(model, set string, pairs []SimPair) Spread {
	var pos, neg []float64
	for _, p := range pairs {
		if p.Same {
			pos = append(pos, p.Cos)
		} else {
			neg = append(neg, p.Cos)
		}
	}
	return Spread{Model: model, Set: set, Positives: quantiles(pos), Negatives: quantiles(neg)}
}

// quantiles are nearest-rank: the q-quantile of n sorted values is the
// value at rank ceil(q·n).
func quantiles(v []float64) Quantiles {
	if len(v) == 0 {
		return Quantiles{}
	}
	slices.Sort(v)
	at := func(q float64) float64 {
		i := int(math.Ceil(q*float64(len(v)))) - 1
		return v[max(i, 0)]
	}
	return Quantiles{P10: at(0.1), P50: at(0.5), P90: at(0.9), Max: v[len(v)-1]}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

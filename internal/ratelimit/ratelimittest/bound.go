package ratelimittest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

// Step is one event of a random trace: wait Gap, then either ask for Cost
// units or, when Adjust is set, adjust by Delta.
type Step struct {
	Gap    time.Duration
	Cost   int64
	Adjust bool
	Delta  int64
}

// Trace returns a seeded random trace for l of n steps. Gaps are whole
// microseconds and mostly shorter than the emission interval, so the
// offered load is a few times the sustained rate, with simultaneous
// arrivals and the occasional idle stretch of a period or more. Costs are
// mostly 1 and otherwise uniform in 1..Burst+1, so impossible costs occur
// too. With adjust, about one step in ten is an Adjust by a nonzero delta
// in -Burst..Burst.
func Trace(l ratelimit.Limit, seed uint64, n int, adjust bool) []Step {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	T := max(l.Interval().Microseconds(), 1)
	P := max(l.Period.Microseconds(), 1)
	steps := make([]Step, n)
	for i := range steps {
		var gap int64
		switch r := rng.IntN(1000); {
		case r < 150:
			gap = 0
		case r < 650:
			gap = rng.Int64N(T/2 + 1)
		case r < 950:
			gap = rng.Int64N(2*T + 1)
		case r < 995:
			gap = rng.Int64N(P/4 + 1)
		default:
			gap = P + rng.Int64N(P+1)
		}
		s := Step{Gap: time.Duration(gap) * time.Microsecond, Cost: 1}
		if rng.IntN(4) == 0 {
			s.Cost = 1 + rng.Int64N(l.Burst+1)
		}
		if adjust && rng.IntN(10) == 0 {
			s.Adjust = true
			for s.Delta == 0 {
				s.Delta = rng.Int64N(2*l.Burst+1) - l.Burst
			}
		}
		steps[i] = s
	}
	return steps
}

// Violation is a window in which a limiter admitted more than Limit.Bound.
type Violation struct {
	From, To time.Time
	Admitted int64
	Bound    int64
}

func (v *Violation) String() string {
	return fmt.Sprintf("%d units admitted in [%s, %s] (%v), bound %d",
		v.Admitted, v.From.Format(time.RFC3339Nano), v.To.Format(time.RFC3339Nano), v.To.Sub(v.From), v.Bound)
}

// FindBoundViolation replays Trace(l, seed, steps, false) against h from
// Start on key and returns the first window found in which the admitted
// units exceed l.Bound of the window's length, or nil. Only windows that
// start and end on an admission need checking: any other window can be
// shrunk to one without losing admissions or growing its bound. h.Set
// must not be nil.
func FindBoundViolation(ctx context.Context, h Harness, l ratelimit.Limit, key string, seed uint64, steps int) (*Violation, error) {
	if h.Set == nil {
		return nil, fmt.Errorf("ratelimittest: bound check needs a settable clock")
	}
	type admission struct {
		at   time.Time
		cost int64
	}
	var admitted []admission
	now := Start
	h.Set(now)
	for i, s := range Trace(l, seed, steps, false) {
		now = now.Add(s.Gap)
		h.Set(now)
		dec, err := h.Limiter.Allow(ctx, key, l, s.Cost)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		if dec.Allowed {
			admitted = append(admitted, admission{now, s.Cost})
		}
	}
	prefix := make([]int64, len(admitted)+1)
	for i, a := range admitted {
		prefix[i+1] = prefix[i] + a.cost
	}
	for i := range admitted {
		for j := i; j < len(admitted); j++ {
			sum := prefix[j+1] - prefix[i]
			if b := l.Bound(admitted[j].at.Sub(admitted[i].at)); sum > b {
				return &Violation{From: admitted[i].at, To: admitted[j].at, Admitted: sum, Bound: b}, nil
			}
		}
	}
	return nil, nil
}

// boundLimits are the limits RunBound and RunOracle replay traces under.
var boundLimits = []ratelimit.Limit{
	{Rate: 100, Period: time.Minute, Burst: 20}, // the Q2 limit, T = 600 ms
	{Rate: 10, Period: time.Second, Burst: 3},   // T = 100 ms
	{Rate: 5, Period: 10 * time.Second, Burst: 1},
	{Rate: 7, Period: 7 * time.Millisecond, Burst: 7}, // T = 1 ms
}

// Seeds and trace length of RunBound and RunOracle: enough to cover
// boundaries, idle refills, debt and impossible costs on every limit,
// small enough to stay fast against miniredis under -race.
const (
	traces     = 12
	traceSteps = 200
)

// RunBound checks the conformance property only GCRA meets: over seeded
// random traces, the units admitted in every window of length W are at
// most Limit.Bound(W). Each trace gets a fresh harness.
func RunBound(t *testing.T, name string, newHarness func(t *testing.T) Harness) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		for _, l := range boundLimits {
			t.Run(limitName(l), func(t *testing.T) {
				for seed := uint64(1); seed <= traces; seed++ {
					h := newHarness(t)
					if h.Set == nil {
						t.Skipf("%s: needs a settable clock", name)
					}
					v, err := FindBoundViolation(context.Background(), h, l, fmt.Sprintf("bound-%d", seed), seed, traceSteps)
					d := &driver{t: t, name: name, h: h}
					d.check(err, fmt.Sprintf("seed %d", seed))
					if v != nil {
						t.Fatalf("%s: seed %d: %s", name, seed, v)
					}
				}
			})
		}
	})
}

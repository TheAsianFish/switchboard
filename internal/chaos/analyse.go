package chaos

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/limitseval"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// PhaseSteady names the phase before the first event, the reference for
// the steady-state rule.
const PhaseSteady = "steady"

// Stream is one load stream's raw files.
type Stream struct {
	Result  bench.Result
	Records []bench.Record
}

// Input is everything a run left behind.
type Input struct {
	Meta    Meta
	Events  []Event
	Streams map[string]*Stream
	Ledger  []fake.Entry
}

// Analyse computes the summary of a run under rule.
func Analyse(in Input, rule Rule) (*Summary, error) {
	main := in.Streams[StreamMain]
	if main == nil {
		return nil, errors.New("chaos: no main stream")
	}
	if rule.BucketSeconds <= 0 || rule.Consecutive < 1 {
		return nil, fmt.Errorf("chaos: bad rule %+v", rule)
	}
	start := main.Result.StartedAt
	end := main.Result.Config.DurationSec
	if end <= 0 {
		return nil, errors.New("chaos: main stream has no duration")
	}
	s := &Summary{Meta: in.Meta, Hardware: main.Result.Hardware, Rule: rule}
	events := slices.Clone(in.Events)
	slices.SortStableFunc(events, func(a, b Event) int { return cmpFloat(a.IssuedUnix, b.IssuedUnix) })
	s.Phases = phases(in.Meta, events, start, end)
	for _, e := range events {
		s.Events = append(s.Events, EventResult{Event: e,
			AtSeconds: round3(rel(start, e.IssuedUnix)), DoneSeconds: round3(rel(start, e.DoneUnix))})
	}
	for _, name := range []string{StreamMain, StreamLimited, StreamBudget} {
		if st := in.Streams[name]; st != nil {
			s.Streams = append(s.Streams, streamInfo(name, st, start))
		}
	}

	ph := func(t float64) int { return phaseOf(s.Phases, t) }
	buckets := mainStream(s, main, ph, end, rule)
	reference(s, rule)
	for _, r := range main.Records {
		if !r.Warmup && r.Outcome == bench.OutcomeOK && r.LatencyMS != nil && *r.LatencyMS > s.Reference.P99LimitMS {
			s.Phases[ph(Observed(r))].Main.Slow++
		}
	}
	for i := range buckets {
		b := &buckets[i]
		b.Good = b.Requests > 0 && b.OK > 0 && b.ErrorRate <= s.Reference.ErrorLimit && b.P99MS <= s.Reference.P99LimitMS
	}
	s.Buckets = buckets
	recovery(s, main, end, rule)

	if st := in.Streams[StreamLimited]; st != nil {
		if err := limited(s, st, in.Ledger, start, end, ph); err != nil {
			return nil, err
		}
	}
	if st := in.Streams[StreamBudget]; st != nil {
		if err := budgeted(s, st, in.Ledger, start, ph); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// phases splits [0, end) at each event. Each phase records how many
// replicas and whether Redis were up, from the kills and starts before it.
func phases(m Meta, events []Event, start time.Time, end float64) []PhaseResult {
	replicas := max(m.Replicas, 1)
	down := map[string]bool{}
	cur := PhaseResult{Name: PhaseSteady, FromSeconds: 0, ReplicasUp: replicas, RedisUp: true}
	var out []PhaseResult
	for _, e := range events {
		at := round3(rel(start, e.IssuedUnix))
		cur.ToSeconds = at
		out = append(out, cur)
		down[e.Target] = e.Action == ActionKill
		cur = PhaseResult{Name: e.Phase, FromSeconds: at, ReplicasUp: replicas, RedisUp: !down[RedisTarget]}
		for target, d := range down {
			if d && target != RedisTarget {
				cur.ReplicasUp--
			}
		}
	}
	cur.ToSeconds = end
	return append(out, cur)
}

// phaseOf is the index of the phase t (seconds from the start) falls in.
// Phases are half open, [from, to); anything before the first phase is in
// it and anything after the last is in the last.
func phaseOf(ps []PhaseResult, t float64) int {
	for i := len(ps) - 1; i > 0; i-- {
		if t >= ps[i].FromSeconds {
			return i
		}
	}
	return 0
}

type mainAcc struct {
	latency, ttft []time.Duration
}

// mainStream fills each phase's MainPhase and returns the per-bucket
// tallies, without the Good flag.
func mainStream(s *Summary, st *Stream, ph func(float64) int, end float64, rule Rule) []BucketResult {
	n := int(math.Ceil(end/rule.BucketSeconds - 1e-9))
	buckets := make([]BucketResult, n)
	bucketLat := make([][]time.Duration, n)
	for k := range buckets {
		buckets[k].StartSeconds = round3(float64(k) * rule.BucketSeconds)
	}
	accs := make([]mainAcc, len(s.Phases))
	for i := range s.Phases {
		s.Phases[i].Main.ErrorsByKind = map[string]int{}
		s.Phases[i].Main.CacheStatuses = map[string]int{}
	}
	for _, r := range st.Records {
		if r.Warmup {
			continue
		}
		t := Observed(r)
		i := ph(t)
		m := &s.Phases[i].Main
		m.Requests++
		k := int(math.Floor(t / rule.BucketSeconds))
		inBucket := t >= 0 && k < n
		if inBucket {
			buckets[k].Requests++
		}
		if r.Outcome != bench.OutcomeOK {
			m.Errors++
			m.ErrorsByKind[r.Outcome]++
			if inBucket {
				buckets[k].Errors++
			}
			continue
		}
		m.OK++
		status := r.Cache
		if status == "" {
			status = "none"
		}
		m.CacheStatuses[status]++
		hit := isHit(r.Cache)
		if hit {
			m.CacheHits++
		}
		if r.Pool != nil {
			m.RepeatOK++
			if hit {
				m.RepeatHits++
			}
		}
		if r.LatencyMS != nil {
			d := msDuration(*r.LatencyMS)
			accs[i].latency = append(accs[i].latency, d)
			if inBucket {
				bucketLat[k] = append(bucketLat[k], d)
			}
		}
		if r.TTFTMS != nil {
			accs[i].ttft = append(accs[i].ttft, msDuration(*r.TTFTMS))
		}
		if inBucket {
			buckets[k].OK++
			if hit {
				buckets[k].CacheHits++
			}
		}
	}
	for i := range s.Phases {
		m := &s.Phases[i].Main
		m.ErrorRate = ratio(m.Errors, m.Requests)
		m.CacheHitRate = ratio(m.CacheHits, m.OK)
		m.RepeatHitRate = ratio(m.RepeatHits, m.RepeatOK)
		m.Latency = bench.Summarise(accs[i].latency)
		m.TTFT = bench.Summarise(accs[i].ttft)
	}
	for k := range buckets {
		b := &buckets[k]
		b.ErrorRate = ratio(b.Errors, b.Requests)
		b.P99MS = bench.Summarise(bucketLat[k]).P99
	}
	return buckets
}

// Observed is when r's outcome was known, in seconds from its stream's
// start: its scheduled time plus its latency.
func Observed(r bench.Record) float64 {
	if r.LatencyMS == nil {
		return r.At
	}
	return r.At + *r.LatencyMS/1000
}

// isHit reports whether a cache status header value is a hit.
func isHit(status string) bool { return status == "hit-exact" || status == "hit-semantic" }

// reference sets the rule's baseline from the steady phase.
func reference(s *Summary, rule Rule) {
	steady := s.Phases[0].Main
	s.Reference = Reference{
		ErrorRate:  steady.ErrorRate,
		P99MS:      steady.Latency.P99,
		ErrorLimit: round6(max(rule.MinErrorRate, rule.ErrorFactor*steady.ErrorRate)),
		P99LimitMS: round3(rule.P99Factor * steady.Latency.P99),
	}
}

// recovery applies the rule after each event.
func recovery(s *Summary, main *Stream, end float64, rule Rule) {
	bs := rule.BucketSeconds
	for i := range s.Events {
		e := &s.Events[i]
		next := end
		if i+1 < len(s.Events) {
			next = min(s.Events[i+1].AtSeconds, end)
		}
		e.WindowSeconds = round3(max(next-e.AtSeconds, 0))
		until := next
		for k := int(math.Ceil(e.AtSeconds/bs - 1e-9)); float64(k+rule.Consecutive)*bs <= next+1e-9; k++ {
			if k < 0 || k+rule.Consecutive > len(s.Buckets) {
				break
			}
			ok := true
			for j := k; j < k+rule.Consecutive; j++ {
				if !s.Buckets[j].Good {
					ok = false
					break
				}
			}
			if ok {
				e.Recovered = true
				v := round3(float64(k)*bs - e.AtSeconds)
				e.RecoverySeconds = &v
				until = float64(k) * bs
				break
			}
		}
		for _, r := range main.Records {
			if t := Observed(r); !r.Warmup && t >= e.AtSeconds && t < until && r.Outcome != bench.OutcomeOK {
				e.ErrorsBeforeRecovery++
			}
		}
		// The worst bucket overlapping the event's window, including the
		// one the event falls in.
		for k := int(math.Floor(e.AtSeconds / bs)); k >= 0 && k < len(s.Buckets) && float64(k)*bs < next; k++ {
			b := s.Buckets[k]
			if e.WorstBucket == nil || b.Errors > e.WorstBucket.Errors ||
				(b.Errors == e.WorstBucket.Errors && b.P99MS > e.WorstBucket.P99MS) {
				e.WorstBucket = &b
			}
		}
	}
}

// limited fills each phase's LimitedPhase from the limited stream and the
// ledger.
func limited(s *Summary, st *Stream, ledger []fake.Entry, start time.Time, end float64, ph func(float64) int) error {
	m := s.Meta
	if m.Limit.Rate <= 0 || m.Limit.PeriodSeconds <= 0 || m.Limit.Model == "" {
		return fmt.Errorf("chaos: meta.json has no usable limit: %+v", m.Limit)
	}
	off := st.Result.StartedAt.Sub(start).Seconds()
	for _, r := range st.Records {
		if r.Warmup {
			continue
		}
		p := &s.Phases[ph(off+Observed(r))].Limited
		p.Offered++
		switch r.Outcome {
		case bench.OutcomeOK:
		case "http_429":
			p.Refused++
		default:
			p.OtherErrors++
		}
	}
	var times []time.Time
	last := start.Add(time.Duration(end * float64(time.Second)))
	for _, e := range ledger {
		if e.Model != m.Limit.Model {
			continue
		}
		times = append(times, e.Time)
		s.Phases[ph(e.Time.Sub(start).Seconds())].Limited.Admitted++
		if !e.Time.Before(last) {
			last = e.Time.Add(time.Microsecond)
		}
	}
	period := time.Duration(m.Limit.PeriodSeconds * float64(time.Second))
	local := m.Limit.Rate / int64(max(m.Replicas, 1))
	for i := range s.Phases {
		p := &s.Phases[i]
		p.Limited.LocalBound = m.Limit.Rate
		if !p.RedisUp {
			p.Limited.LocalBound = int64(p.ReplicasUp) * max(local, 1)
		}
	}
	for _, w := range limitseval.PerLimiterWindow(times, period, start, last) {
		p := &s.Phases[ph(w.StartSeconds+m.Limit.PeriodSeconds/2)].Limited
		p.Windows++
		p.MostInWindow = max(p.MostInWindow, w.Admitted)
		p.OverAdmissions += max(int64(w.Admitted)-m.Limit.Rate, 0)
	}
	return nil
}

// budgeted fills each phase's BudgetPhase from the budget stream and the
// ledger, pricing the ledger's usage with the meta's prices.
func budgeted(s *Summary, st *Stream, ledger []fake.Entry, start time.Time, ph func(float64) int) error {
	b := s.Meta.Budget
	limit, err := cost.ParseUSD(b.USD)
	if err != nil {
		return fmt.Errorf("chaos: budget: %w", err)
	}
	grace, err := cost.ParseUSD(b.GraceUSD)
	if err != nil {
		return fmt.Errorf("chaos: grace: %w", err)
	}
	in, err := cost.ParsePerMTok(b.InputPrice)
	if err != nil {
		return fmt.Errorf("chaos: input price: %w", err)
	}
	out, err := cost.ParsePerMTok(b.OutputPrice)
	if err != nil {
		return fmt.Errorf("chaos: output price: %w", err)
	}
	if b.Model == "" {
		return errors.New("chaos: meta.json names no budget model")
	}
	off := st.Result.StartedAt.Sub(start).Seconds()
	for _, r := range st.Records {
		if r.Warmup {
			continue
		}
		p := &s.Phases[ph(off+Observed(r))].Budget
		p.Offered++
		switch r.Outcome {
		case bench.OutcomeOK:
			p.OK++
		case "http_429", "http_402":
			p.Refused++
		default:
			p.OtherErrors++
		}
	}
	for _, e := range ledger {
		if e.Model != b.Model {
			continue
		}
		p := &s.Phases[ph(e.Time.Sub(start).Seconds())].Budget
		p.Admitted++
		p.Spent += cost.Nanos(int64(e.InputTokens)*int64(in) + int64(e.OutputTokens)*int64(out))
	}
	var total cost.Nanos
	for i := range s.Phases {
		p := &s.Phases[i]
		total += p.Budget.Spent
		p.Budget.Cumulative = total
		p.Budget.Overshoot = max(total-limit, 0)
		if !p.RedisUp {
			p.Budget.GraceBound = cost.Nanos(p.ReplicasUp) * grace
		}
	}
	return nil
}

func streamInfo(name string, st *Stream, start time.Time) StreamInfo {
	c := st.Result.Config
	return StreamInfo{
		Name: name, Model: c.Model, Rate: c.Rate, Stream: c.Stream, Repeat: c.Repeat, Pool: c.RepeatPool,
		Requests:           st.Result.Requests,
		StartOffsetSeconds: round3(st.Result.StartedAt.Sub(start).Seconds()),
		MaxSendDelayMS:     st.Result.SendDelay.Max,
	}
}

func rel(start time.Time, unix float64) float64 {
	return unix - float64(start.UnixNano())/1e9
}

func msDuration(ms float64) time.Duration {
	return time.Duration(math.Round(ms * float64(time.Millisecond)))
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return round6(float64(a) / float64(b))
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func round3(f float64) float64 { return math.Round(f*1e3) / 1e3 }
func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

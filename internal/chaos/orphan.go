package chaos

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// ScheduleOrphan is M12's run (`make chaos-orphan RUN=before|after`): a
// replica killed with budgeted streams in flight while Redis is up, and
// the provider's ledger reconciled against spent past the hold TTL.
const ScheduleOrphan = "orphan"

// OrphanMeta describes an orphan run; chaos/run.sh writes it.
type OrphanMeta struct {
	// Commit is the working tree the harness, the fake provider and the
	// analysis ran from; GatewayCommit is the replicas' build (the
	// commit before the fix in the before arm).
	Commit             string `json:"commit"`
	GatewayCommit      string `json:"gateway_commit"`
	FakeProviderCommit string `json:"fake_provider_commit"`
	// Run is "before" or "after".
	Run      string `json:"run"`
	Date     string `json:"date"`
	Command  string `json:"command"`
	Schedule string `json:"schedule"`
	Replicas int    `json:"replicas"`
	// ReplicaIPs maps each replica's compose service to its address on
	// the compose network, which the fake provider's ledger records as
	// each request's caller.
	ReplicaIPs map[string]string `json:"replica_ips"`
	// StartUnix is the load's start, the origin of every offset.
	StartUnix      float64 `json:"start_unix"`
	LoadSeconds    float64 `json:"load_seconds"`
	ObserveSeconds float64 `json:"observe_seconds"`
	Kill           struct {
		Target         string  `json:"target"`
		PlannedSeconds float64 `json:"planned_seconds"`
		Restarted      bool    `json:"restarted"`
	} `json:"kill"`
	// BalanceRead says how the snapshots' ledger balance was read.
	BalanceRead string `json:"balance_read"`
	Budget      struct {
		Model           string  `json:"model"`
		USD             string  `json:"usd"`
		InputPrice      string  `json:"input_price"`
		OutputPrice     string  `json:"output_price"`
		Rate            float64 `json:"rate"`
		Stream          bool    `json:"stream"`
		MaxTokens       int     `json:"max_tokens"`
		ReservationTTL  string  `json:"reservation_ttl"`
		ProviderTimeout string  `json:"provider_timeout"`
	} `json:"budget"`
	FakeProvider struct {
		Latency             string `json:"latency"`
		Tokens              int    `json:"tokens"`
		TokenInterval       string `json:"token_interval"`
		BudgetTokenInterval string `json:"budget_token_interval"`
	} `json:"fake_provider"`
}

// OrphanSnapshot is one read of the budgeted key's balance: first a raw
// HMGET of the balance hash, then the same balance through the ledger
// (a replica's admin API, which runs the balance script and so purges
// expired holds before it reads). LedgerBalance is the admin API's budget
// object, nil when that read failed.
type OrphanSnapshot struct {
	Label         string         `json:"label"`
	Unix          float64        `json:"unix"`
	RawSpent      cost.Nanos     `json:"spent_nanodollars"`
	RawReserved   cost.Nanos     `json:"reserved_nanodollars"`
	LedgerUnix    float64        `json:"ledger_unix"`
	LedgerBalance *LedgerBalance `json:"ledger_balance"`
}

// LedgerBalance is the admin API's budget object, in dollars.
type LedgerBalance struct {
	SpentUSD    float64 `json:"spent_usd"`
	ReservedUSD float64 `json:"reserved_usd"`
	Error       string  `json:"error,omitempty"`
}

// OrphanInput is an orphan run's raw files.
type OrphanInput struct {
	Meta      OrphanMeta
	Events    []Event
	Ledger    []fake.Entry
	Snapshots []OrphanSnapshot
	// Budget is the budget stream's request log; nil if absent.
	Budget []bench.Record
}

// OrphanSummary is summary.json of an orphan run. Amounts are
// nanodollars; offsets are seconds from the load's start.
type OrphanSummary struct {
	Meta OrphanMeta `json:"meta"`
	// KillSeconds is when the kill was issued; DiedSeconds when the
	// killed replica's connections to the provider closed (the first
	// end of one of its unfinished requests after the kill), or the
	// kill's issue time if it had none.
	KillSeconds float64 `json:"kill_seconds"`
	DiedSeconds float64 `json:"died_seconds"`
	// ExpiredBySeconds is DiedSeconds plus the hold TTL: every hold the
	// killed replica took has lapsed by then.
	ExpiredBySeconds float64 `json:"expired_by_seconds"`
	// Requests is the budget model's provider requests per replica, and
	// Unattributed those whose caller is no known replica.
	Requests     int            `json:"requests"`
	ByReplica    map[string]int `json:"by_replica"`
	Unattributed int            `json:"unattributed"`
	// OneRequest is the most the provider billed for one complete budget
	// request: the tolerance of the before hypothesis.
	OneRequest cost.Nanos `json:"one_request_nanodollars"`
	// Client counts the budget stream's requests by outcome.
	Client   map[string]int `json:"client_outcomes,omitempty"`
	InFlight OrphanInFlight `json:"in_flight"`
	// HeldCheck compares Redis's reserved, once the live replicas'
	// requests have settled and before any of the killed replica's holds
	// lapsed, with the holds recomputed from the ledger; nil if no
	// snapshot fell in that window.
	HeldCheck *HeldCheck    `json:"held_check,omitempty"`
	Timeline  []OrphanPoint `json:"timeline"`
	// AfterTTL is the last snapshot read through the ledger after
	// ExpiredBySeconds; nil if there is none.
	AfterTTL *OrphanPoint     `json:"after_ttl,omitempty"`
	Before   BeforeHypothesis `json:"before_hypothesis"`
	After    AfterHypothesis  `json:"after_hypothesis"`
}

// OrphanInFlight is the budget requests the provider had open from the
// killed replica when it died, counted from the ledger: the requests
// whose holds nobody settled.
type OrphanInFlight struct {
	Replica string `json:"replica"`
	IP      string `json:"ip"`
	Count   int    `json:"count"`
	// Completed of them the provider finished after the replica died
	// (none expected: the connection is gone).
	Completed    int `json:"completed"`
	OutputTokens int `json:"output_tokens"`
	// Billed prices what the provider reported for them; Held is the
	// gateway's holds for them, the input estimate plus max_tokens at the
	// model's price, recomputed from the ledger's record of each request.
	Billed          cost.Nanos `json:"billed_nanodollars"`
	Held            cost.Nanos `json:"held_nanodollars"`
	HeldMinusBilled cost.Nanos `json:"held_minus_billed_nanodollars"`
	// HeldOverBilled is Held / Billed.
	HeldOverBilled float64 `json:"held_over_billed"`
	// FirstSeconds and LastSeconds are when the first and last of them
	// reached the provider.
	FirstSeconds float64 `json:"first_seconds"`
	LastSeconds  float64 `json:"last_seconds"`
}

// HeldCheck sets Redis's reserved against the recomputed holds.
type HeldCheck struct {
	Label     string     `json:"label"`
	AtSeconds float64    `json:"at_seconds"`
	Reserved  cost.Nanos `json:"reserved_nanodollars"`
	Held      cost.Nanos `json:"held_nanodollars"`
	Equal     bool       `json:"equal"`
}

// OrphanPoint is one snapshot set against the provider's ledger.
type OrphanPoint struct {
	Label     string  `json:"label"`
	AtSeconds float64 `json:"at_seconds"`
	// Billed prices the budget model's requests that reached the
	// provider before the snapshot, at their final usage (so during the
	// load it runs ahead of spent by the replies still streaming).
	Billed cost.Nanos `json:"billed_nanodollars"`
	// RawSpent and RawReserved are the HMGET; Spent and Reserved the
	// read through the ledger right after it, absent if that failed.
	RawSpent    cost.Nanos  `json:"raw_spent_nanodollars"`
	RawReserved cost.Nanos  `json:"raw_reserved_nanodollars"`
	Spent       *cost.Nanos `json:"spent_nanodollars,omitempty"`
	Reserved    *cost.Nanos `json:"reserved_nanodollars,omitempty"`
	// Gap is Billed minus Spent (the ledger read, or the raw one when
	// that failed): positive is spend Redis never recorded.
	Gap cost.Nanos `json:"gap_nanodollars"`
	// PastTTL is true once every hold of the killed replica has lapsed.
	PastTTL bool `json:"past_ttl"`
}

// BeforeHypothesis is the before arm's hypothesis: after the TTL the
// gap is positive and equals what the provider billed for the requests
// in flight on the killed replica, within one request.
type BeforeHypothesis struct {
	Expected  bool       `json:"expected"`
	Holds     bool       `json:"holds"`
	Gap       cost.Nanos `json:"gap_nanodollars"`
	Billed    cost.Nanos `json:"in_flight_billed_nanodollars"`
	Diff      cost.Nanos `json:"gap_minus_billed_nanodollars"`
	Tolerance cost.Nanos `json:"tolerance_nanodollars"`
	Note      string     `json:"note"`
}

// AfterHypothesis is the after arm's hypothesis: after the TTL the gap
// is at most 0 and the over-count at most the held-minus-billed of the
// in-flight requests, reported as a multiple of what was billed for them.
type AfterHypothesis struct {
	Expected  bool       `json:"expected"`
	Holds     bool       `json:"holds"`
	Gap       cost.Nanos `json:"gap_nanodollars"`
	OverCount cost.Nanos `json:"over_count_nanodollars"`
	Bound     cost.Nanos `json:"bound_nanodollars"`
	// OverCountOverBilled is the over-count as a multiple of what the
	// provider billed for the in-flight requests; BoundOverBilled the
	// bound's.
	OverCountOverBilled float64 `json:"over_count_over_billed"`
	BoundOverBilled     float64 `json:"bound_over_billed"`
	Note                string  `json:"note"`
}

// LoadOrphan reads an orphan run's raw files from dir.
func LoadOrphan(dir string) (*OrphanInput, error) {
	in := &OrphanInput{}
	if err := readJSON(filepath.Join(dir, MetaFile), &in.Meta); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, EventsFile), &in.Events); err != nil {
		return nil, err
	}
	var ledger struct {
		Entries []fake.Entry `json:"entries"`
	}
	if err := readJSON(filepath.Join(dir, LedgerFile), &ledger); err != nil {
		return nil, err
	}
	in.Ledger = ledger.Entries
	if err := readJSON(filepath.Join(dir, SnapshotsFile), &in.Snapshots); err != nil {
		return nil, err
	}
	st, err := loadStream(dir, StreamBudget)
	switch {
	case err == nil:
		in.Budget = st.Records
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	return in, nil
}

// AnalyseOrphan reconciles an orphan run.
func AnalyseOrphan(in OrphanInput) (*OrphanSummary, error) {
	m := in.Meta
	if m.Schedule != ScheduleOrphan {
		return nil, fmt.Errorf("chaos: schedule %q is not %q", m.Schedule, ScheduleOrphan)
	}
	if m.Budget.Model == "" {
		return nil, errors.New("chaos: meta.json names no budget model")
	}
	pin, err := cost.ParsePerMTok(m.Budget.InputPrice)
	if err != nil {
		return nil, fmt.Errorf("chaos: input price: %w", err)
	}
	pout, err := cost.ParsePerMTok(m.Budget.OutputPrice)
	if err != nil {
		return nil, fmt.Errorf("chaos: output price: %w", err)
	}
	ttl, err := time.ParseDuration(m.Budget.ReservationTTL)
	if err != nil {
		return nil, fmt.Errorf("chaos: reservation ttl: %w", err)
	}
	target := m.Kill.Target
	ip, ok := m.ReplicaIPs[target]
	if !ok || ip == "" {
		return nil, fmt.Errorf("chaos: meta.json has no address for the killed replica %q", target)
	}
	var kill *Event
	for i, e := range in.Events {
		if e.Action == ActionKill && e.Target == target {
			kill = &in.Events[i]
			break
		}
	}
	if kill == nil {
		return nil, fmt.Errorf("chaos: no kill of %s in events.json", target)
	}
	at := func(t time.Time) float64 { return float64(t.UnixNano())/1e9 - m.StartUnix }
	billed := func(e fake.Entry) cost.Nanos {
		return cost.Nanos(int64(e.InputTokens)*int64(pin) + int64(e.OutputTokens)*int64(pout))
	}

	s := &OrphanSummary{Meta: m, KillSeconds: round3(kill.IssuedUnix - m.StartUnix), ByReplica: map[string]int{}}
	byIP := map[string]string{}
	for svc, a := range m.ReplicaIPs {
		byIP[a] = svc
	}
	var budget []fake.Entry
	for _, e := range in.Ledger {
		if e.Model != m.Budget.Model {
			continue
		}
		budget = append(budget, e)
		s.Requests++
		if svc, ok := byIP[e.Caller]; ok {
			s.ByReplica[svc]++
		} else {
			s.Unattributed++
		}
		if e.Completed {
			s.OneRequest = max(s.OneRequest, billed(e))
		}
	}

	// The replica died when its first unfinished request ended after
	// the kill was issued: SIGKILL closes its sockets and the provider
	// sees the connection go.
	died := s.KillSeconds
	first := math.Inf(1)
	for _, e := range budget {
		if e.Caller == ip && !e.Completed && !e.Ended.IsZero() && at(e.Ended) >= s.KillSeconds {
			first = math.Min(first, at(e.Ended))
		}
	}
	if !math.IsInf(first, 1) {
		died = first
	}
	s.DiedSeconds = round3(died)
	s.ExpiredBySeconds = round3(died + ttl.Seconds())

	f := &s.InFlight
	f.Replica, f.IP = target, ip
	f.FirstSeconds = math.Inf(1)
	minHeldAt := math.Inf(1) // earliest arrival of an in-flight request
	lastOther := 0.0         // the last end of a request on a live replica
	for _, e := range budget {
		if e.Caller != ip {
			if !e.Ended.IsZero() {
				lastOther = math.Max(lastOther, at(e.Ended))
			}
			continue
		}
		// Open at the provider when the replica died: arrived before,
		// and not ended before.
		if at(e.Time) > died || !e.Ended.IsZero() && at(e.Ended) < died {
			continue
		}
		if e.MaxTokens <= 0 {
			return nil, fmt.Errorf("chaos: a request in flight on %s has no max_tokens, so its hold is not known", target)
		}
		f.Count++
		if e.Completed {
			f.Completed++
		}
		f.OutputTokens += e.OutputTokens
		f.Billed += billed(e)
		f.Held += cost.Nanos(int64(e.EstimatedInputTokens)*int64(pin) + int64(e.MaxTokens)*int64(pout))
		f.FirstSeconds = math.Min(f.FirstSeconds, at(e.Time))
		f.LastSeconds = math.Max(f.LastSeconds, at(e.Time))
		minHeldAt = math.Min(minHeldAt, at(e.Time))
	}
	if f.Count == 0 {
		f.FirstSeconds = 0
	}
	f.FirstSeconds, f.LastSeconds = round3(f.FirstSeconds), round3(f.LastSeconds)
	f.HeldMinusBilled = f.Held - f.Billed
	f.HeldOverBilled = nanoRatio(f.Held, f.Billed)

	if len(in.Budget) > 0 {
		s.Client = map[string]int{}
		for _, r := range in.Budget {
			if !r.Warmup {
				s.Client[r.Outcome]++
			}
		}
	}

	snaps := append([]OrphanSnapshot(nil), in.Snapshots...)
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Unix < snaps[j].Unix })
	for _, sn := range snaps {
		p := OrphanPoint{
			Label: sn.Label, AtSeconds: round3(sn.Unix - m.StartUnix),
			RawSpent: sn.RawSpent, RawReserved: sn.RawReserved,
		}
		for _, e := range budget {
			if at(e.Time) < p.AtSeconds {
				p.Billed += billed(e)
			}
		}
		spent := sn.RawSpent
		if b := sn.LedgerBalance; b != nil && b.Error == "" {
			sp, rs := usdNanos(b.SpentUSD), usdNanos(b.ReservedUSD)
			p.Spent, p.Reserved = &sp, &rs
			spent = sp
		}
		p.Gap = p.Billed - spent
		p.PastTTL = p.AtSeconds > s.ExpiredBySeconds
		s.Timeline = append(s.Timeline, p)
		if p.PastTTL && p.Spent != nil {
			s.AfterTTL = &s.Timeline[len(s.Timeline)-1]
		}
		// Every live request has ended and no orphan hold can have
		// lapsed: reserved is the orphans' holds and nothing else.
		if s.HeldCheck == nil && f.Count > 0 && sn.Unix-m.StartUnix > lastOther && sn.Unix-m.StartUnix < minHeldAt+ttl.Seconds() {
			s.HeldCheck = &HeldCheck{Label: sn.Label, AtSeconds: p.AtSeconds, Reserved: sn.RawReserved, Held: f.Held, Equal: sn.RawReserved == f.Held}
		}
	}
	if s.AfterTTL != nil {
		// Copy, so the pointer does not alias the slice.
		p := *s.AfterTTL
		s.AfterTTL = &p
	}
	s.Before, s.After = hypotheses(s)
	return s, nil
}

func hypotheses(s *OrphanSummary) (BeforeHypothesis, AfterHypothesis) {
	f := s.InFlight
	b := BeforeHypothesis{Expected: s.Meta.Run == "before", Billed: f.Billed, Tolerance: s.OneRequest}
	a := AfterHypothesis{Expected: s.Meta.Run == "after", Bound: f.HeldMinusBilled, BoundOverBilled: nanoRatio(f.HeldMinusBilled, f.Billed)}
	p := s.AfterTTL
	if p == nil {
		b.Note = "no balance read through the ledger after every hold of the killed replica lapsed"
		a.Note = b.Note
		return b, a
	}
	b.Gap, b.Diff = p.Gap, p.Gap-f.Billed
	b.Holds = f.Count > 0 && p.Gap > 0 && abs(b.Diff) <= b.Tolerance
	b.Note = fmt.Sprintf("at %.1f s the gap (provider's bill minus spent) was %s; the %d requests in flight on %s were billed %s",
		p.AtSeconds, p.Gap, f.Count, f.Replica, f.Billed)
	a.Gap = p.Gap
	a.OverCount = max(-p.Gap, 0)
	a.OverCountOverBilled = nanoRatio(a.OverCount, f.Billed)
	a.Holds = f.Count > 0 && p.Gap <= 0 && a.OverCount <= a.Bound
	a.Note = fmt.Sprintf("at %.1f s spent was %s above the provider's bill, %.2f× what the %d requests in flight on %s were billed (%s); bound %s held minus billed",
		p.AtSeconds, a.OverCount, a.OverCountOverBilled, f.Count, f.Replica, f.Billed, a.Bound)
	return b, a
}

func nanoRatio(a, b cost.Nanos) float64 {
	if b == 0 {
		return 0
	}
	return round3(float64(a) / float64(b))
}

// usdNanos converts the admin API's dollars (float64 of nanodollars /
// 1e9) back to nanodollars.
func usdNanos(usd float64) cost.Nanos { return cost.Nanos(math.Round(usd * float64(cost.Dollar))) }

// DescribeOrphan is a short plain-text account of s for the terminal.
func DescribeOrphan(s *OrphanSummary) string {
	var b strings.Builder
	f := s.InFlight
	fmt.Fprintf(&b, "%s (%s): kill at %.2f s, %s died at %.2f s, holds lapsed by %.2f s\n",
		s.Meta.Run, s.Meta.GatewayCommit, s.KillSeconds, f.Replica, s.DiedSeconds, s.ExpiredBySeconds)
	fmt.Fprintf(&b, "%d budget requests at the provider %v, %d unattributed\n", s.Requests, s.ByReplica, s.Unattributed)
	fmt.Fprintf(&b, "in flight on %s: %d (%d output tokens), billed %s, held %s (%.2f×)\n",
		f.Replica, f.Count, f.OutputTokens, f.Billed, f.Held, f.HeldOverBilled)
	if h := s.HeldCheck; h != nil {
		fmt.Fprintf(&b, "reserved at %.1f s: %s against %s held (equal: %v)\n", h.AtSeconds, h.Reserved, h.Held, h.Equal)
	}
	fmt.Fprintf(&b, "%-8s %8s %14s %14s %14s %14s\n", "label", "at s", "billed", "spent", "reserved", "gap")
	for _, p := range s.Timeline {
		sp, rs := "-", "-"
		if p.Spent != nil {
			sp, rs = p.Spent.String(), p.Reserved.String()
		}
		fmt.Fprintf(&b, "%-8s %8.2f %14s %14s %14s %14s\n", p.Label, p.AtSeconds, p.Billed, sp, rs, p.Gap)
	}
	fmt.Fprintf(&b, "before hypothesis (expected %v): holds %v; %s\n", s.Before.Expected, s.Before.Holds, s.Before.Note)
	fmt.Fprintf(&b, "after hypothesis (expected %v): holds %v; %s\n", s.After.Expected, s.After.Holds, s.After.Note)
	return b.String()
}

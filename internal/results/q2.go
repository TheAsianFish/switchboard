package results

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/limitseval"
)

// Q2Dir is the subdirectory written by eval/limits/run.sh.
const Q2Dir = "q2-limits"

// Q2 is the shared limits and budget section.
type Q2 struct {
	Rows []Q2Row
	// Outages are the phase tables of the runs with a Redis outage.
	Outages []Q2Outage
	Budget  *Q2Budget
	// Limit describes the limit, e.g. "100 requests per 60 s, burst 20".
	Limit, Offered, Duration string
	Commits, Dates, Hardware []string
	Command                  string
	// Missing names scenarios of the full run with no result file.
	Missing []string
}

// Q2Row is one rate-limit scenario over the whole run.
type Q2Row struct {
	Scenario, Store, Algorithm string
	Offered, Admitted          int
	Refused                    int
	Bound                      int64
	Over                       int64
	// Worst is the window length (s) with the most over-admission, and
	// WorstAdmitted, WorstBound, WorstOver its numbers.
	Worst                     string
	WorstAdmitted             int
	WorstBound, WorstOver     int64
	SixtyAdmitted             int
	SixtyBound, SixtyOver     int64
	SixtyStart, WorstStart    string
	OtherErrors               int
	OfferedRate, DurationSecs string
}

// Q2Outage is the phase table of one outage run.
type Q2Outage struct {
	Scenario string
	Phases   []Q2Phase
}

// Q2Phase is one phase of an outage run.
type Q2Phase struct {
	Name, From, To        string
	Admitted              int
	Bound, Over           int64
	Worst                 string
	WorstAdmitted         int
	WorstBound, WorstOver int64
}

// Q2Budget is the shared budget run.
type Q2Budget struct {
	Limit, Ledger, Gateway, Reserved, Diff, Metrics string
	Offered, Admitted, Refused, Incomplete          int
	InputTokens, OutputTokens                       int64
	MatchesToCent, MetricsToCent, WithinBudget      string
	Scraped, Replicas                               int
	InputPrice, OutputPrice, OfferedRate            string
}

// scenarioOrder is the order of the full run, which is also the table's.
var scenarioOrder = []string{"fixed-window", "fixed-window-redis-outage", "gcra", "gcra-redis-outage", "no-shared-store"}

func loadQ2(dir string) (*Q2, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	if len(paths) == 0 {
		if _, err := os.Stat(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("results: %w", err)
		}
		return nil, nil
	}
	q := &Q2{}
	seen := map[string]bool{}
	commits, dates, hardware := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var limits []limitseval.LimitsResult
	for _, p := range paths {
		if filepath.Base(p) == "budget.json" {
			var b limitseval.BudgetResult
			if err := readJSON(p, &b); err != nil {
				return nil, err
			}
			q.Budget = budgetRow(b)
			commits[b.Commit], dates[b.Date[:10]], hardware[describeHardware(b.Hardware)] = true, true, true
			q.Command = b.Command
			continue
		}
		var r limitseval.LimitsResult
		if err := readJSON(p, &r); err != nil {
			return nil, err
		}
		if r.Scenario == "" || r.Admitted == 0 && r.Offered == 0 {
			return nil, fmt.Errorf("results: %s is not a limits result", p)
		}
		limits = append(limits, r)
		seen[r.Scenario] = true
		commits[r.Commit], dates[r.Date[:10]], hardware[describeHardware(r.Hardware)] = true, true, true
		q.Command = r.Command
	}
	slices.SortFunc(limits, func(a, b limitseval.LimitsResult) int { return order(a.Scenario) - order(b.Scenario) })
	for _, r := range limits {
		q.Rows = append(q.Rows, limitsRow(r))
		if len(r.Phases) > 0 {
			o := Q2Outage{Scenario: r.Scenario}
			for _, ph := range r.Phases {
				o.Phases = append(o.Phases, Q2Phase{
					Name: ph.Name, From: num(ph.FromSeconds), To: num(ph.ToSeconds),
					Admitted: ph.Admitted, Bound: ph.Bound, Over: ph.Over,
					Worst: num(ph.Worst.Seconds), WorstAdmitted: ph.Worst.Admitted, WorstBound: ph.Worst.Bound, WorstOver: ph.Worst.Over,
				})
			}
			q.Outages = append(q.Outages, o)
		}
	}
	if len(limits) > 0 {
		r := limits[0]
		q.Limit = fmt.Sprintf("%d requests per %s s, burst %d", r.Limit.Rate, num(r.Limit.PeriodSeconds), r.Limit.Burst)
		q.Offered = fmt.Sprintf("%s requests per second (%s per minute)", num(r.OfferedPerSecond), num(r.OfferedPerSecond*60))
		q.Duration = fmt.Sprintf("%.0f s", r.DurationSeconds)
	}
	for _, s := range scenarioOrder {
		if !seen[s] {
			q.Missing = append(q.Missing, s)
		}
	}
	q.Commits, q.Dates, q.Hardware = sortedKeys(commits), sortedKeys(dates), sortedKeys(hardware)
	return q, nil
}

func order(s string) int {
	if i := slices.Index(scenarioOrder, s); i >= 0 {
		return i
	}
	return len(scenarioOrder)
}

func limitsRow(r limitseval.LimitsResult) Q2Row {
	row := Q2Row{
		Scenario: r.Scenario, Store: r.Store, Algorithm: r.Algorithm,
		Offered: r.Offered, Admitted: r.Admitted,
		Bound: r.Whole.Bound, Over: r.Whole.Over,
		Worst: num(r.Whole.Worst.Seconds), WorstAdmitted: r.Whole.Worst.Admitted,
		WorstBound: r.Whole.Worst.Bound, WorstOver: r.Whole.Worst.Over,
		WorstStart:   num(r.Whole.Worst.StartSeconds),
		OfferedRate:  num(r.OfferedPerSecond),
		DurationSecs: fmt.Sprintf("%.0f", r.DurationSeconds),
	}
	if s := r.Whole.Sixty; s != nil {
		row.SixtyAdmitted, row.SixtyBound, row.SixtyOver, row.SixtyStart = s.Admitted, s.Bound, s.Over, num(s.StartSeconds)
	}
	for k, v := range r.Outcomes {
		switch k {
		case "ok":
		case "http_429":
			row.Refused += v
		default:
			row.OtherErrors += v
		}
	}
	return row
}

func budgetRow(b limitseval.BudgetResult) *Q2Budget {
	yes := func(v bool) string {
		if v {
			return "yes"
		}
		return "no"
	}
	q := &Q2Budget{
		Limit: dollars(b.LimitNanos), Ledger: b.LedgerNanos.String(), Gateway: b.GatewayNanos.String(),
		Reserved: b.ReservedNanos.String(), Diff: b.DiffNanos.String(), Metrics: fmt.Sprintf("$%.9f", b.MetricsDollars),
		Offered: b.Offered, Admitted: b.Admitted, Incomplete: b.Incomplete,
		InputTokens: b.InputTokens, OutputTokens: b.OutputTokens,
		MatchesToCent: yes(b.MatchesToCent), MetricsToCent: yes(b.MetricsToCent), WithinBudget: yes(b.WithinBudget),
		Scraped: b.ScrapedReplicas, Replicas: b.Replicas,
		InputPrice: b.Price.InputPerMTok, OutputPrice: b.Price.OutputPerMTok, OfferedRate: num(b.OfferedPerSecond),
	}
	for k, v := range b.Outcomes {
		if k != "ok" {
			q.Refused += v
		}
	}
	return q
}

func dollars(n cost.Nanos) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("$%.9f", n.USD()), "0"), ".")
}

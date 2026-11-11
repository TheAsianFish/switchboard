package results

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	// MostInWindow is the most admitted in one aligned limiter window,
	// Windows every window's count ("*" marks one the run covers in
	// part), Rate the limit's rate for comparison.
	MostInWindow string
	Windows      string
	Rate         int64
	// P50 and P99 are the latency of the requests that succeeded, in ms.
	P50, P99 string
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
	// MaxTokens is the max_tokens each request sent; OutPerRequest the
	// output tokens the provider returned per admitted request.
	MaxTokens     int
	OutPerRequest string
}

// Scenario returns the row of the named scenario, nil if it was not run.
func (q *Q2) Scenario(name string) *Q2Row {
	for i := range q.Rows {
		if q.Rows[i].Scenario == name {
			return &q.Rows[i]
		}
	}
	return nil
}

// Phase returns the named phase of an outage scenario, nil if there is
// none.
func (q *Q2) Phase(scenario, name string) *Q2Phase {
	for _, o := range q.Outages {
		if o.Scenario != scenario {
			continue
		}
		for i := range o.Phases {
			if o.Phases[i].Name == name {
				return &o.Phases[i]
			}
		}
	}
	return nil
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
		Rate:         r.Limit.Rate,
		// Result files from before these fields were recorded have none.
		MostInWindow: "—",
		P50:          "—",
		P99:          "—",
	}
	if len(r.LimiterWindows) > 0 {
		row.MostInWindow = strconv.Itoa(r.MostInLimiterWindow)
	}
	if r.Latency.Count > 0 {
		row.P50, row.P99 = fmt.Sprintf("%.1f", r.Latency.P50), fmt.Sprintf("%.1f", r.Latency.P99)
	}
	var ws []string
	for _, w := range r.LimiterWindows {
		v := strconv.Itoa(w.Admitted)
		if w.Partial {
			v += "*"
		}
		ws = append(ws, v)
	}
	row.Windows = strings.Join(ws, " · ")
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
		MaxTokens: b.MaxTokens,
	}
	if b.Admitted > 0 {
		q.OutPerRequest = num(float64(b.OutputTokens) / float64(b.Admitted))
	}
	for k, v := range b.Outcomes {
		if k != "ok" {
			q.Refused += v
		}
	}
	return q
}

// dollars formats an amount to the cent when it is whole cents, with all
// nine decimals otherwise.
func dollars(n cost.Nanos) string {
	if n%cost.Cent == 0 {
		return fmt.Sprintf("$%.2f", n.USD())
	}
	return n.String()
}

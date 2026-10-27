package limitseval

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

// Run identifies a run in its result file.
type Run struct {
	Scenario string `json:"scenario"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Command  string `json:"command"`
	// Replicas is the number of gateway replicas behind the load balancer.
	Replicas int            `json:"replicas"`
	Hardware bench.Hardware `json:"hardware"`
}

// Phase is a stretch of a run, for example the Redis outage.
type Phase struct {
	Name        string  `json:"name"`
	FromSeconds float64 `json:"from_seconds"`
	ToSeconds   float64 `json:"to_seconds"`
	Admitted    int     `json:"admitted"`
	Bound       int64   `json:"bound"`
	Over        int64   `json:"over"`
	Worst       Window  `json:"worst_window"`
	Sixty       *Window `json:"window_60s,omitempty"`
}

// LimitsResult is one rate-limit scenario.
type LimitsResult struct {
	Run
	Algorithm string `json:"algorithm"`
	// Store is "redis" when the replicas share limits through Redis and
	// "none" when each enforces the full limit alone.
	Store string `json:"store"`
	Limit struct {
		Rate          int64   `json:"rate"`
		PeriodSeconds float64 `json:"period_seconds"`
		Burst         int64   `json:"burst"`
	} `json:"limit"`
	OfferedPerSecond float64        `json:"offered_per_second"`
	DurationSeconds  float64        `json:"duration_seconds"`
	Offered          int            `json:"offered"`
	Outcomes         map[string]int `json:"outcomes"`
	// Admitted counts the requests in the provider's ledger: what got past
	// the limit, whatever the client saw.
	Admitted int `json:"admitted"`
	// Whole is the run as one phase; Phases splits it at the outage.
	Whole  Phase   `json:"whole_run"`
	Phases []Phase `json:"phases,omitempty"`
}

// BudgetResult is the shared-budget scenario.
type BudgetResult struct {
	Run
	Key              string         `json:"key"`
	LimitNanos       cost.Nanos     `json:"limit_nanodollars"`
	OfferedPerSecond float64        `json:"offered_per_second"`
	Offered          int            `json:"offered"`
	Outcomes         map[string]int `json:"outcomes"`
	Admitted         int            `json:"admitted"`
	Incomplete       int            `json:"incomplete"`
	Price            struct {
		InputPerMTok  string `json:"input_per_mtok"`
		OutputPerMTok string `json:"output_per_mtok"`
	} `json:"price"`
	InputTokens  int64 `json:"ledger_input_tokens"`
	OutputTokens int64 `json:"ledger_output_tokens"`
	// LedgerNanos is the provider's ledger priced independently of the
	// gateway's cost code; GatewayNanos is the spend in the gateway's
	// Redis ledger; MetricsDollars is the sum of the replicas'
	// switchboard_cost_dollars_total for the key.
	LedgerNanos    cost.Nanos `json:"ledger_nanodollars"`
	GatewayNanos   cost.Nanos `json:"gateway_spent_nanodollars"`
	ReservedNanos  cost.Nanos `json:"gateway_reserved_nanodollars"`
	MetricsDollars float64    `json:"metrics_dollars"`
	// DiffNanos is GatewayNanos - LedgerNanos.
	DiffNanos       cost.Nanos `json:"diff_nanodollars"`
	MatchesToCent   bool       `json:"matches_to_cent"`
	MetricsToCent   bool       `json:"metrics_match_to_cent"`
	WithinBudget    bool       `json:"within_budget"`
	ScrapedReplicas int        `json:"scraped_replicas"`
}

// Main implements `switchboard eval limits` and `switchboard eval budget`;
// args start with the subcommand.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "limits" && args[0] != "budget") {
		_, _ = fmt.Fprintln(stderr, "usage: switchboard eval limits|budget [flags]")
		return 2
	}
	var err error
	if args[0] == "limits" {
		err = limitsMain(ctx, args[1:], stdout, stderr)
	} else {
		err = budgetMain(ctx, args[1:], stdout, stderr)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		_, _ = fmt.Fprintf(stderr, "eval %s: %v\n", args[0], err)
		return 1
	}
}

var errUsage = errors.New("usage")

type common struct {
	ledger, model, benchFile, out string
	run                           Run
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.ledger, "ledger", "http://127.0.0.1:9101", "fake provider base URL")
	fs.StringVar(&c.model, "model", "", "upstream model name the scenario's key uses")
	fs.StringVar(&c.benchFile, "bench", "", "the run's `switchboard bench` result file")
	fs.StringVar(&c.out, "out", "", "write the result here (default stdout)")
	fs.StringVar(&c.run.Scenario, "scenario", "", "scenario name")
	fs.StringVar(&c.run.Commit, "commit", "", "commit under test")
	fs.StringVar(&c.run.Command, "command", "", "command line that produced the run")
	fs.IntVar(&c.run.Replicas, "replicas", 3, "gateway replicas")
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	return nil
}

func limitsMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eval limits", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.flags(fs)
	var res LimitsResult
	var l ratelimit.Limit
	var start, end, outFrom, outTo float64
	fs.StringVar(&res.Algorithm, "algorithm", ratelimit.AlgorithmFixedWindow, "limiter algorithm the replicas ran")
	fs.StringVar(&res.Store, "store", "redis", "redis, or none for replicas without a shared store")
	fs.Int64Var(&l.Rate, "rate", 100, "limit: requests per period")
	fs.DurationVar(&l.Period, "per", time.Minute, "limit: period")
	fs.Int64Var(&l.Burst, "burst", 20, "limit: burst")
	fs.Float64Var(&start, "start", 0, "run start, Unix seconds")
	fs.Float64Var(&end, "end", 0, "run end, Unix seconds")
	fs.Float64Var(&outFrom, "outage-from", 0, "when Redis was killed, Unix seconds (0: no outage)")
	fs.Float64Var(&outTo, "outage-to", 0, "when Redis was back, Unix seconds")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := l.Validate(); err != nil {
		return err
	}
	if start == 0 || end <= start || c.model == "" || c.benchFile == "" {
		_, _ = fmt.Fprintln(stderr, "eval limits: --model, --bench, --start and --end are required")
		return errUsage
	}
	b, err := readBench(c.benchFile)
	if err != nil {
		return err
	}
	entries, err := fetchLedger(ctx, c.ledger)
	if err != nil {
		return err
	}
	from, to := unix(start), unix(end)
	var times []time.Time
	for _, e := range entries {
		if e.Model == c.model {
			times = append(times, e.Time)
		}
	}
	res.Run = c.run
	res.Date = from.UTC().Format(time.RFC3339)
	res.Hardware = b.Hardware
	res.Limit.Rate, res.Limit.PeriodSeconds, res.Limit.Burst = l.Rate, l.Period.Seconds(), l.Burst
	res.OfferedPerSecond = b.Config.Rate
	res.DurationSeconds = to.Sub(from).Seconds()
	res.Offered = b.Requests + b.WarmupRequests
	res.Outcomes = b.Outcomes
	res.Admitted = len(times)
	res.Whole = phase("whole run", times, l, from, from, to)
	if outFrom > 0 {
		of, ot := unix(outFrom), unix(outTo)
		res.Phases = []Phase{
			phase("before the outage", times, l, from, from, of),
			phase("Redis down", times, l, from, of, ot),
			phase("after Redis is back", times, l, from, ot, to),
		}
	}
	return write(c.out, stdout, res)
}

// phase summarises the admissions between a and b; offsets are relative to
// the run start.
func phase(name string, times []time.Time, l ratelimit.Limit, start, a, b time.Time) Phase {
	// Phases are half open, [a, b), so a request on a boundary counts once.
	var in []time.Time
	for _, t := range times {
		if !t.Before(a) && t.Before(b) {
			in = append(in, t)
		}
	}
	p := Phase{
		Name:        name,
		FromSeconds: round(a.Sub(start).Seconds()),
		ToSeconds:   round(b.Sub(start).Seconds()),
		Admitted:    len(in),
		Bound:       l.Bound(b.Sub(a)),
		Worst:       Worst(in, l, time.Minute, a, b),
	}
	p.Over = max(int64(p.Admitted)-p.Bound, 0)
	p.Worst.StartSeconds = round(p.Worst.StartSeconds + a.Sub(start).Seconds())
	if b.Sub(a) >= time.Minute {
		w := At(sortedTimes(in), l, time.Minute, a, b)
		w.StartSeconds = round(w.StartSeconds + a.Sub(start).Seconds())
		p.Sixty = &w
	}
	return p
}

func budgetMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eval budget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.flags(fs)
	var res BudgetResult
	var redisURL, prefix, period, limit, metrics string
	fs.StringVar(&res.Key, "key", "budget", "key name")
	fs.StringVar(&redisURL, "redis", "redis://127.0.0.1:6379/0", "the gateway's Redis")
	fs.StringVar(&prefix, "prefix", "sb:", "the gateway's Redis key prefix")
	fs.StringVar(&period, "period", "total", "the key's budget period")
	fs.StringVar(&limit, "limit", "", "the key's budget in dollars")
	fs.StringVar(&res.Price.InputPerMTok, "input-price", "", "the model's input price, $/MTok")
	fs.StringVar(&res.Price.OutputPerMTok, "output-price", "", "the model's output price, $/MTok")
	fs.StringVar(&metrics, "metrics", "", "comma-separated /metrics URLs of the replicas")
	if err := parse(fs, args); err != nil {
		return err
	}
	lim, err := cost.ParseUSD(limit)
	if err != nil || c.model == "" || c.benchFile == "" {
		_, _ = fmt.Fprintln(stderr, "eval budget: --model, --bench and a valid --limit are required")
		return errUsage
	}
	inPrice, err1 := ratFromDecimal(res.Price.InputPerMTok)
	outPrice, err2 := ratFromDecimal(res.Price.OutputPerMTok)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("prices: %w", err)
	}
	b, err := readBench(c.benchFile)
	if err != nil {
		return err
	}
	entries, err := fetchLedger(ctx, c.ledger)
	if err != nil {
		return err
	}
	res.Run = c.run
	res.Date = b.StartedAt.UTC().Format(time.RFC3339)
	res.Hardware = b.Hardware
	res.LimitNanos = lim
	res.OfferedPerSecond = b.Config.Rate
	res.Offered = b.Requests + b.WarmupRequests
	res.Outcomes = b.Outcomes
	for _, e := range entries {
		if e.Model != c.model {
			continue
		}
		res.Admitted++
		if !e.Completed {
			res.Incomplete++
		}
		res.InputTokens += int64(e.InputTokens)
		res.OutputTokens += int64(e.OutputTokens)
	}
	// The ledger priced with exact rationals, not with the gateway's cost
	// package, so a bug there cannot hide in both numbers.
	total := new(big.Rat).Add(
		new(big.Rat).Mul(big.NewRat(res.InputTokens, 1_000_000), inPrice),
		new(big.Rat).Mul(big.NewRat(res.OutputTokens, 1_000_000), outPrice))
	nanos := new(big.Rat).Mul(total, big.NewRat(1_000_000_000, 1))
	if !nanos.IsInt() {
		return fmt.Errorf("ledger cost %s is not a whole number of nanodollars", nanos.FloatString(3))
	}
	res.LedgerNanos = cost.Nanos(nanos.Num().Int64())

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("redis url: %w", err)
	}
	rdb := redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()
	bal, err := budget.NewRedis(rdb, prefix, 0, time.Now).Balance(ctx, res.Key, budget.Budget{Limit: lim, Period: budget.Period(period)})
	if err != nil {
		return fmt.Errorf("read the gateway's ledger: %w", err)
	}
	res.GatewayNanos, res.ReservedNanos = bal.Spent, bal.Reserved
	res.DiffNanos = res.GatewayNanos - res.LedgerNanos
	res.MatchesToCent = cents(res.GatewayNanos) == cents(res.LedgerNanos)
	res.WithinBudget = res.GatewayNanos <= lim

	for _, u := range strings.Split(metrics, ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		v, err := scrapeDollars(ctx, u, res.Key)
		if err != nil {
			return err
		}
		res.MetricsDollars += v
		res.ScrapedReplicas++
	}
	res.MetricsDollars = math.Round(res.MetricsDollars*1e9) / 1e9
	res.MetricsToCent = math.Round(res.MetricsDollars*100) == float64(cents(res.LedgerNanos))
	return write(c.out, stdout, res)
}

// cents rounds an amount to whole cents, half away from zero.
func cents(n cost.Nanos) int64 {
	return int64(math.Round(float64(n) / float64(cost.Cent)))
}

func ratFromDecimal(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("%q is not a non-negative decimal", s)
	}
	return r, nil
}

func scrapeDollars(ctx context.Context, url, key string) (float64, error) {
	body, err := get(ctx, url)
	if err != nil {
		return 0, err
	}
	p := expfmt.NewTextParser(model.UTF8Validation)
	families, err := p.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", url, err)
	}
	total := 0.0
	if f, ok := families["switchboard_cost_dollars_total"]; ok {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "key" && l.GetValue() == key {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total, nil
}

func fetchLedger(ctx context.Context, base string) ([]fake.Entry, error) {
	body, err := get(ctx, strings.TrimRight(base, "/")+"/ledger")
	if err != nil {
		return nil, err
	}
	var l struct {
		Entries []fake.Entry `json:"entries"`
	}
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, fmt.Errorf("decode ledger: %w", err)
	}
	return l.Entries, nil
}

func get(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func readBench(path string) (*bench.Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r bench.Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &r, nil
}

func write(path string, stdout io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if path == "" {
		_, err = stdout.Write(b)
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func unix(s float64) time.Time {
	sec, frac := math.Modf(s)
	return time.Unix(int64(sec), int64(frac*1e9))
}

func round(f float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 3, 64), 64)
	return v
}

func sortedTimes(ts []time.Time) []time.Time {
	out := slices.Clone(ts)
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

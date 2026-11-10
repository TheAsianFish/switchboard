package results

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// Q4Dir is the subdirectory written by eval/hedge/run.sh.
const Q4Dir = "q4-hedge"

// Q4PriceModel prices the synthetic run's tokens, to put the extra
// requests in dollars: the cheapest Claude model, the one the live part of
// Q4 would use.
const Q4PriceModel = "claude-haiku-5-5"

// LedgerSummary is the fake provider's ledger for one arm, reduced to the
// counts Q4 needs: every request that reached the provider, whether it
// finished, and the tokens it reported.
type LedgerSummary struct {
	Requests     int `json:"requests"`
	Completed    int `json:"completed"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// SummarizeLedger reduces the fake provider's GET /ledger body.
func SummarizeLedger(r io.Reader) (LedgerSummary, error) {
	var body struct {
		Entries []struct {
			InputTokens  int  `json:"input_tokens"`
			OutputTokens int  `json:"output_tokens"`
			Completed    bool `json:"completed"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return LedgerSummary{}, fmt.Errorf("ledger: %w", err)
	}
	var s LedgerSummary
	for _, e := range body.Entries {
		s.Requests++
		if e.Completed {
			s.Completed++
		}
		s.InputTokens += e.InputTokens
		s.OutputTokens += e.OutputTokens
	}
	return s, nil
}

// LedgerMain is `switchboard eval hedge-ledger`: it reads the fake
// provider's ledger on stdin and writes its summary on stdout, so the raw
// result is a few numbers rather than one record per request.
func LedgerMain(stdin io.Reader, stdout, stderr io.Writer) int {
	s, err := SummarizeLedger(stdin)
	if err == nil {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(s)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "switchboard: %v\n", err)
		return 1
	}
	return 0
}

// Q4Meta describes the run; eval/hedge/run.sh writes it as meta.json.
type Q4Meta struct {
	Commit       string `json:"commit"`
	Date         string `json:"date"`
	FakeProvider struct {
		Latency       string  `json:"latency"`
		LatencySigma  float64 `json:"latency_sigma"`
		SlowFraction  float64 `json:"slow_fraction"`
		SlowFactor    float64 `json:"slow_factor"`
		Tokens        int     `json:"tokens"`
		TokenInterval string  `json:"token_interval"`
		Seed          uint64  `json:"seed"`
	} `json:"fake_provider"`
	Rate     float64 `json:"rate"`
	Duration string  `json:"duration"`
	Warmup   string  `json:"warmup"`
	Command  string  `json:"command"`
}

// Q4 is the hedging section.
type Q4 struct {
	Meta Q4Meta
	// SlowPercent and the other strings are formatted for prose.
	SlowPercent, Sigma string
	Rows               []Q4Row
	// TTFTCut and LatencyCut are hedging's change to the p99s, on minus
	// off, as "-445.1 ms (-69%)".
	TTFTCut, LatencyCut string
	ExtraRequests       string
	ExtraSpend          string
	PriceModel          string
	Hardware            string
	Resamples           int
}

// Q4Row is one arm. Times are milliseconds with one decimal.
type Q4Row struct {
	Arm                         string
	Requests, Errors            int
	TTFTP50, TTFTP95, TTFTP99   string
	TTFTP99CI                   string
	TTFTP999                    string
	LatencyP50, LatencyP99      string
	LatencyP99CI                string
	Upstream                    int
	ExtraPercent                string
	Cancelled                   int
	HedgesSent, HedgesWon       string
	HedgesRefused               string
	HedgeDelay                  string
	InputTokens, OutputTokens   int
	Spend                       string
	upstreamPerClient, spendUSD float64
	ttftP99, latencyP99         float64
	ttftP50                     float64
}

// q4Resamples is the bootstrap's resample count; the seed is fixed so
// RESULTS.md re-renders byte for byte.
const q4Resamples = 1000

func loadQ4(dir string) (*Q4, error) {
	var meta Q4Meta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	price, ok := cost.Defaults[Q4PriceModel]
	if !ok {
		return nil, fmt.Errorf("results: no list price for %s", Q4PriceModel)
	}
	q := &Q4{Meta: meta, PriceModel: Q4PriceModel, Resamples: q4Resamples,
		SlowPercent: strconv.FormatFloat(meta.FakeProvider.SlowFraction*100, 'f', -1, 64),
		Sigma:       strconv.FormatFloat(meta.FakeProvider.LatencySigma, 'f', -1, 64)}
	for _, arm := range []string{"off", "on"} {
		row, hw, err := loadQ4Arm(dir, arm, price)
		if err != nil {
			return nil, err
		}
		q.Rows = append(q.Rows, row)
		q.Hardware = hw
	}
	off, on := q.Rows[0], q.Rows[1]
	q.TTFTCut = change(off.ttftP99, on.ttftP99)
	q.LatencyCut = change(off.latencyP99, on.latencyP99)
	q.ExtraRequests = fmt.Sprintf("%+.1f%%", (on.upstreamPerClient-off.upstreamPerClient)/off.upstreamPerClient*100)
	q.ExtraSpend = fmt.Sprintf("%+.1f%%", (on.spendUSD-off.spendUSD)/off.spendUSD*100)
	return q, nil
}

func ratio(p99, p50 float64) string { return strconv.FormatFloat(p99/p50, 'f', 1, 64) }

func change(before, after float64) string {
	return fmt.Sprintf("%+.1f ms (%+.0f%%)", after-before, (after-before)/before*100)
}

func loadQ4Arm(dir, arm string, price cost.Price) (Q4Row, string, error) {
	var res bench.Result
	if err := readJSON(filepath.Join(dir, arm+".json"), &res); err != nil {
		return Q4Row{}, "", err
	}
	if res.Samples == nil || len(res.Samples.TTFTMS) == 0 {
		return Q4Row{}, "", fmt.Errorf("results: %s/%s.json has no samples", dir, arm)
	}
	var led LedgerSummary
	if err := readJSON(filepath.Join(dir, arm+"-ledger.json"), &led); err != nil {
		return Q4Row{}, "", err
	}
	hedges, err := readCounters(filepath.Join(dir, arm+"-metrics.prom"), "switchboard_provider_hedges_total", "event")
	if err != nil {
		return Q4Row{}, "", err
	}
	var health struct {
		Providers []struct {
			HedgeDelayMS map[string]float64 `json:"hedge_delay_ms"`
		} `json:"providers"`
	}
	if err := readJSON(filepath.Join(dir, arm+"-providers.json"), &health); err != nil {
		return Q4Row{}, "", err
	}
	client := res.Requests + res.WarmupRequests
	// The ledger sums a whole arm, and Of picks its rates from one
	// request's prompt size: an arm past a long-context threshold would be
	// priced as one long request. Each bench request is short, so the base
	// rates apply.
	spent := cost.Price{Base: price.Base}.Of(core.Usage{InputTokens: led.InputTokens, OutputTokens: led.OutputTokens})
	row := Q4Row{
		Arm:      arm,
		Requests: res.Requests, Errors: res.Errors(),
		TTFTP50: f1(res.TTFT.P50), TTFTP95: f1(res.TTFT.P95), TTFTP99: f1(res.TTFT.P99), TTFTP999: f1(res.TTFT.P999),
		TTFTP99CI:  bootstrapP99(res.Samples.TTFTMS),
		LatencyP50: f1(res.Latency.P50), LatencyP99: f1(res.Latency.P99),
		LatencyP99CI: bootstrapP99(res.Samples.LatencyMS),
		Upstream:     led.Requests,
		ExtraPercent: fmt.Sprintf("%.1f%%", float64(led.Requests-client)/float64(client)*100),
		Cancelled:    led.Requests - led.Completed,
		HedgesSent:   count(hedges["sent"]), HedgesWon: count(hedges["won"]), HedgesRefused: count(hedges["refused"]),
		HedgeDelay:  "n/a",
		InputTokens: led.InputTokens, OutputTokens: led.OutputTokens,
		Spend:             fmt.Sprintf("$%.4f", spent.USD()),
		upstreamPerClient: float64(led.Requests) / float64(client),
		spendUSD:          spent.USD(),
		ttftP99:           res.TTFT.P99, latencyP99: res.Latency.P99, ttftP50: res.TTFT.P50,
	}
	// The hedging-off arm keeps a latency window too, but no request used
	// it as a delay.
	for _, p := range health.Providers {
		for _, d := range p.HedgeDelayMS {
			if arm == "on" {
				row.HedgeDelay = f1(d)
			}
		}
	}
	return row, describeHardware(res.Hardware), nil
}

func f1(ms float64) string { return strconv.FormatFloat(ms, 'f', 1, 64) }

func count(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }

// bootstrapP99 is a percentile-bootstrap 95% interval for the p99 of
// samples (milliseconds), from q4Resamples resamples with a fixed seed.
func bootstrapP99(samples []float64) string {
	n := len(samples)
	rnd := rand.New(rand.NewPCG(4, 4))
	ests := make([]float64, q4Resamples)
	buf := make([]time.Duration, n)
	for i := range ests {
		for j := range buf {
			buf[j] = time.Duration(samples[rnd.IntN(n)] * float64(time.Millisecond))
		}
		slices.Sort(buf)
		ests[i] = float64(bench.Percentile(buf, 99)) / float64(time.Millisecond)
	}
	slices.Sort(ests)
	lo := ests[int(0.025*float64(q4Resamples))]
	hi := ests[int(0.975*float64(q4Resamples))-1]
	return fmt.Sprintf("%s–%s", f1(lo), f1(hi))
}

// readCounters sums a Prometheus text file's series of one counter by the
// value of one label. A missing file or series reads as zero.
func readCounters(path, name, label string) (map[string]float64, error) {
	out := map[string]float64{}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		end := strings.LastIndexByte(line, '}')
		if end < 0 {
			return nil, fmt.Errorf("results: %s: bad line %q", path, line)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[end+1:]), 64)
		if err != nil {
			return nil, fmt.Errorf("results: %s: bad value in %q", path, line)
		}
		for _, kv := range strings.Split(line[len(name)+1:end], ",") {
			k, val, _ := strings.Cut(kv, "=")
			if k == label {
				out[strings.Trim(val, `"`)] += v
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("results: %s: %w", path, err)
	}
	return out, nil
}

// Q4OllamaDir is the subdirectory written by eval/hedge/ollama.sh.
const Q4OllamaDir = "q4-hedge-ollama"

// Q4Ollama is the local check: the same comparison against a real model
// on local Ollama.
type Q4Ollama struct {
	Meta struct {
		Commit        string  `json:"commit"`
		Date          string  `json:"date"`
		Model         string  `json:"model"`
		OllamaVersion string  `json:"ollama_version"`
		Rate          float64 `json:"rate"`
		Duration      string  `json:"duration"`
		Warmup        string  `json:"warmup"`
		MaxTokens     int     `json:"max_tokens"`
		Command       string  `json:"command"`
	}
	Rows     []Q4Row
	Hardware string
	// TTFTCut and LatencyCut are on minus off at the p99.
	TTFTCut, LatencyCut string
	// TailRatio is the hedging-off arm's TTFT p99 over its p50, and
	// SyntheticTailRatio the same for the synthetic run when there is one.
	TailRatio, SyntheticTailRatio string
}

func loadQ4Ollama(dir string) (*Q4Ollama, error) {
	q := &Q4Ollama{}
	if err := readJSON(filepath.Join(dir, "meta.json"), &q.Meta); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var p99s [][2]float64
	var p50s []float64
	for _, arm := range []string{"off", "on"} {
		var res bench.Result
		if err := readJSON(filepath.Join(dir, arm+".json"), &res); err != nil {
			return nil, err
		}
		if res.Samples == nil || len(res.Samples.TTFTMS) == 0 {
			return nil, fmt.Errorf("results: %s/%s.json has no samples", dir, arm)
		}
		hedges, err := readCounters(filepath.Join(dir, arm+"-metrics.prom"), "switchboard_provider_hedges_total", "event")
		if err != nil {
			return nil, err
		}
		client := res.Requests + res.WarmupRequests
		q.Rows = append(q.Rows, Q4Row{
			Arm: arm, Requests: res.Requests, Errors: res.Errors(),
			TTFTP50: f1(res.TTFT.P50), TTFTP95: f1(res.TTFT.P95), TTFTP99: f1(res.TTFT.P99),
			TTFTP99CI:  bootstrapP99(res.Samples.TTFTMS),
			LatencyP50: f1(res.Latency.P50), LatencyP99: f1(res.Latency.P99),
			LatencyP99CI: bootstrapP99(res.Samples.LatencyMS),
			ExtraPercent: fmt.Sprintf("%.1f%%", hedges["sent"]/float64(client)*100),
			HedgesSent:   count(hedges["sent"]), HedgesWon: count(hedges["won"]), HedgesRefused: count(hedges["refused"]),
		})
		p99s = append(p99s, [2]float64{res.TTFT.P99, res.Latency.P99})
		p50s = append(p50s, res.TTFT.P50)
		q.Hardware = describeHardware(res.Hardware)
	}
	q.TTFTCut = change(p99s[0][0], p99s[1][0])
	q.LatencyCut = change(p99s[0][1], p99s[1][1])
	q.TailRatio = ratio(p99s[0][0], p50s[0])
	return q, nil
}

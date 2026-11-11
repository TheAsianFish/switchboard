package results

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"strconv"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// Q4OllamaDir is the subdirectory written by eval/hedge/ollama.sh.
const Q4OllamaDir = "q4-hedge-ollama"

// Q4ClaudeDir is the subdirectory written by eval/hedge/claude.sh.
const Q4ClaudeDir = "q4-hedge-claude"

// Q4LiveMeta describes a run of eval/hedge/live.sh (through ollama.sh or
// claude.sh); it is that run's meta.json.
type Q4LiveMeta struct {
	Commit string `json:"commit"`
	Date   string `json:"date"`
	// Provider is "anthropic" for claude.sh; ollama.sh does not write it.
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// OllamaVersion is only written by ollama.sh.
	OllamaVersion string  `json:"ollama_version"`
	Rate          float64 `json:"rate"`
	Duration      string  `json:"duration"`
	Warmup        string  `json:"warmup"`
	MaxTokens     int     `json:"max_tokens"`
	Command       string  `json:"command"`
}

// Q4Live is the same comparison as Q4 against a real backend: a model on
// local Ollama (Q4Ollama) or Claude (Q4Claude).
type Q4Live struct {
	Meta     Q4LiveMeta
	Rows     []Q4Row
	Hardware string
	// TTFTCut and LatencyCut are on minus off at the p99.
	TTFTCut, LatencyCut string
	// TailRatio is the hedging-off arm's TTFT p99 over its p50, and
	// SyntheticTailRatio the same for the synthetic run when there is one.
	TailRatio, SyntheticTailRatio string
	// Spend is what the run cost, when its metrics recorded tokens and the
	// model has a list price; nil otherwise (the local run).
	Spend *Q4LiveSpend

	// clients and hedgeShares are per arm: client requests (warm-up
	// included) and hedges sent per client request.
	clients     []int
	hedgeShares []float64
}

// Q4LiveSpend is a live run's cost at list prices. The gateway counts the
// tokens of the reply each client request got; a hedge's losing attempt
// is cancelled upstream and never reports usage, so its cost is bounded
// from the mean prompt and max_tokens.
type Q4LiveSpend struct {
	PriceModel                string
	InputTokens, OutputTokens int
	// Counted is the counted tokens' price, Losers the number of cancelled
	// attempts (hedges sent, both arms) and LoserBound their bound.
	Counted, LoserBound, Total string
	Losers                     int
}

func loadQ4Live(dir string) (*Q4Live, error) {
	q := &Q4Live{}
	if err := readJSON(filepath.Join(dir, "meta.json"), &q.Meta); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var p99s [][2]float64
	var p50s []float64
	var tokens []map[string]float64
	var losers float64
	for _, arm := range []string{"off", "on"} {
		var res bench.Result
		if err := readJSON(filepath.Join(dir, arm+".json"), &res); err != nil {
			return nil, err
		}
		if res.Samples == nil || len(res.Samples.TTFTMS) == 0 {
			return nil, fmt.Errorf("results: %s/%s.json has no samples", dir, arm)
		}
		metrics := filepath.Join(dir, arm+"-metrics.prom")
		hedges, err := readCounters(metrics, "switchboard_provider_hedges_total", "event")
		if err != nil {
			return nil, err
		}
		tok, err := readCounters(metrics, "switchboard_tokens_total", "type")
		if err != nil {
			return nil, err
		}
		client := res.Requests + res.WarmupRequests
		share := hedges["sent"] / float64(client)
		q.Rows = append(q.Rows, Q4Row{
			Arm: arm, Requests: res.Requests, Errors: res.Errors(),
			TTFTP50: f1(res.TTFT.P50), TTFTP95: f1(res.TTFT.P95), TTFTP99: f1(res.TTFT.P99),
			TTFTP99CI:  bootstrapP99(res.Samples.TTFTMS),
			LatencyP50: f1(res.Latency.P50), LatencyP99: f1(res.Latency.P99),
			LatencyP99CI: bootstrapP99(res.Samples.LatencyMS),
			ExtraPercent: fmt.Sprintf("%.1f%%", share*100),
			HedgesSent:   count(hedges["sent"]), HedgesWon: count(hedges["won"]), HedgesRefused: count(hedges["refused"]),
			InputTokens: int(tok["input"]), OutputTokens: int(tok["output"]),
		})
		q.clients = append(q.clients, client)
		q.hedgeShares = append(q.hedgeShares, share)
		tokens = append(tokens, tok)
		losers += hedges["sent"]
		p99s = append(p99s, [2]float64{res.TTFT.P99, res.Latency.P99})
		p50s = append(p50s, res.TTFT.P50)
		q.Hardware = describeHardware(res.Hardware)
	}
	q.TTFTCut = change(p99s[0][0], p99s[1][0])
	q.LatencyCut = change(p99s[0][1], p99s[1][1])
	q.TailRatio = ratio(p99s[0][0], p50s[0])
	if price, ok := cost.Defaults[q.Meta.Model]; ok && len(tokens[0])+len(tokens[1]) > 0 {
		q.Spend = liveSpend(q, price, tokens, int(losers))
	}
	return q, nil
}

// liveSpend prices a live run's counted tokens and bounds its cancelled
// attempts. Each bench request is short, so the base rates apply (see
// loadQ4Arm).
func liveSpend(q *Q4Live, price cost.Price, tokens []map[string]float64, losers int) *Q4LiveSpend {
	var u core.Usage
	for i := range q.Rows {
		u.InputTokens += int(tokens[i]["input"])
		u.OutputTokens += int(tokens[i]["output"])
		u.CacheReadTokens += int(tokens[i]["cache_read"])
		u.CacheWriteTokens += int(tokens[i]["cache_write"])
		q.Rows[i].Spend = usd6(cost.Price{Base: price.Base}.Of(core.Usage{
			InputTokens: int(tokens[i]["input"]), OutputTokens: int(tokens[i]["output"]),
			CacheReadTokens: int(tokens[i]["cache_read"]), CacheWriteTokens: int(tokens[i]["cache_write"]),
		}).USD())
	}
	counted := cost.Price{Base: price.Base}.Of(u)
	meanInput := float64(u.InputTokens) / float64(q.clients[0]+q.clients[1])
	bound := float64(losers) * (meanInput*float64(price.Base.Input) + float64(q.Meta.MaxTokens)*float64(price.Base.Output)) / float64(cost.Dollar)
	return &Q4LiveSpend{
		PriceModel:  q.Meta.Model,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		Counted:    usd6(counted.USD()),
		Losers:     losers,
		LoserBound: usd6(bound),
		Total:      usd6(counted.USD() + bound),
	}
}

// Q4ClaudeEstimate is the cost of eval/hedge/claude.sh before it has run,
// estimated from the local run's request counts and hedge share.
type Q4ClaudeEstimate struct {
	// Command repeats the local run's settings, so the estimate is for
	// exactly the run it names.
	Command    string
	PriceModel string
	// PerArm is rate × (duration + warm-up), Upstream the attempts over
	// both arms with the local run's hedge shares.
	PerArm, Upstream int
	HedgeShare       string
	MaxTokens        int
	// Input is the synthetic run's prompt tokens per upstream request
	// ("" if there is no synthetic run).
	Input string
	USD   string
}

// estimateQ4Claude prices claude.sh at the local run's settings: per arm,
// rate × (duration + warm-up) client requests times one plus that arm's
// hedge share in the local run; every attempt at max_tokens output, and
// the prompt at the synthetic run's ledger count when there is one.
func estimateQ4Claude(local *Q4Live, synthetic *Q4) (*Q4ClaudeEstimate, error) {
	price, ok := cost.Defaults[Q4PriceModel]
	if !ok {
		return nil, fmt.Errorf("results: no list price for %s", Q4PriceModel)
	}
	d, err := time.ParseDuration(local.Meta.Duration)
	if err != nil {
		return nil, fmt.Errorf("results: %s meta.json: duration: %w", Q4OllamaDir, err)
	}
	w, err := time.ParseDuration(local.Meta.Warmup)
	if err != nil {
		return nil, fmt.Errorf("results: %s meta.json: warmup: %w", Q4OllamaDir, err)
	}
	perArm := local.Meta.Rate * (d + w).Seconds()
	var upstream float64
	for _, s := range local.hedgeShares {
		upstream += perArm * (1 + s)
	}
	var input float64
	e := &Q4ClaudeEstimate{
		Command: fmt.Sprintf("ANTHROPIC_API_KEY=... RATE=%s DURATION=%s WARMUP=%s MAX_TOKENS=%d eval/hedge/claude.sh",
			num(local.Meta.Rate), local.Meta.Duration, local.Meta.Warmup, local.Meta.MaxTokens),
		PriceModel: Q4PriceModel,
		PerArm:     int(math.Round(perArm)), Upstream: int(math.Round(upstream)),
		HedgeShare: local.Rows[len(local.Rows)-1].ExtraPercent,
		MaxTokens:  local.Meta.MaxTokens,
	}
	if synthetic != nil && synthetic.Rows[0].Upstream > 0 {
		off := synthetic.Rows[0]
		input = math.Round(float64(off.InputTokens)/float64(off.Upstream)*10) / 10
		e.Input = strconv.FormatFloat(input, 'f', -1, 64)
	}
	usd := upstream * (input*float64(price.Base.Input) + float64(local.Meta.MaxTokens)*float64(price.Base.Output)) / float64(cost.Dollar)
	e.USD = usd6(usd)
	return e, nil
}

// usd6 formats dollars to a ten-thousandth of a cent: a live run at one
// request a second costs fractions of a cent.
func usd6(usd float64) string { return fmt.Sprintf("$%.6f", usd) }

// Package bench is an open-loop HTTP load generator for the two chat APIs the
// gateway speaks. It answers PLAN.md's Q1 (what the gateway adds to a
// request) by running the same load directly against the fake provider and
// through the gateway.
//
// Arrivals are at a constant rate: request i is scheduled at start + i/Rate
// and sent at that time in its own goroutine whether or not earlier requests
// have finished, so a slow target cannot lower the offered load. Latency is
// measured from the scheduled time, not from the moment the request was
// actually sent, which avoids coordinated omission; the gap between the two
// is reported separately as send delay so a lagging generator is visible.
package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Wire formats a run can speak.
const (
	FormatOpenAI    = "openai"
	FormatAnthropic = "anthropic"
)

// Defaults applied by Run to zero Config fields.
const (
	DefaultTimeout     = 30 * time.Second
	DefaultMaxInFlight = 4096
	// DefaultAnthropicMaxTokens is used for the Anthropic format, where
	// max_tokens is required. The OpenAI format omits it when zero.
	DefaultAnthropicMaxTokens = 64
	// DefaultRepeatPool is the number of distinct repeated prompts when
	// Repeat is set and RepeatPool is not.
	DefaultRepeatPool = 50
)

// Config describes one run.
type Config struct {
	// URL is the target's base URL without the /v1 path, for example
	// http://127.0.0.1:8080.
	URL string
	// Format is FormatOpenAI (the default) or FormatAnthropic.
	Format string
	// APIKey is sent as a bearer token (OpenAI) or x-api-key (Anthropic).
	// It is never part of the Result.
	APIKey string
	Model  string
	Stream bool
	// Rate is the arrival rate in requests per second.
	Rate float64
	// Duration is the measured window; Warmup precedes it. Requests
	// scheduled during Warmup are sent but excluded from all statistics.
	Duration, Warmup time.Duration
	// Timeout bounds each request from the moment it is sent.
	Timeout time.Duration
	// MaxInFlight caps outstanding requests. A request whose scheduled time
	// finds the cap reached is counted as dropped, not delayed.
	MaxInFlight int
	// MaxTokens is the output limit sent with each request.
	MaxTokens int
	// Label names the run in the Result, for example "gateway-stream-200".
	Label string
	// KeepSamples records every measured request's latency and TTFT in the
	// Result, so a later analysis can compute statistics the summary does
	// not have, such as a confidence interval on a percentile.
	KeepSamples bool
	// Repeat is the share of requests, in [0, 1], that reuse a prompt from
	// a fixed pool of RepeatPool prompts instead of sending a unique one,
	// so a cache in front of the target sees repeats. The first use of a
	// pool prompt is a miss that fills the cache; later uses can hit.
	// Whether request i repeats, and which pool prompt it uses, depends
	// only on Seed and i, so a run is reproducible whatever the timing.
	Repeat     float64
	RepeatPool int
	Seed       uint64
	// CacheDirective, if set, is sent as the gateway's cache directive
	// header (off, exact or semantic) on every request.
	CacheDirective string
	// Log, if set, receives one JSON line per scheduled request when the
	// run ends, warm-up included, in schedule order (see Record).
	Log io.Writer
}

// withDefaults validates c and fills zero fields.
func (c Config) withDefaults() (Config, error) {
	c.URL = strings.TrimRight(c.URL, "/")
	if c.URL == "" {
		return c, errors.New("bench: URL is required")
	}
	if c.Format == "" {
		c.Format = FormatOpenAI
	}
	if c.Format != FormatOpenAI && c.Format != FormatAnthropic {
		return c, fmt.Errorf("bench: unknown format %q (want %s or %s)", c.Format, FormatOpenAI, FormatAnthropic)
	}
	if c.Rate <= 0 || math.IsInf(c.Rate, 0) || math.IsNaN(c.Rate) {
		return c, fmt.Errorf("bench: rate must be positive, got %v", c.Rate)
	}
	if c.Duration <= 0 {
		return c, fmt.Errorf("bench: duration must be positive, got %v", c.Duration)
	}
	if c.Warmup < 0 {
		return c, fmt.Errorf("bench: warmup must not be negative, got %v", c.Warmup)
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = DefaultMaxInFlight
	}
	if c.MaxTokens < 0 {
		return c, fmt.Errorf("bench: max tokens must not be negative, got %d", c.MaxTokens)
	}
	if c.Format == FormatAnthropic && c.MaxTokens == 0 {
		c.MaxTokens = DefaultAnthropicMaxTokens
	}
	if c.Repeat < 0 || c.Repeat > 1 || math.IsNaN(c.Repeat) {
		return c, fmt.Errorf("bench: repeat must be in [0, 1], got %v", c.Repeat)
	}
	if c.RepeatPool < 0 {
		return c, fmt.Errorf("bench: repeat pool must not be negative, got %d", c.RepeatPool)
	}
	if c.Repeat > 0 && c.RepeatPool == 0 {
		c.RepeatPool = DefaultRepeatPool
	}
	return c, nil
}

// ConfigEcho is the effective configuration as recorded in a Result. It
// leaves out the API key on purpose.
type ConfigEcho struct {
	Label       string  `json:"label,omitempty"`
	URL         string  `json:"url"`
	Format      string  `json:"format"`
	Model       string  `json:"model"`
	Stream      bool    `json:"stream"`
	Rate        float64 `json:"rate"`
	DurationSec float64 `json:"duration_seconds"`
	WarmupSec   float64 `json:"warmup_seconds"`
	TimeoutSec  float64 `json:"timeout_seconds"`
	MaxInFlight int     `json:"max_in_flight"`
	MaxTokens   int     `json:"max_tokens"`
	// The repeat workload and the cache directive are recorded only when
	// set, so a result without them reads as before.
	Repeat         float64 `json:"repeat,omitempty"`
	RepeatPool     int     `json:"repeat_pool,omitempty"`
	Seed           uint64  `json:"seed,omitempty"`
	CacheDirective string  `json:"cache_directive,omitempty"`
}

func echo(c Config) ConfigEcho {
	e := ConfigEcho{
		Label:       c.Label,
		URL:         c.URL,
		Format:      c.Format,
		Model:       c.Model,
		Stream:      c.Stream,
		Rate:        c.Rate,
		DurationSec: c.Duration.Seconds(),
		WarmupSec:   c.Warmup.Seconds(),
		TimeoutSec:  c.Timeout.Seconds(),
		MaxInFlight: c.MaxInFlight,
		MaxTokens:   c.MaxTokens,
	}
	if c.Repeat > 0 {
		e.Repeat, e.RepeatPool, e.Seed = c.Repeat, c.RepeatPool, c.Seed
	}
	e.CacheDirective = c.CacheDirective
	return e
}

// Result is the outcome of a run. Its JSON form is the raw result file the
// renderer reads, so field names are stable.
//
// Counts and statistics cover the measured window only (requests scheduled
// after Warmup). Latency and TTFT are over successful requests; SendDelay is
// over every request that was sent, successful or not.
type Result struct {
	Config    ConfigEcho `json:"config"`
	StartedAt time.Time  `json:"started_at"`
	// WallSeconds runs from the first scheduled request, warm-up included,
	// until the last request finished.
	WallSeconds float64 `json:"wall_seconds"`
	// Requests is the number of measured requests scheduled; WarmupRequests
	// the number scheduled during warm-up.
	Requests       int `json:"requests"`
	WarmupRequests int `json:"warmup_requests"`
	// OfferedRate is Requests over Duration. AchievedRate is the measured
	// requests that succeeded over Duration: the offered rate times the
	// success fraction.
	OfferedRate  float64 `json:"offered_rps"`
	AchievedRate float64 `json:"achieved_rps"`
	// PeakInFlight is the most requests outstanding at once, warm-up
	// included.
	PeakInFlight int            `json:"peak_in_flight"`
	Outcomes     map[string]int `json:"outcomes"`
	// Latency is total time from scheduled start to the complete reply.
	Latency Stats `json:"latency"`
	// TTFT is time from scheduled start to the first generated text; equal
	// to Latency for non-streamed requests.
	TTFT Stats `json:"ttft"`
	// SendDelay is actual send time minus scheduled time.
	SendDelay Stats    `json:"send_delay"`
	Hardware  Hardware `json:"hardware"`
	// Samples holds the per-request values behind Latency and TTFT when
	// Config.KeepSamples is set.
	Samples *Samples `json:"samples,omitempty"`
}

// Samples are the measured successful requests' latency and TTFT in
// milliseconds, rounded to the microsecond, in the order the requests were
// scheduled. TTFT omits requests that produced no text.
type Samples struct {
	LatencyMS []float64 `json:"latency_ms"`
	TTFTMS    []float64 `json:"ttft_ms"`
}

// Errors is the number of measured requests whose outcome was not ok,
// dropped requests included.
func (r *Result) Errors() int {
	n := 0
	for k, v := range r.Outcomes {
		if k != OutcomeOK {
			n += v
		}
	}
	return n
}

// newClient returns an HTTP client tuned for many concurrent requests to one
// host: enough idle connections to reuse one per in-flight request, no proxy
// from the environment, and a short dial timeout so a dead target fails fast.
func newClient(maxInFlight int) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          maxInFlight,
		MaxIdleConnsPerHost:   maxInFlight,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		ResponseHeaderTimeout: 0, // per-request contexts bound the wait
	}}
}

// scheduled is one request slot: its scheduled time, whether it fell in the
// warm-up, and its timing once sent.
type scheduled struct {
	at     time.Time
	warmup bool
	sample
}

// Run executes cfg and returns its Result. If ctx ends early, Run stops
// scheduling, waits for the requests already out (which see the
// cancellation) and returns the partial Result together with ctx's error.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	client := newClient(cfg.MaxInFlight)
	defer client.CloseIdleConnections()

	interval := float64(time.Second) / cfg.Rate
	nWarm := int(math.Ceil(float64(cfg.Warmup) / interval))
	nTotal := int(math.Ceil(float64(cfg.Warmup+cfg.Duration) / interval))

	var (
		inFlight atomic.Int64
		peak     int64
		wg       sync.WaitGroup
		results  = make([]scheduled, nTotal)
	)
	startedAt := time.Now()
	start := startedAt
	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C

	n := 0
schedule:
	for ; n < nTotal; n++ {
		at := start.Add(time.Duration(float64(n) * interval))
		if d := time.Until(at); d > 0 {
			timer.Reset(d)
			select {
			case <-ctx.Done():
				break schedule
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			break schedule
		}
		results[n].at = at
		results[n].warmup = n < nWarm
		// Only this goroutine increments, so check-then-add cannot overshoot.
		if inFlight.Load() >= int64(cfg.MaxInFlight) {
			results[n].outcome = OutcomeDropped
			continue
		}
		if cur := inFlight.Add(1); cur > peak {
			peak = cur
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer inFlight.Add(-1)
			results[i].sample = send(ctx, client, &cfg, i)
		}(n)
	}
	wg.Wait()
	end := time.Now()

	res := summarise(cfg, results[:n], start, end)
	res.StartedAt = startedAt.UTC()
	res.PeakInFlight = int(peak)
	res.Hardware = DetectHardware()
	var logErr error
	if cfg.Log != nil {
		logErr = writeLog(cfg.Log, &cfg, results[:n], start)
	}
	if err := ctx.Err(); err != nil {
		return res, errors.Join(fmt.Errorf("bench: run interrupted: %w", err), logErr)
	}
	return res, logErr
}

// summarise builds the Result from the scheduled requests.
func summarise(cfg Config, rs []scheduled, start, end time.Time) *Result {
	res := &Result{
		Config:      echo(cfg),
		WallSeconds: round3(end.Sub(start).Seconds()),
		Outcomes:    map[string]int{},
	}
	var latency, ttft, delay []time.Duration
	for _, r := range rs {
		if r.warmup {
			res.WarmupRequests++
			continue
		}
		res.Requests++
		res.Outcomes[r.outcome]++
		if r.outcome == OutcomeDropped {
			continue
		}
		if !r.sent.IsZero() {
			delay = append(delay, r.sent.Sub(r.at))
		}
		if r.outcome != OutcomeOK {
			continue
		}
		latency = append(latency, r.done.Sub(r.at))
		if !r.firstToken.IsZero() {
			ttft = append(ttft, r.firstToken.Sub(r.at))
		}
	}
	res.OfferedRate = round3(float64(res.Requests) / cfg.Duration.Seconds())
	res.AchievedRate = round3(float64(res.Outcomes[OutcomeOK]) / cfg.Duration.Seconds())
	res.Latency = Summarise(latency)
	res.TTFT = Summarise(ttft)
	res.SendDelay = Summarise(delay)
	if cfg.KeepSamples {
		res.Samples = &Samples{LatencyMS: millis(latency), TTFTMS: millis(ttft)}
	}
	return res
}

func millis(ds []time.Duration) []float64 {
	out := make([]float64, len(ds))
	for i, d := range ds {
		out[i] = round3(ms(d))
	}
	return out
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

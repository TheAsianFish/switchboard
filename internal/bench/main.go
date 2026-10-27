package bench

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// KeyEnv is the environment variable --key defaults to.
const KeyEnv = "SWITCHBOARD_BENCH_KEY"

// Main implements `switchboard bench`. args excludes the subcommand name. The
// Result is written as JSON to --out, or to stdout without it; a one-line
// summary goes to stderr. It returns the process exit code: 0 on success
// (whatever the requests' outcomes), 1 if the run failed, 2 on bad usage.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg Config
	var out string
	fs.StringVar(&cfg.URL, "url", "http://127.0.0.1:8080", "target base URL, without /v1")
	fs.StringVar(&cfg.Format, "format", FormatOpenAI, "wire format: openai or anthropic")
	// The environment default is applied after parsing so -help never
	// prints the key.
	fs.StringVar(&cfg.APIKey, "key", "", "API key (default $"+KeyEnv+")")
	fs.StringVar(&cfg.Model, "model", "fake", "model name to request")
	fs.BoolVar(&cfg.Stream, "stream", false, "request streamed replies and measure time to first token")
	fs.Float64Var(&cfg.Rate, "rate", 50, "arrival rate in requests per second")
	fs.DurationVar(&cfg.Duration, "duration", 10*time.Second, "measured window")
	fs.DurationVar(&cfg.Warmup, "warmup", 2*time.Second, "warm-up before the measured window, sent but not counted")
	fs.DurationVar(&cfg.Timeout, "timeout", DefaultTimeout, "per-request timeout")
	fs.IntVar(&cfg.MaxInFlight, "max-in-flight", 1024, "cap on outstanding requests; requests beyond it are dropped")
	fs.IntVar(&cfg.MaxTokens, "max-tokens", 0, "max_tokens per request (0: omit for openai, 64 for anthropic)")
	fs.StringVar(&cfg.Label, "label", "", "name recorded in the result")
	fs.BoolVar(&cfg.KeepSamples, "samples", false, "record every measured request's latency and TTFT in the result")
	fs.StringVar(&out, "out", "", "write the result JSON to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv(KeyEnv)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "bench: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	res, err := Run(ctx, cfg)
	if res == nil {
		fmt.Fprintln(stderr, err)
		return 2 // only configuration errors return no result
	}
	code := 0
	if err != nil {
		fmt.Fprintln(stderr, err)
		code = 1
	}
	b, merr := json.MarshalIndent(res, "", "  ")
	if merr != nil {
		fmt.Fprintf(stderr, "bench: encode result: %v\n", merr)
		return 1
	}
	b = append(b, '\n')
	if out != "" {
		if werr := os.WriteFile(out, b, 0o644); werr != nil {
			fmt.Fprintf(stderr, "bench: %v\n", werr)
			return 1
		}
	} else if _, werr := stdout.Write(b); werr != nil {
		fmt.Fprintf(stderr, "bench: write result: %v\n", werr)
		return 1
	}
	fmt.Fprintln(stderr, Summary(res))
	return code
}

// Summary is a one-line human description of r.
func Summary(r *Result) string {
	name := r.Config.Label
	if name == "" {
		name = r.Config.URL
	}
	s := fmt.Sprintf("%s: %d requests, offered %.1f/s, achieved %.1f/s, ok %d, errors %d, peak in flight %d; latency p50 %.3f ms p99 %.3f ms",
		name, r.Requests, r.OfferedRate, r.AchievedRate, r.Outcomes[OutcomeOK], r.Errors(), r.PeakInFlight, r.Latency.P50, r.Latency.P99)
	if r.Config.Stream {
		s += fmt.Sprintf("; ttft p50 %.3f ms p99 %.3f ms", r.TTFT.P50, r.TTFT.P99)
	}
	return s + fmt.Sprintf("; send delay p99 %.3f ms", r.SendDelay.P99)
}

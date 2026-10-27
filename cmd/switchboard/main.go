// Command switchboard is the gateway and its tooling in one binary.
//
//	switchboard serve             run the gateway
//	switchboard fake-provider     run the deterministic fake backend
//	switchboard bench             drive load at a target and record latency
//	switchboard eval results      render RESULTS.md from eval/results/
//	switchboard eval limits       Q2: over-admission from the fake provider's ledger
//	switchboard eval budget       Q2: shared budget spend against the ledger
//	switchboard eval cache-data   Q3: build the cache pair sets (needs Ollama)
//	switchboard version           print the build version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cacheeval"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/limitseval"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/provider"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/results"
	"github.com/TheAsianFish/switchboard/internal/server"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

const usage = `usage: switchboard <command> [flags]

commands:
  serve             run the gateway (config: --config or SWITCHBOARD_CONFIG)
  fake-provider     run the deterministic fake backend
  bench             drive load at a target and record latency
  eval results      render RESULTS.md from eval/results/
  eval limits       Q2: over-admission from the fake provider's ledger
  eval budget       Q2: shared budget spend against the ledger
  eval cache-data   Q3: build the cache pair sets (needs Ollama)
  version           print the build version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return exit(stderr, serve(ctx, args[1:], stderr))
	case "fake-provider":
		return exit(stderr, fakeProvider(ctx, args[1:], stderr))
	case "bench":
		return bench.Main(ctx, args[1:], stdout, stderr)
	case "eval":
		if len(args) > 1 && args[1] == "results" {
			return results.Main(args[2:], stdout, stderr)
		}
		if len(args) > 1 && (args[1] == "limits" || args[1] == "budget") {
			return limitseval.Main(ctx, args[1:], stdout, stderr)
		}
		if len(args) > 1 && args[1] == "cache-data" {
			return cacheeval.Main(ctx, args[1:], stdout, stderr)
		}
		_, _ = fmt.Fprintln(stderr, "usage: switchboard eval results|limits|budget|cache-data [flags]")
		return 2
	case "version":
		_, _ = fmt.Fprintln(stdout, version())
		return 0
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func exit(stderr io.Writer, err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		_, _ = fmt.Fprintf(stderr, "switchboard: %v\n", err)
		return 1
	}
}

var errUsage = errors.New("usage")

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(fs.Output(), "unexpected arguments: %v\n", fs.Args())
		return errUsage
	}
	return nil
}

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfigPath(), "path to switchboard.yaml (env SWITCHBOARD_CONFIG)")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*path, os.Getenv)
	if err != nil {
		return err
	}
	log, err := telemetry.NewLogger(stderr, cfg.Log.Format, cfg.Log.Level)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	built := make(map[string]coreProvider, len(cfg.Providers))
	for _, p := range cfg.Providers {
		prov, err := provider.Build(p)
		if err != nil {
			return err
		}
		if provider.Disabled(prov) {
			log.Warn("provider has no API key; its models will refuse requests",
				slog.String("provider", p.Name), slog.String("env", p.APIKeyEnv))
		}
		built[p.Name] = prov
	}
	if len(cfg.Keys) == 0 {
		log.Warn("no API keys configured; every request will be refused")
	}
	metrics := telemetry.NewMetrics(true)
	pol, closePolicy, err := buildPolicy(ctx, cfg, metrics, log)
	if err != nil {
		return err
	}
	defer closePolicy()
	gw := pipeline.New(auth.NewKeys(cfg.Keys), routes(cfg, built, log), pol)
	log.Info("gateway configured",
		slog.String("config", *path), slog.Int("providers", len(cfg.Providers)),
		slog.Int("models", len(cfg.Models)), slog.Int("keys", len(cfg.Keys)), slog.String("version", version()))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	adminErr := make(chan error, 1)
	go func() {
		err := server.Run(ctx, cfg.AdminListen, adminHandler(metrics), log, nil)
		if err != nil {
			cancel() // the gateway without its metrics is not worth running
		}
		adminErr <- err
	}()
	err = server.Run(ctx, cfg.Listen, server.Handler(gw, log), log, nil)
	cancel()
	if aerr := <-adminErr; aerr != nil {
		return fmt.Errorf("admin listener: %w", aerr)
	}
	return err
}

// coreProvider is the provider interface, named here to keep signatures
// short.
type coreProvider = core.Provider

func defaultConfigPath() string {
	if p := os.Getenv("SWITCHBOARD_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("switchboard.yaml"); err == nil {
		return "switchboard.yaml"
	}
	return ""
}

func fakeProvider(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("fake-provider", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:9101", "address to listen on")
	latency := fs.Duration("latency", fake.DefaultConfig.Latency, "delay before the first byte (and the whole reply when not streaming)")
	tokens := fs.Int("tokens", fake.DefaultConfig.Tokens, "output tokens per reply")
	interval := fs.Duration("token-interval", fake.DefaultConfig.TokenInterval, "delay between streamed tokens")
	logLevel := fs.String("log-level", envOr("SWITCHBOARD_LOG_LEVEL", "warn"), "log level; warn by default so a benchmark measures serving, not logging")
	logFormat := fs.String("log-format", envOr("SWITCHBOARD_LOG_FORMAT", "json"), "json or text")
	if err := parse(fs, args); err != nil {
		return err
	}
	log, err := telemetry.NewLogger(stderr, *logFormat, *logLevel)
	if err != nil {
		return err
	}
	fp := fake.New(fake.Config{Latency: *latency, Tokens: *tokens, TokenInterval: *interval})
	gw := pipeline.Open{Provider: fp, Names: []string{"fake"}}
	mux := http.NewServeMux()
	mux.Handle("/ledger", fp.LedgerHandler())
	mux.Handle("/ledger/", fp.LedgerHandler())
	mux.Handle("/", server.Handler(gw, log))
	log.Warn("fake provider starting", slog.String("listen", *listen), slog.Duration("latency", *latency),
		slog.Int("tokens", *tokens), slog.Duration("token_interval", *interval))
	return server.Run(ctx, *listen, mux, log, nil)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := info.Main.Version
	var rev, modified string
	var at time.Time
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		case "vcs.time":
			at, _ = time.Parse(time.RFC3339, s.Value)
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		v = fmt.Sprintf("%s (%s%s, %s)", v, rev, modified, at.Format("2006-01-02"))
	}
	return v
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/admin"
	"github.com/TheAsianFish/switchboard/internal/admin/ui"
	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/hedge"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/router"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// buildPolicy builds the limiter and the budget ledger the config asks
// for: shared through Redis with a per-replica fallback when redis.url is
// set, per process otherwise. close releases the Redis client. rdb is the
// client, nil without redis.url, for the admin fleet view; onDegraded is
// told when a component moves to or from its fallback.
func buildPolicy(ctx context.Context, cfg *config.Config, m *telemetry.Metrics, onDegraded func(string, bool), log *slog.Logger) (pol pipeline.Policy, closeFn func(), rdb *redis.Client, err error) {
	alg := cfg.RateLimit.Algorithm
	// Refuse to start on an algorithm that cannot admit anything, rather
	// than failing every request later.
	probe, err := ratelimit.NewMemory(alg, time.Now)
	if err != nil {
		return pol, nil, nil, err
	}
	if _, err := probe.Allow(ctx, "startup-probe", ratelimit.Limit{Rate: 1, Period: time.Second, Burst: 1}, 1); errors.Is(err, ratelimit.ErrNotImplemented) {
		return pol, nil, nil, fmt.Errorf("ratelimit.algorithm %q is not implemented yet; use %q", alg, ratelimit.AlgorithmFixedWindow)
	}
	pol.Metrics = m
	ttl := cfg.Budget.ReservationTTL
	if cfg.Redis.URL == "" {
		pol.Limiter, _ = ratelimit.NewMemory(alg, time.Now)
		pol.Ledger = budget.NewMemory(ttl, time.Now)
		pol.Cache, pol.CacheMaxTemperature = buildCache(cfg, nil, m, log), cfg.Cache.MaxTemperature
		if cfg.RateLimit.Replicas > 1 {
			log.Warn("redis.url is not set: every replica enforces the full limits and budgets on its own",
				slog.Int("replicas", cfg.RateLimit.Replicas))
		}
		return pol, waitFills(pol.Cache), nil, nil
	}

	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return pol, nil, nil, fmt.Errorf("redis.url: %w", err)
	}
	// Fail fast and let the degrade policy answer: a request should not
	// wait out dial retries against a store that is down.
	opts.DialTimeout = cfg.Redis.Timeout
	opts.ReadTimeout = cfg.Redis.Timeout
	opts.WriteTimeout = cfg.Redis.Timeout
	opts.PoolTimeout = cfg.Redis.Timeout
	opts.MaxRetries = -1
	rdb = redis.NewClient(opts)

	primary, err := ratelimit.New(alg, rdb, cfg.Redis.KeyPrefix)
	if err != nil {
		_ = rdb.Close()
		return pol, nil, nil, err
	}
	local, _ := ratelimit.NewMemory(alg, time.Now)
	lim := ratelimit.NewDegrading(primary, local, cfg.RateLimit.Replicas)
	lim.RetryInterval = cfg.RateLimit.RetryInterval
	lim.Timeout = cfg.Redis.Timeout
	lim.OnState = func(d bool) { m.Degraded("ratelimit", d); onDegraded("ratelimit", d) }
	pol.Limiter = lim

	grace, _ := cost.ParseUSD(string(cfg.Budget.GraceUSD)) // validated
	pol.Ledger = budget.NewDegrading(budget.NewRedis(rdb, cfg.Redis.KeyPrefix, ttl, time.Now), budget.DegradeOptions{
		Grace:         grace,
		RetryInterval: cfg.RateLimit.RetryInterval,
		Timeout:       cfg.Redis.Timeout,
		OnState:       func(d bool) { m.Degraded("budget", d); onDegraded("budget", d) },
	})

	pol.Cache, pol.CacheMaxTemperature = buildCache(cfg, rdb, m, log), cfg.Cache.MaxTemperature

	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		// Start anyway: the degrade policy covers an unreachable store, and
		// a replica that refuses to start because Redis is down turns one
		// outage into two.
		log.Warn("redis is unreachable at startup; limits and budgets start on the per-replica fallback",
			slog.String("error", err.Error()))
	}
	log.Info("shared limits and budgets", slog.String("algorithm", alg), slog.Int("replicas", cfg.RateLimit.Replicas),
		slog.String("grace", grace.String()))
	wait := waitFills(pol.Cache)
	return pol, func() { wait(); _ = rdb.Close() }, rdb, nil
}

// waitFills stops new cache writes at shutdown and lets those in flight
// finish before the store goes away.
func waitFills(c *cache.Cache) func() {
	return func() {
		if c != nil {
			c.Close()
		}
	}
}

// buildCache builds the response cache, or returns nil when no key may use
// it. Entries live in Redis when rdb is set, so every replica serves what
// any of them stored; otherwise in this process. The vector index is
// created on the first embedding, once its dimension is known.
func buildCache(cfg *config.Config, rdb *redis.Client, m *telemetry.Metrics, log *slog.Logger) *cache.Cache {
	semantic, enabled := false, false
	for _, k := range cfg.Keys {
		mode, _ := cache.ParseMode(k.Cache) // validated
		enabled = enabled || mode != cache.ModeOff
		semantic = semantic || mode == cache.ModeSemantic
	}
	if !enabled {
		return nil
	}
	c := cfg.Cache
	opts := cache.Options{
		TTL:          c.TTL,
		Timeout:      c.Timeout,
		Threshold:    c.Semantic.Threshold,
		MatchNumbers: c.Semantic.MatchNumbers == nil || *c.Semantic.MatchNumbers,
		Metrics:      m,
	}
	if rdb != nil {
		opts.KV = cache.NewRedisKV(rdb, cfg.Redis.KeyPrefix)
		opts.RetryInterval = cfg.RateLimit.RetryInterval
	} else {
		opts.KV = cache.NewMemoryKV(c.MaxEntries, time.Now)
	}
	if semantic {
		opts.Embedder = embed.Memoized(&embed.Ollama{BaseURL: c.Semantic.EmbedURL, Name: c.Semantic.Model, Timeout: c.Timeout})
		opts.NewIndex = func(dims int) (cache.Index, error) {
			if rdb == nil {
				return cache.NewMemoryIndex(dims, c.MaxEntries, time.Now), nil
			}
			return cache.NewRedisIndex(rdb, cache.RedisIndexOptions{
				Prefix: cfg.Redis.KeyPrefix, Name: fmt.Sprintf("%s:%d", slug(c.Semantic.Model), dims),
				Dims: dims, Algorithm: strings.ToUpper(c.Semantic.Index),
			})
		}
	}
	log.Info("response cache", slog.String("default_mode", c.Mode), slog.Bool("semantic", semantic),
		slog.String("embed_model", c.Semantic.Model), slog.Float64("threshold", c.Semantic.Threshold),
		slog.String("index", c.Semantic.Index), slog.Bool("shared", rdb != nil))
	return cache.New(opts)
}

// slug keeps letters, digits and dots of an embedding model's name, so it
// can be part of a Redis index name.
func slug(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, s)
}

// adminHandler serves the loopback admin listener: /metrics, /providers
// (each provider's breaker state and hedge delays), and the admin API and
// page.
func adminHandler(m *telemetry.Metrics, providers []*hedge.Provider, adm *admin.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", adm.Handler())
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /providers", func(w http.ResponseWriter, _ *http.Request) {
		out := make([]hedge.Health, 0, len(providers))
		for _, p := range providers {
			out = append(out, p.Health())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Providers []hedge.Health `json:"providers"`
		}{out})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// routes builds the pipeline routes with each model's price.
func routes(cfg *config.Config, built map[string]coreProvider, log *slog.Logger) []pipeline.Route {
	out := make([]pipeline.Route, 0, len(cfg.Models))
	for _, mdl := range cfg.Models {
		price, priced, _ := mdl.ResolvedPrice() // validated
		if !priced {
			log.Info("model has no price; its spend is not counted", slog.String("model", mdl.Name))
		}
		out = append(out, pipeline.Route{
			Model: mdl.Name, ProviderName: mdl.Provider, Upstream: mdl.Upstream, Provider: built[mdl.Provider],
			Price: price, Priced: priced, OutputEstimate: mdl.MaxOutputTokens,
		})
	}
	return out
}

// resilienceConfig fills a provider's resilience settings from the
// defaults.
func resilienceConfig(r config.Resilience) hedge.Config {
	c := hedge.DefaultConfig
	if r.Retries != nil {
		c.Retries = *r.Retries
	}
	if r.BackoffBase > 0 {
		c.BackoffBase = r.BackoffBase
	}
	if r.BackoffMax > 0 {
		c.BackoffMax = r.BackoffMax
	}
	if r.MaxRetryAfter > 0 {
		c.MaxRetryAfter = r.MaxRetryAfter
	}
	if r.Breaker.Failures != nil {
		c.Breaker.Failures = *r.Breaker.Failures
	}
	if r.Breaker.Cooldown > 0 {
		c.Breaker.Cooldown = r.Breaker.Cooldown
	}
	h := r.Hedge
	c.Hedge.Enabled, c.Hedge.Delay = h.Enabled, h.Delay
	if h.Quantile > 0 {
		c.Hedge.Quantile = h.Quantile
	}
	if h.Window > 0 {
		c.Hedge.Window = h.Window
	}
	if h.MinSamples > 0 {
		c.Hedge.MinSamples = h.MinSamples
	}
	if h.Budget != nil {
		c.Hedge.Budget = *h.Budget
	}
	if h.Burst > 0 {
		c.Hedge.Burst = h.Burst
	}
	// A window smaller than the default sample floor must still hedge.
	c.Hedge.MinSamples = min(c.Hedge.MinSamples, c.Hedge.Window)
	return c
}

// buildAdmin builds the admin API: this replica's recorder, the fleet
// view through Redis when there is one, and the page.
func buildAdmin(cfg *config.Config, rec *admin.Recorder, rdb *redis.Client, keys *auth.Keys, pol pipeline.Policy, providers []*hedge.Provider, log *slog.Logger) *admin.Server {
	id := cfg.Admin.ReplicaID
	if id == "" {
		id, _ = os.Hostname()
	}
	if id == "" {
		id = "switchboard"
	}
	o := admin.Options{
		ID: id, Version: version(), Started: time.Now(), Recorder: rec,
		Keys: keys.Principals(), Ledger: pol.Ledger, CacheEnabled: pol.Cache != nil,
		TraceURL: cfg.Admin.TraceURL, AllowedHosts: cfg.Admin.AllowedHosts, UI: ui.FS(), Log: log,
		Providers: func() []admin.ProviderHealth {
			out := make([]admin.ProviderHealth, 0, len(providers))
			for _, p := range providers {
				h := p.Health()
				out = append(out, admin.ProviderHealth{Provider: h.Provider, Breaker: h.Breaker, RetryAfterMS: h.RetryAfterMS, Hedging: h.Hedging, HedgeDelayMS: h.HedgeDelayMS})
			}
			return out
		},
	}
	if rdb != nil {
		o.Fleet = admin.NewFleet(rdb, cfg.Redis.KeyPrefix, log)
	}
	if r := pol.Router; r != nil {
		o.Router = admin.Router{Enabled: true, Target: r.Target(), Candidates: routerCandidates(r), Fallback: r.Weights().Fallback}
	}
	log.Info("admin page", slog.String("listen", cfg.AdminListen), slog.String("replica", id), slog.Bool("fleet", rdb != nil))
	return admin.NewServer(o)
}

// routerCandidates names the router's candidates in its weights' order.
func routerCandidates(r *router.Router) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, c := range r.Weights().Candidates {
		out = append(out, c.Model)
	}
	return out
}

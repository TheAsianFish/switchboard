package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// buildPolicy builds the limiter and the budget ledger the config asks
// for: shared through Redis with a per-replica fallback when redis.url is
// set, per process otherwise. close releases the Redis client.
func buildPolicy(ctx context.Context, cfg *config.Config, m *telemetry.Metrics, log *slog.Logger) (pol pipeline.Policy, closeFn func(), err error) {
	alg := cfg.RateLimit.Algorithm
	// Refuse to start on an algorithm that cannot admit anything, rather
	// than failing every request later.
	probe, err := ratelimit.NewMemory(alg, time.Now)
	if err != nil {
		return pol, nil, err
	}
	if _, err := probe.Allow(ctx, "startup-probe", ratelimit.Limit{Rate: 1, Period: time.Second, Burst: 1}, 1); errors.Is(err, ratelimit.ErrNotImplemented) {
		return pol, nil, fmt.Errorf("ratelimit.algorithm %q is not implemented yet; use %q", alg, ratelimit.AlgorithmFixedWindow)
	}
	pol.Metrics = m
	ttl := cfg.Budget.ReservationTTL
	if cfg.Redis.URL == "" {
		pol.Limiter, _ = ratelimit.NewMemory(alg, time.Now)
		pol.Ledger = budget.NewMemory(ttl, time.Now)
		if cfg.RateLimit.Replicas > 1 {
			log.Warn("redis.url is not set: every replica enforces the full limits and budgets on its own",
				slog.Int("replicas", cfg.RateLimit.Replicas))
		}
		return pol, func() {}, nil
	}

	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return pol, nil, fmt.Errorf("redis.url: %w", err)
	}
	// Fail fast and let the degrade policy answer: a request should not
	// wait out dial retries against a store that is down.
	opts.DialTimeout = cfg.Redis.Timeout
	opts.ReadTimeout = cfg.Redis.Timeout
	opts.WriteTimeout = cfg.Redis.Timeout
	opts.PoolTimeout = cfg.Redis.Timeout
	opts.MaxRetries = -1
	rdb := redis.NewClient(opts)

	primary, err := ratelimit.New(alg, rdb, cfg.Redis.KeyPrefix)
	if err != nil {
		_ = rdb.Close()
		return pol, nil, err
	}
	local, _ := ratelimit.NewMemory(alg, time.Now)
	lim := ratelimit.NewDegrading(primary, local, cfg.RateLimit.Replicas)
	lim.RetryInterval = cfg.RateLimit.RetryInterval
	lim.Timeout = cfg.Redis.Timeout
	lim.OnState = func(d bool) { m.Degraded("ratelimit", d) }
	pol.Limiter = lim

	grace, _ := cost.ParseUSD(string(cfg.Budget.GraceUSD)) // validated
	pol.Ledger = budget.NewDegrading(budget.NewRedis(rdb, cfg.Redis.KeyPrefix, ttl, time.Now), budget.DegradeOptions{
		Grace:         grace,
		RetryInterval: cfg.RateLimit.RetryInterval,
		Timeout:       cfg.Redis.Timeout,
		OnState:       func(d bool) { m.Degraded("budget", d) },
	})

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
	return pol, func() { _ = rdb.Close() }, nil
}

// adminHandler serves the loopback admin listener: /metrics now, the admin
// API in a later milestone.
func adminHandler(m *telemetry.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
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

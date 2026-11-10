package admin

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// Options configure a Server.
type Options struct {
	// ID names this replica in the fleet; Version and Started describe it.
	ID      string
	Version string
	Started time.Time
	// Recorder is this replica's own record of its calls.
	Recorder *Recorder
	// Fleet shares reports through Redis; nil shows this replica only.
	Fleet *Fleet
	// Interval is how often the report is published; zero means
	// DefaultInterval.
	Interval time.Duration
	// Keys are the configured keys' principals.
	Keys []*auth.Principal
	// Ledger reads budgeted keys' balances; nil when budgets are off.
	Ledger budget.Ledger
	// CacheEnabled says whether the gateway has a response cache.
	CacheEnabled bool
	// Router describes the auto model; Enabled false without one.
	Router Router
	// Providers reports each provider's breaker and hedging state.
	Providers func() []ProviderHealth
	// TraceURL is the trace link template, with "{trace_id}".
	TraceURL string
	// AllowedHosts are extra Host header names the page and API answer,
	// beyond loopback names and IP literals.
	AllowedHosts []string
	// UI is the built page; nil serves no page.
	UI fs.FS
	// Timeout bounds each store read while building an overview.
	Timeout time.Duration
	Log     *slog.Logger
	Now     func() time.Time
}

// Server builds overviews and serves the admin API and page.
type Server struct {
	o Options
}

// NewServer returns a server; call Run to publish to the fleet.
func NewServer(o Options) *Server {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Timeout <= 0 {
		o.Timeout = 500 * time.Millisecond
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Providers == nil {
		o.Providers = func() []ProviderHealth { return nil }
	}
	return &Server{o: o}
}

// self is this replica's current report and recent calls.
func (s *Server) self() (report, []Call) {
	rep, recent := s.o.Recorder.snapshot()
	now := s.o.Now()
	rep.ID, rep.Version, rep.StartedAt, rep.LastSeen = s.o.ID, s.o.Version, s.o.Started.UTC(), now.UTC()
	rep.Providers = s.o.Providers()
	for i := range recent {
		recent[i].Replica = s.o.ID
	}
	return rep, recent
}

// Run publishes this replica's report every interval until ctx ends. A
// failed publish is logged once per outage, not every interval: while
// Redis is down the degrade policies already say so.
func (s *Server) Run(ctx context.Context) {
	if s.o.Fleet == nil {
		return
	}
	t := time.NewTicker(s.o.Interval)
	defer t.Stop()
	failing := false
	for {
		rep, recent := s.self()
		pctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
		err := s.o.Fleet.publish(pctx, rep, recent)
		cancel()
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			s.o.Log.Warn("fleet report not published", slog.String("error", err.Error()))
			failing = true
		case err == nil && failing:
			s.o.Log.Info("fleet reports published again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// MaxRecent caps the overview's recent calls.
const MaxRecent = 200

// Overview builds the page's snapshot: this replica's own state, read
// locally, and every other replica's last report from Redis.
func (s *Server) Overview(ctx context.Context) Overview {
	now := s.o.Now()
	own, ownRecent := s.self()
	ov := Overview{GeneratedAt: now.UTC(), Self: s.o.ID, TraceURL: s.o.TraceURL, Shared: s.o.Fleet != nil}
	reps := []report{own}
	recent := [][]Call{ownRecent}
	if s.o.Fleet != nil {
		rctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
		others, calls, err := s.o.Fleet.read(rctx, now)
		cancel()
		if err != nil {
			ov.FleetError = err.Error()
		}
		for _, r := range others {
			if r.ID == s.o.ID {
				continue // the local snapshot is newer
			}
			reps = append(reps, r)
			recent = append(recent, calls[r.ID])
		}
	}

	sort.Slice(reps, func(i, j int) bool { return reps[i].ID < reps[j].ID })
	keyTotalsAll := map[string]keyTotals{}
	lookups := map[string]int64{}
	decisions := map[[2]string]int64{}
	var saved int64
	for _, r := range reps {
		row := r.Replica
		age := now.Sub(r.LastSeen)
		row.AgeSeconds = max(0, age.Seconds())
		row.Status = status(age)
		row.Self = r.ID == s.o.ID
		if row.Self {
			row.Status, row.AgeSeconds = StatusUp, 0
		}
		if row.Outcomes == nil {
			row.Outcomes = map[string]int64{}
		}
		if row.Degraded == nil {
			row.Degraded = map[string]bool{}
		}
		if row.Providers == nil {
			row.Providers = []ProviderHealth{}
		}
		ov.Replicas = append(ov.Replicas, row)
		for k, v := range r.Keys {
			t := keyTotalsAll[k]
			t.Calls += v.Calls
			t.SpentNanos += v.SpentNanos
			keyTotalsAll[k] = t
		}
		for k, v := range r.Lookups {
			lookups[k] += v
		}
		for _, d := range r.Decisions {
			decisions[[2]string{d.Model, d.Reason}] += d.Count
		}
		saved += r.SavedNanos
	}

	ov.Keys = s.keys(ctx, keyTotalsAll)
	ov.Cache = cacheSummary(s.o.CacheEnabled, lookups, saved)
	ov.Router = s.o.Router
	ov.Router.Candidates = slices.Clone(s.o.Router.Candidates)
	ov.Router.Decisions = []Decision{}
	for k, n := range decisions {
		ov.Router.Decisions = append(ov.Router.Decisions, Decision{Model: k[0], Reason: k[1], Count: n})
	}
	sortDecisions(ov.Router.Decisions)
	ov.Recent = mergeRecent(recent, MaxRecent)
	return ov
}

// keys lists the configured keys with their policy, their totals over
// the fleet and, for budgeted keys, the shared ledger's balance.
func (s *Server) keys(ctx context.Context, totals map[string]keyTotals) []Key {
	out := make([]Key, 0, len(s.o.Keys))
	for _, p := range s.o.Keys {
		k := Key{
			Name: p.Name, Tenant: tenantName(p.Tenant), Models: p.Models(), Cache: p.Cache.String(),
			Calls: totals[p.Name].Calls, SpentUSD: cost.Nanos(totals[p.Name].SpentNanos).USD(),
		}
		if p.Requests != nil {
			k.Requests = &Limit{Rate: p.Requests.Rate, PeriodSeconds: p.Requests.Period.Seconds(), Burst: p.Requests.Burst}
		}
		if p.Tokens != nil {
			k.Tokens = &Limit{Rate: p.Tokens.Rate, PeriodSeconds: p.Tokens.Period.Seconds(), Burst: p.Tokens.Burst}
		}
		if b := p.Budget; b != nil {
			kb := &Budget{LimitUSD: b.Limit.USD(), Period: string(b.Period)}
			if s.o.Ledger != nil {
				bctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
				bal, err := s.o.Ledger.Balance(bctx, p.Name, *b)
				cancel()
				if err != nil {
					kb.Error = err.Error()
				} else {
					kb.Window, kb.SpentUSD, kb.ReservedUSD = bal.Window, bal.Spent.USD(), bal.Reserved.USD()
				}
			}
			k.Budget = kb
		}
		out = append(out, k)
	}
	return out
}

// tenantName strips the namespace's "tenant:" or "key:" prefix.
func tenantName(ns string) string {
	if _, name, ok := strings.Cut(ns, ":"); ok {
		return name
	}
	return ns
}

func cacheSummary(enabled bool, lookups map[string]int64, saved int64) Cache {
	c := Cache{Enabled: enabled, Lookups: lookups, SavedUSD: cost.Nanos(saved).USD()}
	hits := lookups[string(cache.StatusExact)] + lookups[string(cache.StatusSemantic)]
	if n := hits + lookups[string(cache.StatusMiss)]; n > 0 {
		c.HitRate = float64(hits) / float64(n)
	}
	return c
}

// mergeRecent merges the replicas' recent calls, newest first, keeping
// at most n.
func mergeRecent(lists [][]Call, n int) []Call {
	var all []Call
	for _, l := range lists {
		all = append(all, l...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.After(all[j].Time) })
	if len(all) > n {
		all = all[:n]
	}
	if all == nil {
		all = []Call{}
	}
	return all
}

// Handler serves GET /api/overview and the page. Both answer only
// requests whose Host is a loopback name, an IP literal or an allowed
// host: the listener is on loopback, and checking Host stops a web page
// from reading the API through a DNS name rebound to 127.0.0.1.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s.Overview(r.Context()))
	})
	if s.o.UI != nil {
		files := http.FileServerFS(s.o.UI)
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			files.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "host not allowed on the admin listener", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return true
	}
	return slices.ContainsFunc(s.o.AllowedHosts, func(a string) bool { return strings.EqualFold(a, host) })
}

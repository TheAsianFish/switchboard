package telemetry

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Request outcomes, the values of the outcome label.
const (
	OutcomeOK              = "ok"
	OutcomeError           = "error"
	OutcomeAbandoned       = "abandoned"
	OutcomeRateLimited     = "rate_limited"
	OutcomeBudgetExhausted = "budget_exhausted"
	// OutcomeCacheHit is a request answered from the response cache.
	OutcomeCacheHit = "cache_hit"
)

// Metrics are the gateway's Prometheus series. Key labels carry the key's
// configured name, never the key, and their cardinality is bounded by the
// key table. Every method is safe on a nil *Metrics, which records nothing,
// so tests and tools need not build one.
type Metrics struct {
	reg       *prometheus.Registry
	requests  *prometheus.CounterVec
	tokens    *prometheus.CounterVec
	dollars   *prometheus.CounterVec
	unpriced  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	refusals  *prometheus.CounterVec
	degraded  *prometheus.GaugeVec
	storeErrs *prometheus.CounterVec

	cacheLookups *prometheus.CounterVec
	cacheLatency *prometheus.HistogramVec
	cacheFills   *prometheus.CounterVec
	cacheErrs    *prometheus.CounterVec
	cacheSaved   *prometheus.CounterVec
}

// NewMetrics registers the gateway's series on a fresh registry, with the
// Go runtime and process collectors when runtime is true.
func NewMetrics(runtime bool) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_requests_total",
			Help: "Requests by key, model and outcome (ok, error, abandoned, rate_limited, budget_exhausted, cache_hit).",
		}, []string{"key", "model", "outcome"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_tokens_total",
			Help: "Tokens by key, model and type (input, output, cache_read, cache_write), as reported by the provider or estimated when it reported none.",
		}, []string{"key", "model", "type"}),
		dollars: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_cost_dollars_total",
			Help: "Spend in US dollars by key and model, from the provider's usage and the model's price.",
		}, []string{"key", "model"}),
		unpriced: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_unpriced_requests_total",
			Help: "Requests to models without a price, whose spend is not in switchboard_cost_dollars_total.",
		}, []string{"key", "model"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchboard_request_duration_seconds",
			Help:    "Time from admission to the end of the reply, by model and whether it streamed.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"model", "stream"}),
		refusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_ratelimit_refusals_total",
			Help: "Requests refused by a rate limit, by key and limit (requests, tokens).",
		}, []string{"key", "limit"}),
		degraded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "switchboard_store_degraded",
			Help: "1 while a component (ratelimit, budget) runs on its local fallback because Redis is unreachable.",
		}, []string{"component"}),
		storeErrs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_store_errors_total",
			Help: "Failed calls to the shared store that were not answered by a fallback, by component and operation.",
		}, []string{"component", "op"}),
		cacheLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_cache_lookups_total",
			Help: "Response cache lookups by result (hit-exact, hit-semantic, miss, skip, error).",
		}, []string{"result"}),
		cacheLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchboard_cache_lookup_seconds",
			Help:    "Time spent in a cache layer per lookup: exact (one store read) or semantic (embedding plus vector search).",
			Buckets: []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
		}, []string{"layer"}),
		cacheFills: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_cache_fills_total",
			Help: "Replies written to the response cache, by result (ok, error).",
		}, []string{"result"}),
		cacheErrs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_cache_errors_total",
			Help: "Failed cache operations by operation (get, set, search, add, embed, index); the request still goes upstream.",
		}, []string{"op"}),
		cacheSaved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_cache_saved_dollars_total",
			Help: "What the replies served from the cache cost when they were first generated, in US dollars, by key and model.",
		}, []string{"key", "model"}),
	}
	m.reg.MustRegister(m.requests, m.tokens, m.dollars, m.unpriced, m.duration, m.refusals, m.degraded, m.storeErrs,
		m.cacheLookups, m.cacheLatency, m.cacheFills, m.cacheErrs, m.cacheSaved)
	if runtime {
		m.reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	for _, c := range []string{"ratelimit", "budget"} {
		m.degraded.WithLabelValues(c).Set(0)
	}
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// Registry exposes the registry to tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Call records one finished or refused request.
func (m *Metrics) Call(key, model, outcome string) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(key, model, outcome).Inc()
}

// Usage records the tokens and spend of one request. dollars is ignored
// when priced is false.
func (m *Metrics) Usage(key, model string, input, output, cacheRead, cacheWrite int, dollars float64, priced bool) {
	if m == nil {
		return
	}
	for typ, n := range map[string]int{"input": input, "output": output, "cache_read": cacheRead, "cache_write": cacheWrite} {
		if n > 0 {
			m.tokens.WithLabelValues(key, model, typ).Add(float64(n))
		}
	}
	if !priced {
		m.unpriced.WithLabelValues(key, model).Inc()
		return
	}
	m.dollars.WithLabelValues(key, model).Add(dollars)
}

// Duration records how long an admitted request took.
func (m *Metrics) Duration(model string, stream bool, d time.Duration) {
	if m == nil {
		return
	}
	s := "false"
	if stream {
		s = "true"
	}
	m.duration.WithLabelValues(model, s).Observe(d.Seconds())
}

// Refused records a rate-limit refusal.
func (m *Metrics) Refused(key, limit string) {
	if m == nil {
		return
	}
	m.refusals.WithLabelValues(key, limit).Inc()
}

// Degraded sets whether component runs on its local fallback.
func (m *Metrics) Degraded(component string, degraded bool) {
	if m == nil {
		return
	}
	v := 0.0
	if degraded {
		v = 1
	}
	m.degraded.WithLabelValues(component).Set(v)
}

// StoreError records a store failure that no fallback absorbed.
func (m *Metrics) StoreError(component, op string) {
	if m == nil {
		return
	}
	m.storeErrs.WithLabelValues(component, op).Inc()
}

// CacheLookup records one cache lookup's result.
func (m *Metrics) CacheLookup(result string) {
	if m == nil {
		return
	}
	m.cacheLookups.WithLabelValues(result).Inc()
}

// CacheLatency records the time one cache layer took.
func (m *Metrics) CacheLatency(layer string, d time.Duration) {
	if m == nil {
		return
	}
	m.cacheLatency.WithLabelValues(layer).Observe(d.Seconds())
}

// CacheFill records one write to the cache.
func (m *Metrics) CacheFill(result string) {
	if m == nil {
		return
	}
	m.cacheFills.WithLabelValues(result).Inc()
}

// CacheError records a failed cache operation.
func (m *Metrics) CacheError(op string) {
	if m == nil {
		return
	}
	m.cacheErrs.WithLabelValues(op).Inc()
}

// CacheSaved records the original cost of a reply served from the cache.
func (m *Metrics) CacheSaved(key, model string, dollars float64) {
	if m == nil {
		return
	}
	m.cacheSaved.WithLabelValues(key, model).Add(dollars)
}

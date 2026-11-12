package telemetry

import "github.com/prometheus/client_golang/prometheus"

// resilience holds the retry, hedge and circuit-breaker series. Provider
// labels carry configured provider names, bounded by the config.
type resilience struct {
	retries *prometheus.CounterVec
	hedges  *prometheus.CounterVec
	breaker *prometheus.GaugeVec
	ftt     *prometheus.CounterVec
}

func newResilience(reg *prometheus.Registry) resilience {
	r := resilience{
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_provider_retries_total",
			Help: "Requests sent to a provider again after a retryable failure, by provider and the failure's kind (overloaded, rate_limited, timeout, upstream).",
		}, []string{"provider", "kind"}),
		hedges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_provider_hedges_total",
			Help: "Hedging events by provider: sent (a second attempt went out), won (it answered first), lost (the first attempt answered first), refused (the tenant's hedge budget was spent).",
		}, []string{"provider", "event"}),
		breaker: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "switchboard_provider_breaker_state",
			Help: "Each provider's circuit breaker: 0 closed, 1 half-open (probing), 2 open (failing fast).",
		}, []string{"provider"}),
		ftt: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_provider_first_token_timeouts_total",
			Help: "Streamed attempts failed by the provider's first-token timeout (resilience.first_token_timeout): no first token arrived in time.",
		}, []string{"provider"}),
	}
	reg.MustRegister(r.retries, r.hedges, r.breaker, r.ftt)
	return r
}

// Retry counts a retried request.
func (m *Metrics) Retry(provider, kind string) {
	if m == nil {
		return
	}
	m.retries.WithLabelValues(provider, kind).Inc()
}

// Hedge counts a hedging event.
func (m *Metrics) Hedge(provider, event string) {
	if m == nil {
		return
	}
	m.hedges.WithLabelValues(provider, event).Inc()
}

// BreakerState sets a provider's breaker state.
func (m *Metrics) BreakerState(provider string, state int) {
	if m == nil {
		return
	}
	m.breaker.WithLabelValues(provider).Set(float64(state))
}

// FirstTokenTimeout counts an attempt failed by the first-token timeout.
func (m *Metrics) FirstTokenTimeout(provider string) {
	if m == nil {
		return
	}
	m.ftt.WithLabelValues(provider).Inc()
}

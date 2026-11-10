// Package admin is the operator's view of a gateway fleet: a JSON API and
// the React page that reads it, served on the loopback admin listener only.
//
// Each replica records its own calls in memory (Recorder) and publishes a
// snapshot to Redis every couple of seconds (Fleet). Any replica's admin
// API reads every snapshot back, so whichever replica an operator opens
// shows the whole fleet, including replicas that stopped reporting. Nothing
// here is on the request path except one mutex-guarded append per call.
package admin

import "time"

// Overview is GET /api/overview: everything the page shows, in one
// snapshot.
type Overview struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Self is the id of the replica that answered.
	Self string `json:"self"`
	// Shared is true when the fleet view comes from Redis; false when this
	// replica runs without Redis (then Replicas holds only itself).
	Shared bool `json:"shared"`
	// FleetError says why the shared view could not be read (Redis down);
	// Replicas then holds only this replica.
	FleetError string    `json:"fleet_error,omitempty"`
	Replicas   []Replica `json:"replicas"`
	Keys       []Key     `json:"keys"`
	Cache      Cache     `json:"cache"`
	Router     Router    `json:"router"`
	// Recent is the fleet's most recent calls, newest first.
	Recent []Call `json:"recent"`
	// TraceURL is the trace link template ("{trace_id}" is replaced), or
	// empty when no trace UI is configured.
	TraceURL string `json:"trace_url,omitempty"`
}

// Replica status values.
const (
	// StatusUp: reported within the last StaleAfter.
	StatusUp = "up"
	// StatusStale: last report older than StaleAfter but newer than
	// DownAfter.
	StatusStale = "stale"
	// StatusDown: no report for DownAfter; kept on the page until
	// ForgetAfter so a killed replica stays visible.
	StatusDown = "down"
)

// Replica is one gateway process as it last reported itself.
type Replica struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
	// AgeSeconds is how long ago LastSeen was, from the answering
	// replica's clock.
	AgeSeconds float64 `json:"age_seconds"`
	Status     string  `json:"status"`
	// Self marks the replica that answered.
	Self bool `json:"self"`
	// Window is the replica's recent activity (the last WindowSeconds).
	Window Window `json:"window"`
	// Totals since the replica started.
	Requests int64            `json:"requests"`
	Outcomes map[string]int64 `json:"outcomes"`
	SpentUSD float64          `json:"spent_usd"`
	// Degraded is true per component ("ratelimit", "budget") while that
	// component runs on its per-replica fallback because Redis is
	// unreachable.
	Degraded  map[string]bool  `json:"degraded"`
	Providers []ProviderHealth `json:"providers"`
}

// Window is a replica's activity over the last few seconds.
type Window struct {
	Seconds   float64 `json:"seconds"`
	Requests  int64   `json:"requests"`
	RPS       float64 `json:"rps"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	// P50MS and P99MS are the durations of admitted requests (refusals
	// are counted in Requests but not timed); 0 with none.
	P50MS float64 `json:"p50_ms"`
	P99MS float64 `json:"p99_ms"`
	// CacheHitRate is hits over lookups that consulted the cache.
	CacheHitRate float64 `json:"cache_hit_rate"`
}

// ProviderHealth is a provider's breaker and hedging state on one replica.
type ProviderHealth struct {
	Provider     string             `json:"provider"`
	Breaker      string             `json:"breaker"`
	RetryAfterMS int64              `json:"retry_after_ms,omitempty"`
	Hedging      bool               `json:"hedging"`
	HedgeDelayMS map[string]float64 `json:"hedge_delay_ms,omitempty"`
}

// Key is one configured API key: its policy and what it has used. The key
// itself is never part of the API.
type Key struct {
	Name   string `json:"name"`
	Tenant string `json:"tenant"`
	// Models is the allow list; nil allows every model.
	Models   []string `json:"models"`
	Cache    string   `json:"cache"`
	Requests *Limit   `json:"requests_limit"`
	Tokens   *Limit   `json:"tokens_limit"`
	Budget   *Budget  `json:"budget"`
	// Calls and SpentUSD are summed over the replicas that reported.
	Calls    int64   `json:"calls"`
	SpentUSD float64 `json:"spent_usd"`
}

// Limit is a rate limit: Rate per Period with Burst.
type Limit struct {
	Rate          int64   `json:"rate"`
	PeriodSeconds float64 `json:"period_seconds"`
	Burst         int64   `json:"burst"`
}

// Budget is a key's dollar allowance and its position in the current
// window, read from the shared ledger.
type Budget struct {
	LimitUSD    float64 `json:"limit_usd"`
	Period      string  `json:"period"`
	Window      string  `json:"window,omitempty"`
	SpentUSD    float64 `json:"spent_usd"`
	ReservedUSD float64 `json:"reserved_usd"`
	// Error says why the balance could not be read; the amounts are then
	// zero.
	Error string `json:"error,omitempty"`
}

// Cache is the response cache's activity summed over the fleet.
type Cache struct {
	Enabled bool `json:"enabled"`
	// Lookups counts lookups by status: "hit-exact", "hit-semantic",
	// "miss", "skip", "error".
	Lookups map[string]int64 `json:"lookups"`
	// HitRate is hits over hits plus misses (skips are requests the cache
	// was not allowed to answer).
	HitRate  float64 `json:"hit_rate"`
	SavedUSD float64 `json:"saved_usd"`
}

// Router is the auto model's configuration and decisions, summed over the
// fleet.
type Router struct {
	Enabled    bool     `json:"enabled"`
	Target     float64  `json:"target"`
	Candidates []string `json:"candidates"`
	Fallback   string   `json:"fallback,omitempty"`
	// Decisions counts auto requests by chosen model and reason.
	Decisions []Decision `json:"decisions"`
}

// Decision is one (model, reason) count of router choices.
type Decision struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// Call is one request as the gateway saw it. It never holds prompt or
// reply text.
type Call struct {
	Time      time.Time `json:"time"`
	Replica   string    `json:"replica"`
	RequestID string    `json:"request_id"`
	TraceID   string    `json:"trace_id,omitempty"`
	Key       string    `json:"key"`
	// RequestedModel is what the client asked for ("auto" for the
	// router); Model is what answered.
	RequestedModel string `json:"requested_model"`
	Model          string `json:"model"`
	Provider       string `json:"provider,omitempty"`
	Stream         bool   `json:"stream"`
	// Outcome is one of ok, error, abandoned, rate_limited,
	// budget_exhausted, cache_hit.
	Outcome   string `json:"outcome"`
	ErrorKind string `json:"error_kind,omitempty"`
	// Cache is the lookup status ("miss", "hit-exact", ...), empty when
	// the request never reached the cache.
	Cache string `json:"cache,omitempty"`
	// RouterReason and RouterProbs are set for auto requests: why the
	// router chose Model and each candidate's predicted P(correct).
	RouterReason string             `json:"router_reason,omitempty"`
	RouterProbs  map[string]float64 `json:"router_probs,omitempty"`
	DurationMS   float64            `json:"duration_ms"`
	TTFTMS       float64            `json:"ttft_ms,omitempty"`
	InputTokens  int                `json:"input_tokens"`
	OutputTokens int                `json:"output_tokens"`
	CostUSD      float64            `json:"cost_usd"`
	Attempts     int                `json:"attempts,omitempty"`
	Hedged       bool               `json:"hedged,omitempty"`
}

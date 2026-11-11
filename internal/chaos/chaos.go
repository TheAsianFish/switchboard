// Package chaos analyses the Q6 chaos run (`make chaos`, chaos/run.sh):
// three replicas behind nginx under load while a replica and Redis are
// killed and restarted. It reads the raw files the run leaves under
// eval/results/q6-chaos/ (the bench results and per-request logs of three
// load streams, the fake provider's ledger and the times of the docker
// actions) and writes summary.json: per phase the main stream's error
// rate, latency and cache hit rate, the rate-limited key's
// over-admissions and the budgeted key's overshoot, and per event the
// seconds until the fleet was back in steady state.
package chaos

import (
	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cost"
)

// File names in the results directory.
const (
	MetaFile    = "meta.json"
	EventsFile  = "events.json"
	LedgerFile  = "ledger.json.gz"
	SummaryFile = "summary.json"
)

// Stream names: each is a `switchboard bench` run with its result in
// <name>.json and its request log in <name>.jsonl.gz.
const (
	StreamMain    = "main"
	StreamLimited = "limited"
	StreamBudget  = "budget"
)

// Event actions.
const (
	ActionKill  = "kill"
	ActionStart = "start"
)

// RedisTarget is the compose service name of Redis; any other target is
// a gateway replica.
const RedisTarget = "redis"

// Meta describes the run; chaos/run.sh writes it.
type Meta struct {
	// Commit is the short commit under test, "-dirty" if the tree had
	// uncommitted changes.
	Commit string `json:"commit"`
	// Date is when the run started, RFC 3339 in UTC.
	Date    string `json:"date"`
	Command string `json:"command"`
	// Replicas is the number of gateway replicas in the fleet.
	Replicas int `json:"replicas"`
	// DurationSeconds is the main stream's measured window.
	DurationSeconds float64 `json:"duration_seconds"`
	// TimeScale multiplies the PLAN schedule's offsets and the duration;
	// 1 for the full run.
	TimeScale float64 `json:"time_scale"`
	// Limit is the rate-limited key's request limit.
	Limit struct {
		Model         string  `json:"model"`
		Rate          int64   `json:"rate"`
		PeriodSeconds float64 `json:"period_seconds"`
	} `json:"limit"`
	// Budget is the budgeted key's allowance and its model's price.
	Budget struct {
		Model string `json:"model"`
		// USD and GraceUSD are decimal dollars; GraceUSD is each
		// replica's allowance per key while Redis is unreachable.
		USD      string `json:"usd"`
		GraceUSD string `json:"grace_usd"`
		// Prices in dollars per million tokens.
		InputPrice  string `json:"input_price"`
		OutputPrice string `json:"output_price"`
	} `json:"budget"`
	FakeProvider struct {
		Latency       string `json:"latency"`
		Tokens        int    `json:"tokens"`
		TokenInterval string `json:"token_interval"`
	} `json:"fake_provider"`
}

// Event is one docker action of the kill schedule, as chaos/run.sh
// recorded it.
type Event struct {
	// Name is for people, e.g. "kill switchboard-2".
	Name   string `json:"name"`
	Action string `json:"action"` // kill or start
	Target string `json:"target"` // compose service: redis or switchboard-N
	// Phase names the phase the event starts.
	Phase string `json:"phase"`
	// PlannedSeconds is the scheduled offset from the main stream's
	// start; IssuedUnix and DoneUnix are when the docker command was run
	// and when it returned, Unix seconds. The phase starts at IssuedUnix:
	// `docker kill` takes effect within milliseconds but returns a few
	// hundred later, and a start's delay until the replica serves again
	// is part of what the recovery time measures.
	PlannedSeconds float64 `json:"planned_seconds"`
	IssuedUnix     float64 `json:"issued_unix"`
	DoneUnix       float64 `json:"done_unix"`
}

// Rule is the steady-state rule, fixed in PLAN.md before the full run:
// after an event, the fleet is back in steady state at the start of the
// first whole BucketSeconds bucket from which Consecutive buckets in a row
// each have an error rate at most max(MinErrorRate, ErrorFactor × the
// steady phase's) and a p99 latency at most P99Factor × the steady
// phase's. The buckets must lie before the next event (or the run's end);
// otherwise the event is "not recovered".
type Rule struct {
	BucketSeconds float64 `json:"bucket_seconds"`
	Consecutive   int     `json:"consecutive"`
	MinErrorRate  float64 `json:"min_error_rate"`
	ErrorFactor   float64 `json:"error_factor"`
	P99Factor     float64 `json:"p99_factor"`
}

// DefaultRule is the rule PLAN.md fixes for Q6.
var DefaultRule = Rule{BucketSeconds: 1, Consecutive: 5, MinErrorRate: 0.01, ErrorFactor: 2, P99Factor: 2}

// Summary is summary.json.
type Summary struct {
	Meta     Meta           `json:"meta"`
	Hardware bench.Hardware `json:"hardware"`
	Rule     Rule           `json:"rule"`
	// Reference is the steady phase's numbers the rule compares with,
	// and the limits it derives from them.
	Reference Reference `json:"reference"`
	// Streams are the three bench runs' offered rates and totals.
	Streams []StreamInfo   `json:"streams"`
	Events  []EventResult  `json:"events"`
	Phases  []PhaseResult  `json:"phases"`
	Buckets []BucketResult `json:"buckets"`
}

// Reference is the baseline for the steady-state rule.
type Reference struct {
	ErrorRate  float64 `json:"error_rate"`
	P99MS      float64 `json:"p99_ms"`
	ErrorLimit float64 `json:"error_limit"`
	P99LimitMS float64 `json:"p99_limit_ms"`
}

// StreamInfo describes one load stream.
type StreamInfo struct {
	Name     string  `json:"name"`
	Model    string  `json:"model"`
	Rate     float64 `json:"rate"`
	Stream   bool    `json:"stream"`
	Repeat   float64 `json:"repeat,omitempty"`
	Pool     int     `json:"repeat_pool,omitempty"`
	Requests int     `json:"requests"`
	// StartOffsetSeconds is the stream's start relative to the main
	// stream's.
	StartOffsetSeconds float64 `json:"start_offset_seconds"`
	// MaxSendDelayMS is the generator's worst lag behind its schedule.
	MaxSendDelayMS float64 `json:"max_send_delay_ms"`
}

// EventResult is an event with the time the fleet took to recover from
// it.
type EventResult struct {
	Event
	// AtSeconds and DoneSeconds are IssuedUnix and DoneUnix relative to
	// the main stream's start.
	AtSeconds   float64 `json:"at_seconds"`
	DoneSeconds float64 `json:"done_seconds"`
	// Recovered reports whether the rule was met before the next event;
	// RecoverySeconds is from AtSeconds to the start of the first bucket
	// of the run of good buckets.
	Recovered       bool     `json:"recovered"`
	RecoverySeconds *float64 `json:"recovery_seconds,omitempty"`
	// WindowSeconds is how long the rule had: until the next event or
	// the end of the run.
	WindowSeconds float64 `json:"window_seconds"`
	// ErrorsBeforeRecovery counts the main stream's failed requests that
	// ended between the event and recovery (or the window's end).
	ErrorsBeforeRecovery int `json:"errors_before_recovery"`
	// WorstBucket is the bucket after the event with the most errors.
	WorstBucket *BucketResult `json:"worst_bucket,omitempty"`
}

// PhaseResult is one stretch of the run between two events.
type PhaseResult struct {
	Name        string       `json:"name"`
	FromSeconds float64      `json:"from_seconds"`
	ToSeconds   float64      `json:"to_seconds"`
	ReplicasUp  int          `json:"replicas_up"`
	RedisUp     bool         `json:"redis_up"`
	Main        MainPhase    `json:"main"`
	Limited     LimitedPhase `json:"limited"`
	Budget      BudgetPhase  `json:"budget"`
}

// MainPhase is the main stream in one phase. A request belongs to the
// phase in which its outcome was observed: its scheduled time plus its
// latency (the scheduled time for a dropped request). A request in flight
// when a replica dies fails after the kill, so it counts against the
// phase the kill starts, not the one before.
type MainPhase struct {
	Requests int `json:"requests"`
	OK       int `json:"ok"`
	Errors   int `json:"errors"`
	// ErrorRate is Errors / Requests.
	ErrorRate float64 `json:"error_rate"`
	// ErrorsByKind counts the failed requests by outcome (http_502,
	// transport_error, malformed_stream, ...).
	ErrorsByKind map[string]int `json:"errors_by_kind"`
	// Latency and TTFT are over the successful requests, from their
	// scheduled time.
	Latency bench.Stats `json:"latency"`
	TTFT    bench.Stats `json:"ttft"`
	// CacheStatuses counts the successful requests by the gateway's
	// cache status header.
	CacheStatuses map[string]int `json:"cache_statuses"`
	// CacheHits / OK is the hit rate; RepeatHits / RepeatOK is the hit
	// rate among requests that reused a pool prompt, the ones that could
	// hit.
	// Slow counts the successful requests slower than the rule's p99
	// limit (P99Factor × the steady phase's p99): the tail a percentile
	// can hide.
	Slow          int     `json:"slow"`
	CacheHits     int     `json:"cache_hits"`
	CacheHitRate  float64 `json:"cache_hit_rate"`
	RepeatOK      int     `json:"repeat_ok"`
	RepeatHits    int     `json:"repeat_hits"`
	RepeatHitRate float64 `json:"repeat_hit_rate"`
}

// LimitedPhase is the rate-limited key in one phase.
type LimitedPhase struct {
	// Offered, Refused (429) and OtherErrors are the client's view.
	Offered     int `json:"offered"`
	Refused     int `json:"refused"`
	OtherErrors int `json:"other_errors"`
	// Admitted counts the key's requests in the provider's ledger that
	// arrived in the phase.
	Admitted int `json:"admitted"`
	// Windows is the number of limiter windows (aligned to the Unix
	// clock, as the fixed-window limiter counts them) whose midpoint is
	// in the phase, so the window a fault lands in belongs to the phase
	// that holds most of it; MostInWindow the most admissions in one.
	Windows      int `json:"windows"`
	MostInWindow int `json:"most_in_window"`
	// OverAdmissions sums, over those windows, the admissions beyond the
	// limit's rate.
	OverAdmissions int64 `json:"over_admissions"`
	// LocalBound is the most the degrade policy should let through per
	// window while Redis is down: replicas up × (rate ÷ replicas), rounded
	// down; with Redis up it is the rate.
	LocalBound int64 `json:"local_bound"`
}

// BudgetPhase is the budgeted key in one phase.
type BudgetPhase struct {
	Offered     int `json:"offered"`
	OK          int `json:"ok"`
	Refused     int `json:"refused"`
	OtherErrors int `json:"other_errors"`
	// Admitted counts the key's ledger entries that arrived in the
	// phase, and Spent prices their usage.
	Admitted int        `json:"admitted"`
	Spent    cost.Nanos `json:"spent_nanodollars"`
	// Cumulative is the key's spend from the run's start to the phase's
	// end; Overshoot is how far that is past the budget, or 0.
	Cumulative cost.Nanos `json:"cumulative_nanodollars"`
	Overshoot  cost.Nanos `json:"overshoot_nanodollars"`
	// GraceBound is replicas up × grace while Redis is down, the degrade
	// policy's bound on what the phase may spend past the budget; 0 with
	// Redis up.
	GraceBound cost.Nanos `json:"grace_bound_nanodollars"`
}

// BucketResult is one second of the main stream, by the time each
// request's outcome was observed.
type BucketResult struct {
	StartSeconds float64 `json:"start_seconds"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	OK           int     `json:"ok"`
	// P99MS is the successful requests' p99 latency, 0 if none.
	P99MS     float64 `json:"p99_ms"`
	CacheHits int     `json:"cache_hits"`
	// Good reports whether the bucket meets the steady-state rule.
	Good bool `json:"good"`
}

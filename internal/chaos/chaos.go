// Package chaos analyses the Q6 chaos run (`make chaos`, chaos/run.sh):
// three replicas behind nginx under load while a replica and Redis are
// killed and restarted. The same analysis reads M8's flap run (`make
// chaos-flap`), in which Redis is cut off from the network and reconnected
// eight times with its state kept, and reconciles the fake provider's
// ledger with the budget Redis recorded. It reads the raw files the run leaves under
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
	// SnapshotsFile holds the budgeted key's balance as Redis held it,
	// read just before each docker action and once after the run; only
	// the flap run writes it.
	SnapshotsFile = "snapshots.json"
)

// Stream names: each is a `switchboard bench` run with its result in
// <name>.json and its request log in <name>.jsonl.gz.
const (
	StreamMain    = "main"
	StreamLimited = "limited"
	StreamBudget  = "budget"
)

// Event actions. Kill and start act on a container's process;
// disconnect and connect take it off the compose network and put it back
// (`docker network disconnect` / `connect`), so a store keeps its state.
const (
	ActionKill       = "kill"
	ActionStart      = "start"
	ActionDisconnect = "disconnect"
	ActionConnect    = "connect"
)

// Schedules chaos/run.sh knows.
const (
	ScheduleQ6   = "q6"
	ScheduleFlap = "flap"
)

// RedisTarget is the compose service name of Redis; any other target is
// a gateway replica.
const RedisTarget = "redis"

// Meta describes the run; chaos/run.sh writes it.
type Meta struct {
	// Commit is the short commit under test, "-dirty" if the tree had
	// uncommitted changes.
	Commit string `json:"commit"`
	// GatewayCommit is the commit the replicas were built from: Commit,
	// unless the run built them from another (REPLICA_REF, M13's before
	// arm). Empty for runs from before M13.
	GatewayCommit string `json:"gateway_commit,omitempty"`
	// RetryInterval is every replica's ratelimit.retry_interval, a Go
	// duration (M13's switch); empty for runs from before M13, which ran
	// the default, 1 s.
	RetryInterval string `json:"retry_interval,omitempty"`
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
	// Schedule is the kill schedule: "q6" (or empty, from runs before
	// M8) or "flap".
	Schedule string `json:"schedule,omitempty"`
	// RedisPersist is the append-only file's mode (M9): "none",
	// "everysec" or "always"; empty for runs from before M9, which had
	// none.
	RedisPersist string `json:"redis_persist,omitempty"`
	// Redis is how Redis ran: its append-only settings, the filesystem
	// type of its /data directory and the file's size at the end.
	Redis *RedisMeta `json:"redis,omitempty"`
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
		// GraceRefill is how long a replica's grace allowance takes to
		// refill from empty, as a Go duration; empty for builds in which
		// grace is reset after every outage instead.
		GraceRefill string `json:"grace_refill,omitempty"`
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

// RedisMeta is how Redis ran in a chaos run.
type RedisMeta struct {
	AppendOnly  string `json:"appendonly"`
	AppendFsync string `json:"appendfsync"`
	DataFS      string `json:"data_fs"`
	AOFBytes    int64  `json:"aof_bytes_at_end"`
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
	// Flap summarises the budget across the Redis outages; nil for a run
	// without a disconnect.
	Flap *FlapResult `json:"flap,omitempty"`
	// Store is what the replicas' and Redis's logs say about each Redis
	// outage and restart; nil for a run that kept no logs.
	Store *StoreLog `json:"store,omitempty"`
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
	// Reconciliation at the phase's end, when the run took a snapshot of
	// Redis then (the flap run does, just before each docker action).
	Reconcile *Reconcile `json:"reconcile,omitempty"`
}

// Snapshot is the budgeted key's balance as Redis held it at one moment,
// read with redis-cli inside the Redis container, so it works while the
// container is off the network.
type Snapshot struct {
	// Label says which action the snapshot preceded, or "end".
	Label string  `json:"label"`
	Unix  float64 `json:"unix"`
	// Spent and Reserved are the balance hash's fields in nanodollars.
	Spent    cost.Nanos `json:"spent_nanodollars"`
	Reserved cost.Nanos `json:"reserved_nanodollars"`
}

// Reconcile sets what the provider was asked for against what Redis
// recorded, at one snapshot.
type Reconcile struct {
	AtSeconds float64 `json:"at_seconds"`
	// Ledger prices the budgeted key's provider requests that arrived
	// before the snapshot.
	Ledger cost.Nanos `json:"ledger_nanodollars"`
	// RedisSpent and RedisReserved are the snapshot's balance.
	RedisSpent    cost.Nanos `json:"redis_spent_nanodollars"`
	RedisReserved cost.Nanos `json:"redis_reserved_nanodollars"`
	// Unreconciled is Ledger − RedisSpent: spend Redis has not seen
	// (pending on a replica, in flight, or lost with a replica). Negative
	// means Redis counted more than the provider served: a double charge.
	Unreconciled cost.Nanos `json:"unreconciled_nanodollars"`
}

// FlapResult is the budget across a run's Redis outages. An outage is a
// run of consecutive phases with Redis down.
type FlapResult struct {
	Outages []Outage `json:"outages"`
	// FirstOutageSeconds and LastOutageEndSeconds bound the stretch T over
	// which the outages happened.
	FirstOutageSeconds   float64 `json:"first_outage_seconds"`
	LastOutageEndSeconds float64 `json:"last_outage_end_seconds"`
	TSeconds             float64 `json:"t_seconds"`
	// Processes counts the replica processes that could spend grace in T:
	// those up when the first outage began plus every replica start
	// before the last outage ended.
	Processes int `json:"processes"`
	// Overshoot is the key's spend past its budget at the end of the run;
	// OutageSpend is what the provider served it during the outages.
	Overshoot   cost.Nanos `json:"overshoot_nanodollars"`
	OutageSpend cost.Nanos `json:"outage_spend_nanodollars"`
	// Slope and Intercept fit the overshoot after each outage against
	// the outage's number by least squares; R2 is the fit's R².
	Slope     cost.Nanos `json:"slope_nanodollars"`
	Intercept cost.Nanos `json:"intercept_nanodollars"`
	R2        float64    `json:"r2"`
	// PerOutageBound is the bound when grace is reset after every outage:
	// the sum over outages of the replicas up at its start × grace, plus
	// a grace for each replica started during an outage.
	PerOutageBound cost.Nanos `json:"per_outage_bound_nanodollars"`
	// RefillBound is processes × grace × (1 + T / refill) when the run's
	// build refills grace with time; 0 otherwise.
	RefillBound cost.Nanos `json:"refill_bound_nanodollars,omitempty"`
	// Kill is the replica killed while Redis was down, if one was.
	Kill *FlapKill `json:"kill,omitempty"`
	// MaxControlGap is the largest |Unreconciled| at the end of a phase
	// with Redis up that starts at or after the first outage and ends
	// before Kill (the last replica killed during an outage, if any): what
	// the reconciliation reads when nothing is lost. The phase before the
	// first outage is left out; its gap is in the phase table.
	MaxControlGap cost.Nanos `json:"max_control_gap_nanodollars"`
	// MinGap is the most negative Unreconciled at any snapshot after
	// Redis came back (a double charge shows up as a negative gap).
	MinGap cost.Nanos `json:"min_gap_nanodollars"`
}

// Outage is one stretch with Redis unreachable.
type Outage struct {
	N           int     `json:"n"`
	FromSeconds float64 `json:"from_seconds"`
	ToSeconds   float64 `json:"to_seconds"`
	// ReplicasUp is the count when the outage began; ReplicaKilled is
	// true if a replica was killed during it.
	ReplicasUp    int  `json:"replicas_up"`
	ReplicaKilled bool `json:"replica_killed,omitempty"`
	// Spent is what the provider served the budgeted key during the
	// outage; Requests is how many of its requests that was.
	Spent    cost.Nanos `json:"spent_nanodollars"`
	Requests int        `json:"requests"`
	// OvershootAfter is the key's spend past the budget once the outage
	// ended.
	OvershootAfter cost.Nanos `json:"overshoot_after_nanodollars"`
	// SecondsToExhaust is from the outage's start to its last served
	// request, a rough time for the fleet to use up what it could admit.
	SecondsToExhaust float64 `json:"seconds_to_exhaust"`
}

// FlapKill is the replica killed while Redis was down, and what the
// reconciliation says it took with it.
type FlapKill struct {
	Target    string  `json:"target"`
	AtSeconds float64 `json:"at_seconds"`
	// IntoOutageSeconds is how long Redis had been down at the kill.
	IntoOutageSeconds float64 `json:"into_outage_seconds"`
	// Lost is Unreconciled at the run's end: spend the provider served
	// that Redis never recorded. Bound is one replica's grace allowance.
	Lost  cost.Nanos `json:"lost_nanodollars"`
	Bound cost.Nanos `json:"bound_nanodollars"`
	// EstimatedPending is the outage's spend before the kill divided by
	// the replicas up: the killed replica's share if the load balancer
	// spread the key's requests evenly. The ledger does not say which
	// replica sent a request.
	EstimatedPending cost.Nanos `json:"estimated_pending_nanodollars"`
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

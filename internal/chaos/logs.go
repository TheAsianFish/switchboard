package chaos

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// LogsDir is the results subdirectory with each service's log, written by
// chaos/run.sh as <service>.log.gz: `docker compose logs --timestamps`,
// so every line starts with docker's receive time in RFC 3339.
const LogsDir = "logs"

// LogLine is one line of a service's log.
type LogLine struct {
	// Unix is docker's timestamp of the line, Unix seconds.
	Unix float64
	// Text is the line without the timestamp.
	Text string
}

// Store components that log when they give up on Redis and when they find
// it again.
const (
	ComponentBudget    = "budget"
	ComponentRateLimit = "ratelimit"
	ComponentCache     = "cache"
)

// Messages the gateway logs at warn level on each transition.
var transitionMessages = map[string]struct {
	component string
	degraded  bool
}{
	"budget store unreachable, using per-replica grace":      {ComponentBudget, true},
	"budget store recovered":                                 {ComponentBudget, false},
	"rate limit store unavailable, using per-replica limits": {ComponentRateLimit, true},
	"rate limit store recovered, using shared limits":        {ComponentRateLimit, false},
	"cache store failed; bypassing the cache":                {ComponentCache, true},
	"cache store reachable again":                            {ComponentCache, false},
}

// storeErrorMessage reports whether a log message is about a failed
// Redis call (as opposed to, say, a failed provider call).
func storeErrorMessage(msg string) bool {
	if tm, ok := transitionMessages[msg]; ok {
		return tm.degraded
	}
	switch msg {
	case "budget pending charges not flushed", "budget settlement failed",
		"rate limit refund failed", "token limit refund failed", "token limit reconciliation failed",
		"fleet report not published":
		return true
	}
	return false
}

// Error classes of a failed store call, from the error text.
const (
	ErrClassLoading     = "loading"      // Redis answered LOADING
	ErrClassDNS         = "dns"          // the name did not resolve
	ErrClassDialTimeout = "dial_timeout" // no answer to the connection attempt
	ErrClassTimeout     = "timeout"      // a call on a connection timed out
	ErrClassRefused     = "refused"      // nothing listened at the address
	ErrClassClosed      = "closed"       // the connection was cut
	ErrClassOther       = "other"
)

// ClassifyStoreError names the class of a store error's text.
func ClassifyStoreError(text string) string {
	switch {
	case strings.Contains(text, "LOADING"):
		return ErrClassLoading
	case strings.Contains(text, "no such host"), strings.Contains(text, "server misbehaving"),
		strings.Contains(text, "lookup "):
		return ErrClassDNS
	case strings.Contains(text, "dial tcp") && (strings.Contains(text, "i/o timeout") || strings.Contains(text, "timed out")):
		return ErrClassDialTimeout
	case strings.Contains(text, "connection refused"):
		return ErrClassRefused
	case strings.Contains(text, "i/o timeout"), strings.Contains(text, "deadline exceeded"), strings.Contains(text, "timeout"):
		return ErrClassTimeout
	case strings.Contains(text, "EOF"), strings.Contains(text, "connection reset"), strings.Contains(text, "broken pipe"),
		strings.Contains(text, "use of closed"), strings.Contains(text, "client is closed"):
		return ErrClassClosed
	}
	return ErrClassOther
}

// StoreLog is what the kept logs say about the store during the run.
type StoreLog struct {
	// Transitions are every replica's degrade and recovery lines, in time
	// order.
	Transitions []Transition `json:"transitions"`
	// Outages are, per Redis kill or disconnect, the store errors the
	// replicas logged until Redis was back, by class.
	Outages []OutageErrors `json:"outages"`
	// Restarts are, per Redis start, how long Redis took to load and how
	// long the replicas stayed degraded.
	Restarts []RedisRestart `json:"restarts"`
}

// Transition is one replica component giving up on Redis or finding it
// again.
type Transition struct {
	Replica   string  `json:"replica"`
	Component string  `json:"component"`
	AtSeconds float64 `json:"at_seconds"`
	Degraded  bool    `json:"degraded"`
	// Error and Class are the failed call's error, for a degrade.
	Error string `json:"error,omitempty"`
	Class string `json:"class,omitempty"`
}

// OutageErrors counts the store errors logged from one Redis kill or
// disconnect until the next Redis event (or the run's end).
type OutageErrors struct {
	Event     string  `json:"event"`
	AtSeconds float64 `json:"at_seconds"`
	// ToSeconds is the next Redis event, or the run's end.
	ToSeconds float64           `json:"to_seconds"`
	Classes   []ErrorClassCount `json:"classes"`
}

// ErrorClassCount is one error class's lines in an outage.
type ErrorClassCount struct {
	Class string `json:"class"`
	// Lines counts the log lines (every replica, every component), of
	// which PoolLines are go-redis's connection pool reporting a failed
	// dial rather than the gateway reporting a failed call.
	Lines     int `json:"lines"`
	PoolLines int `json:"pool_lines"`
	// FirstSeconds and LastSeconds are the first and last line's time
	// after the event.
	FirstSeconds float64 `json:"first_seconds"`
	LastSeconds  float64 `json:"last_seconds"`
	// Example is one line's error text.
	Example string `json:"example"`
}

// RedisRestart is one start of the Redis container.
type RedisRestart struct {
	AtSeconds float64 `json:"at_seconds"`
	// ReadySeconds is from the start to Redis's "Ready to accept
	// connections" line; LoadedSeconds is the load time Redis reported
	// for its append-only file, nil without one.
	ReadySeconds  *float64 `json:"ready_seconds,omitempty"`
	LoadedSeconds *float64 `json:"aof_loaded_seconds,omitempty"`
	// Errors are the store errors logged from the start until the next
	// Redis event (or the run's end), by class; LoadingErrors is the
	// LOADING class's count.
	Errors        []ErrorClassCount `json:"errors"`
	LoadingErrors int               `json:"loading_errors"`
	// Recoveries are, per replica component degraded at the start, the
	// seconds from the start to its recovery line (nil if none came
	// before the next event or the run's end).
	Recoveries []Recovery `json:"recoveries"`
	// DegradedSeconds is the longest of Recoveries: from the start until
	// every live replica used Redis again for limits, budgets and the
	// cache. Nil if one never did.
	DegradedSeconds *float64 `json:"degraded_seconds,omitempty"`
}

// Recovery is one replica component's way back after a Redis start.
type Recovery struct {
	Replica   string   `json:"replica"`
	Component string   `json:"component"`
	Seconds   *float64 `json:"seconds,omitempty"`
}

// ParseLog reads a `docker compose logs --timestamps` stream.
func ParseLog(sc *bufio.Scanner) ([]LogLine, error) {
	var out []LogLine
	for sc.Scan() {
		line := sc.Text()
		ts, text, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue
		}
		out = append(out, LogLine{Unix: float64(t.UnixNano()) / 1e9, Text: text})
	}
	return out, sc.Err()
}

// loadLogs reads every <service>.log.gz under dir/logs; nil if the run
// kept none.
func loadLogs(dir string) (map[string][]LogLine, error) {
	paths, err := filepath.Glob(filepath.Join(dir, LogsDir, "*.log.gz"))
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	logs := map[string][]LogLine{}
	for _, p := range paths {
		r, closeFn, err := open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		lines, err := ParseLog(sc)
		closeFn()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		logs[strings.TrimSuffix(filepath.Base(p), ".log.gz")] = lines
	}
	return logs, nil
}

// replicaLine is the part of a gateway's JSON log line the analysis reads.
type replicaLine struct {
	Msg   string `json:"msg"`
	Error string `json:"error"`
}

var (
	aofLoaded = regexp.MustCompile(`DB loaded from append only file: ([0-9.]+) seconds`)
	ready     = regexp.MustCompile(`Ready to accept connections`)
)

// storeLog builds the StoreLog from the services' logs; nil without logs.
func storeLog(s *Summary, logs map[string][]LogLine, start time.Time, end float64) *StoreLog {
	if len(logs) == 0 {
		return nil
	}
	sl := &StoreLog{}
	var errs []storeErr
	for replica, lines := range logs {
		if replica == RedisTarget {
			continue
		}
		for _, l := range lines {
			at := rel(start, l.Unix)
			if strings.HasPrefix(l.Text, goRedisPrefix) {
				// go-redis's own logger, plain text: its connection pool
				// giving up on a dial.
				errs = append(errs, storeErr{at: at, class: ClassifyStoreError(l.Text), text: l.Text, pool: true})
				continue
			}
			if !strings.HasPrefix(l.Text, "{") {
				continue
			}
			var rl replicaLine
			if json.Unmarshal([]byte(l.Text), &rl) != nil {
				continue
			}
			if rl.Error != "" && storeErrorMessage(rl.Msg) {
				errs = append(errs, storeErr{at: at, class: ClassifyStoreError(rl.Error), text: rl.Error})
			}
			tm, ok := transitionMessages[rl.Msg]
			if !ok {
				continue
			}
			tr := Transition{Replica: replica, Component: tm.component, AtSeconds: round3(at), Degraded: tm.degraded}
			if tm.degraded {
				tr.Error, tr.Class = rl.Error, ClassifyStoreError(rl.Error)
			}
			sl.Transitions = append(sl.Transitions, tr)
		}
	}
	slices.SortStableFunc(sl.Transitions, func(a, b Transition) int {
		if c := cmpFloat(a.AtSeconds, b.AtSeconds); c != 0 {
			return c
		}
		if c := strings.Compare(a.Replica, b.Replica); c != 0 {
			return c
		}
		return strings.Compare(a.Component, b.Component)
	})
	slices.SortStableFunc(errs, func(a, b storeErr) int { return cmpFloat(a.at, b.at) })

	// Redis events in order, each until the next Redis event.
	var redisEvents []EventResult
	for _, e := range s.Events {
		if e.Target == RedisTarget {
			redisEvents = append(redisEvents, e)
		}
	}
	until := func(i int) float64 {
		if i+1 < len(redisEvents) {
			return redisEvents[i+1].AtSeconds
		}
		return end
	}
	for i, e := range redisEvents {
		to := until(i)
		switch e.Action {
		case ActionKill, ActionDisconnect:
			oe := OutageErrors{Event: e.Name, AtSeconds: e.AtSeconds, ToSeconds: to, Classes: classCounts(errs, e.AtSeconds, to)}
			sl.Outages = append(sl.Outages, oe)
		case ActionStart, ActionConnect:
			rr := restart(s, sl.Transitions, logs[RedisTarget], start, e, to)
			rr.Errors = classCounts(errs, e.AtSeconds, to)
			for _, c := range rr.Errors {
				if c.Class == ErrClassLoading {
					rr.LoadingErrors = c.Lines
				}
			}
			sl.Restarts = append(sl.Restarts, rr)
		}
	}
	return sl
}

// storeErr is one replica log line that carries a store error.
type storeErr struct {
	at    float64
	class string
	text  string
	// pool marks a line from go-redis's connection pool rather than the
	// gateway.
	pool bool
}

// goRedisPrefix starts the lines go-redis's own logger writes.
const goRedisPrefix = "redis: "

// classCounts groups the store errors logged in [from, to) by class, in
// the order each class first appeared, with times relative to from.
func classCounts(errs []storeErr, from, to float64) []ErrorClassCount {
	byClass := map[string]*ErrorClassCount{}
	var order []string
	for _, x := range errs {
		if x.at < from || x.at >= to {
			continue
		}
		c := byClass[x.class]
		if c == nil {
			c = &ErrorClassCount{Class: x.class, FirstSeconds: round3(x.at - from), Example: x.text}
			byClass[x.class] = c
			order = append(order, x.class)
		}
		c.Lines++
		if x.pool {
			c.PoolLines++
		}
		c.LastSeconds = round3(x.at - from)
	}
	out := make([]ErrorClassCount, 0, len(order))
	for _, cl := range order {
		out = append(out, *byClass[cl])
	}
	return out
}

// restart reads one Redis start (or reconnect): Redis's own load and
// ready lines, and when each replica component that was degraded at the
// start logged its recovery.
func restart(s *Summary, trs []Transition, redis []LogLine, start time.Time, e EventResult, to float64) RedisRestart {
	rr := RedisRestart{AtSeconds: e.AtSeconds}
	for _, l := range redis {
		at := rel(start, l.Unix)
		if at < e.AtSeconds || at >= to {
			continue
		}
		if m := aofLoaded.FindStringSubmatch(l.Text); m != nil && rr.LoadedSeconds == nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				rr.LoadedSeconds = &v
			}
		}
		if ready.MatchString(l.Text) && rr.ReadySeconds == nil {
			v := round3(at - e.AtSeconds)
			rr.ReadySeconds = &v
		}
	}
	// Each replica component's state just before the event.
	type rc struct{ replica, component string }
	degraded := map[rc]bool{}
	for _, t := range trs {
		if t.AtSeconds >= e.AtSeconds {
			break
		}
		degraded[rc{t.Replica, t.Component}] = t.Degraded
	}
	// Replicas down at the event do not count: they are not serving.
	down := map[string]bool{}
	for _, ev := range s.Events {
		if ev.Target == RedisTarget || ev.AtSeconds > e.AtSeconds {
			continue
		}
		down[ev.Target] = ev.Action == ActionKill
	}
	var keys []rc
	for k, d := range degraded {
		if d && !down[k.replica] {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b rc) int {
		if c := strings.Compare(a.replica, b.replica); c != 0 {
			return c
		}
		return strings.Compare(a.component, b.component)
	})
	all := len(keys) > 0
	var longest float64
	for _, k := range keys {
		r := Recovery{Replica: k.replica, Component: k.component}
		for _, t := range trs {
			if t.Replica == k.replica && t.Component == k.component && !t.Degraded && t.AtSeconds >= e.AtSeconds && t.AtSeconds < to {
				v := round3(t.AtSeconds - e.AtSeconds)
				r.Seconds = &v
				break
			}
		}
		if r.Seconds == nil {
			all = false
		} else {
			longest = max(longest, *r.Seconds)
		}
		rr.Recoveries = append(rr.Recoveries, r)
	}
	if all {
		rr.DegradedSeconds = &longest
	}
	return rr
}

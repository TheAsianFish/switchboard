package chaos

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
)

func TestClassifyStoreError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text, want string
	}{
		{"LOADING Redis is loading the dataset in memory", ErrClassLoading},
		{"budget reserve: LOADING Redis is loading the dataset in memory", ErrClassLoading},
		{"cache: get: dial tcp: lookup redis on 127.0.0.11:53: server misbehaving", ErrClassDNS},
		{"dial tcp: lookup redis on 127.0.0.11:53: no such host", ErrClassDNS},
		{"dial tcp 172.18.0.2:6379: i/o timeout", ErrClassDialTimeout},
		{"dial tcp 172.18.0.2:6379: connect: connection refused", ErrClassRefused},
		{"budget settle budget: context deadline exceeded", ErrClassTimeout},
		{"read tcp 172.18.0.5:41234->172.18.0.2:6379: i/o timeout", ErrClassTimeout},
		{"EOF", ErrClassClosed},
		{"read tcp 172.18.0.5:41234->172.18.0.2:6379: read: connection reset by peer", ErrClassClosed},
		{"redis: client is closed", ErrClassClosed},
		{"ERR unknown command", ErrClassOther},
	}
	for _, c := range cases {
		if got := ClassifyStoreError(c.text); got != c.want {
			t.Errorf("ClassifyStoreError(%q) = %s, want %s", c.text, got, c.want)
		}
	}
}

func TestParseLog(t *testing.T) {
	t.Parallel()
	in := strings.Join([]string{
		"2026-11-10T12:00:01.5Z {\"msg\":\"a\"}",
		"not a timestamp line",
		"2026-11-10T12:00:02.000000001Z 1:M 10 Nov 2026 12:00:02.000 * Ready to accept connections tcp",
		"",
	}, "\n")
	lines, err := ParseLog(bufio.NewScanner(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(lines), lines)
	}
	if got := lines[0].Unix - unixAt(0); got < 1.4999 || got > 1.5001 {
		t.Errorf("first line at %.4f s, want 1.5", got)
	}
	if lines[0].Text != `{"msg":"a"}` {
		t.Errorf("first line text %q", lines[0].Text)
	}
}

// replicaJSON is one gateway log line at sec seconds into the run.
func replicaJSON(sec float64, msg, errText string) LogLine {
	text := fmt.Sprintf(`{"time":"x","level":"WARN","msg":%q}`, msg)
	if errText != "" {
		text = fmt.Sprintf(`{"time":"x","level":"WARN","msg":%q,"error":%q}`, msg, errText)
	}
	return LogLine{Unix: unixAt(sec), Text: text}
}

func TestStoreLog(t *testing.T) {
	t.Parallel()
	// events(): replica 2 killed at 10 s, Redis killed at 20 s and started
	// at 30 s; the run ends at 40 s.
	s := &Summary{}
	for _, e := range sortedEvents() {
		s.Events = append(s.Events, EventResult{Event: e, AtSeconds: e.IssuedUnix - unixAt(0)})
	}
	const timeout = "budget settle budget: context deadline exceeded"
	const dns = "cache: get: dial tcp: lookup redis on 127.0.0.11:53: server misbehaving"
	logs := map[string][]LogLine{
		"switchboard-1": {
			replicaJSON(5, "llm call", "upstream 500"), // not a store error
			replicaJSON(20.2, "budget store unreachable, using per-replica grace", timeout),
			replicaJSON(20.3, "rate limit store unavailable, using per-replica limits", timeout),
			replicaJSON(20.4, "cache store failed; bypassing the cache", timeout),
			{Unix: unixAt(21.5), Text: "redis: 2026/11/10 12:00:21 pool.go:770: redis: connection pool: failed to dial after 5 attempts: dial tcp: lookup redis on 127.0.0.11:53: server misbehaving"},
			replicaJSON(22, "cache store failed; bypassing the cache", dns),
			replicaJSON(30.2, "cache store failed; bypassing the cache", "LOADING Redis is loading the dataset in memory"),
			replicaJSON(31.0, "rate limit store recovered, using shared limits", ""),
			replicaJSON(31.2, "cache store reachable again", ""),
			replicaJSON(31.5, "budget store recovered", ""),
		},
		// Replica 2 is down from 10 s: its lines before the kill count
		// for the outage table but it is not waited for after the start.
		"switchboard-2": {
			replicaJSON(9, "budget store unreachable, using per-replica grace", timeout),
		},
		"switchboard-3": {
			replicaJSON(20.5, "budget store unreachable, using per-replica grace", timeout),
			replicaJSON(32.25, "budget store recovered", ""),
		},
		RedisTarget: {
			{Unix: unixAt(30.3), Text: "1:M * DB loaded from append only file: 0.012 seconds"},
			{Unix: unixAt(30.4), Text: "1:M * Ready to accept connections tcp"},
		},
	}
	sl := storeLog(s, logs, t0, 40)
	if sl == nil {
		t.Fatal("no store log")
	}
	if len(sl.Outages) != 1 {
		t.Fatalf("outages %+v", sl.Outages)
	}
	o := sl.Outages[0]
	if o.AtSeconds != 20 || o.ToSeconds != 30 {
		t.Errorf("outage window %v-%v, want 20-30", o.AtSeconds, o.ToSeconds)
	}
	if len(o.Classes) != 2 || o.Classes[0].Class != ErrClassTimeout || o.Classes[0].Lines != 4 ||
		o.Classes[1].Class != ErrClassDNS || o.Classes[1].Lines != 2 || o.Classes[1].PoolLines != 1 {
		t.Errorf("outage classes %+v", o.Classes)
	}
	if got := o.Classes[0].FirstSeconds; got < 0.199 || got > 0.201 {
		t.Errorf("first timeout %.3f s after the kill, want 0.2", got)
	}
	if len(sl.Restarts) != 1 {
		t.Fatalf("restarts %+v", sl.Restarts)
	}
	r := sl.Restarts[0]
	if r.LoadedSeconds == nil || *r.LoadedSeconds != 0.012 {
		t.Errorf("loaded %v, want 0.012", r.LoadedSeconds)
	}
	if r.ReadySeconds == nil || *r.ReadySeconds < 0.399 || *r.ReadySeconds > 0.401 {
		t.Errorf("ready %v, want 0.4", r.ReadySeconds)
	}
	if r.LoadingErrors != 1 {
		t.Errorf("loading errors %d, want 1", r.LoadingErrors)
	}
	// Replica 1's three components and replica 3's budget; replica 2 was
	// down at the start.
	if len(r.Recoveries) != 4 {
		t.Fatalf("recoveries %+v", r.Recoveries)
	}
	for _, rec := range r.Recoveries {
		if rec.Replica == "switchboard-2" {
			t.Errorf("waited for the dead replica: %+v", rec)
		}
		if rec.Seconds == nil {
			t.Errorf("%s %s did not recover", rec.Replica, rec.Component)
		}
	}
	if r.DegradedSeconds == nil || *r.DegradedSeconds < 2.249 || *r.DegradedSeconds > 2.251 {
		t.Errorf("degraded %v s, want 2.25 (replica 3's budget)", r.DegradedSeconds)
	}
}

func TestStoreLogNotRecovered(t *testing.T) {
	t.Parallel()
	s := &Summary{}
	for _, e := range sortedEvents() {
		s.Events = append(s.Events, EventResult{Event: e, AtSeconds: e.IssuedUnix - unixAt(0)})
	}
	logs := map[string][]LogLine{
		"switchboard-1": {
			replicaJSON(20.2, "budget store unreachable, using per-replica grace", "EOF"),
			replicaJSON(20.3, "rate limit store unavailable, using per-replica limits", "EOF"),
			replicaJSON(31, "rate limit store recovered, using shared limits", ""),
		},
	}
	sl := storeLog(s, logs, t0, 40)
	r := sl.Restarts[0]
	if r.DegradedSeconds != nil {
		t.Errorf("degraded %v s, want nil: the budget never recovered", *r.DegradedSeconds)
	}
	if storeLog(s, nil, t0, 40) != nil {
		t.Error("a run without logs has a store log")
	}
}

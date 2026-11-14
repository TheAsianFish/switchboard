package chaos

import (
	"fmt"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/bench"
)

// slowLine is a replica's "store call slow" line for a main-stream
// request, logged when the call ended (sec into the run).
func slowLine(sec float64, component string, waitMS float64, errText string, req int) LogLine {
	text := fmt.Sprintf(`{"time":"x","level":"WARN","msg":"store call slow","request_id":%q,"component":%q,"op":"get","wait_ms":%v`,
		bench.RequestID(StreamMain, req), component, waitMS)
	if errText != "" {
		text += fmt.Sprintf(`,"error":%q`, errText)
	}
	return LogLine{Unix: unixAt(sec), Text: text + "}"}
}

func cacheFailLine(sec, waitMS float64, errText string) LogLine {
	return LogLine{Unix: unixAt(sec), Text: fmt.Sprintf(
		`{"time":"x","level":"WARN","msg":"cache store failed; bypassing the cache","op":"get","error":%q,"for":"1s","wait_ms":%v}`, errText, waitMS)}
}

func TestAnalyseOutage(t *testing.T) {
	t.Parallel()
	// events(): replica 2 killed at 10 s, Redis killed at 20 s and started
	// at 30 s; 100 requests a second, request i scheduled at i/100 s.
	const timeout = "cache: get: i/o timeout"
	const dial = "cache: get: dial tcp 172.18.0.2:6379: i/o timeout"
	slow := map[int]float64{
		1995: 400,            // in flight when Redis went: its call started at 19.95 s
		2001: 380, 2002: 390, // the cut's burst
		2160: 420, 2170: 430, // a second burst, 1.6 s on
		2500: 500, // slow, but with no slow store call logged
		3500: 370, // slow after Redis is back, still in the stretch
		500:  999, // before the outage: not counted in it
	}
	st := synthMain(func(i int, _ float64) (string, float64) { return "", slow[i] })
	logs := map[string][]LogLine{
		"switchboard-1": {
			slowLine(20.25, "cache_lookup", 300, timeout, 1995),
			slowLine(20.26, "cache_lookup", 250, timeout, 2001),
			slowLine(20.27, "limiter", 60, "", 2001), // shorter: the cache is what held it
			cacheFailLine(20.25, 300, timeout),
			cacheFailLine(20.26, 250, timeout),
			cacheFailLine(21.85, 250, dial),
			cacheFailLine(24, 0.4, dial), // a later, fast failure: its own round
			// After Redis is ready (30.4 s): the pool's stale dial error.
			cacheFailLine(31, 0.02, "cache: get: dial tcp: lookup redis on 127.0.0.11:53: server misbehaving"),
			{Unix: unixAt(21.9), Text: "redis: 2026/11/10 12:00:21 pool.go:1: redis: connection pool: failed to dial after 5 attempts: dial tcp: i/o timeout"},
		},
		"switchboard-3": {
			slowLine(20.29, "cache_lookup", 260, timeout, 2002),
			slowLine(21.85, "cache_lookup", 250, dial, 2160),
			slowLine(21.95, "budget", 250, dial, 2170),
			slowLine(35.2, "cache_fill", 120, "", 3500),
		},
		RedisTarget: {{Unix: unixAt(30.4), Text: "1:M 10 Nov 2026 12:00:30.400 * Ready to accept connections tcp"}},
	}
	in := Input{Meta: meta(), Events: events(), Streams: map[string]*Stream{StreamMain: st}, Logs: logs}
	o, err := AnalyseOutage(in)
	if err != nil {
		t.Fatal(err)
	}
	if o.Run.SlowRequests != 8 || o.Run.SlowByComponent[ComponentNone] != 2 || o.Run.SlowByComponent["cache_lookup"] != 4 {
		t.Fatalf("run = %+v", o.Run)
	}
	if o.Run.StoreCallsTimedOut != 5 || o.Run.StoreCalls["cache_lookup"] != 4 || o.Run.StoreCalls["limiter"] != 1 {
		t.Errorf("store calls = %+v, %d timed out", o.Run.StoreCalls, o.Run.StoreCallsTimedOut)
	}
	if o.Run.RepeatOK != 400 || o.Run.RepeatHits != 400 || o.Run.RepeatHitRate != 1 {
		t.Errorf("repeats = %+v", o.Run)
	}
	if len(o.Outages) != 1 {
		t.Fatalf("outages = %+v", o.Outages)
	}
	x := o.Outages[0]
	if x.AtSeconds != 20 || x.BackSeconds == nil || *x.BackSeconds != 10 || x.ToSeconds != 40 {
		t.Errorf("outage window = %v, %v, %v", x.AtSeconds, x.BackSeconds, x.ToSeconds)
	}
	if x.SlowRequests != 7 || x.SlowByComponent["cache_lookup"] != 4 || x.SlowByComponent["budget"] != 1 ||
		x.SlowByComponent["cache_fill"] != 1 || x.SlowByComponent[ComponentNone] != 1 {
		t.Errorf("outage slow = %d %v", x.SlowRequests, x.SlowByComponent)
	}
	want := []struct {
		from     float64
		requests int
		inFlight int
		comp     map[string]int
	}{
		{-0.05, 3, 1, map[string]int{"cache_lookup": 3}},
		{1.6, 2, 0, map[string]int{"cache_lookup": 1, "budget": 1}},
		{5, 1, 0, map[string]int{ComponentNone: 1}},
		{15, 1, 0, map[string]int{"cache_fill": 1}},
	}
	if len(x.Bursts) != len(want) {
		t.Fatalf("bursts = %+v", x.Bursts)
	}
	for i, w := range want {
		b := x.Bursts[i]
		if b.FromSeconds != w.from || b.Requests != w.requests || b.InFlightAtCut != w.inFlight || fmt.Sprint(b.ByComponent) != fmt.Sprint(w.comp) {
			t.Errorf("burst %d = %+v, want %+v", i, b, w)
		}
	}
	if b := x.Bursts[0]; b.Replicas != 2 || b.ByClass[ErrClassTimeout] != 3 || b.MedianWaitMS != 260 {
		t.Errorf("cut burst = %+v", b)
	}
	// The disconnect command returned 0.3 s after it was issued: the
	// calls that started at 20.01 s and 20.03 s may have been in flight
	// at the cut or not, the one that started at 19.95 s was.
	if b := x.Bursts[0]; x.DoneSeconds != 0.3 || b.StartedBeforeDone != 3 || b.CallsFromSeconds == nil ||
		*b.CallsFromSeconds != -0.05 || *b.CallsToSeconds != 0.03 {
		t.Errorf("cut burst calls = %+v, command done %v", b, x.DoneSeconds)
	}
	if b := x.Bursts[1]; b.ByClass[ErrClassDialTimeout] != 2 {
		t.Errorf("second burst classes = %v", b.ByClass)
	}
	rounds := x.CacheRounds
	if len(rounds) != 4 || rounds[0].Failures != 2 || rounds[0].SlowFailures != 2 || rounds[0].MaxWaitMS != 300 ||
		rounds[1].FromSeconds != 1.85 || rounds[2].SlowFailures != 0 || rounds[2].ByClass[ErrClassDialTimeout] != 1 {
		t.Errorf("cache rounds = %+v", rounds)
	}
	if x.PoolLines != 1 {
		t.Errorf("pool lines = %d", x.PoolLines)
	}
	// Timed out before Redis came back at 30 s: the five timeouts and dial
	// timeouts; the slow fill at 35.2 s succeeded.
	if x.TimedOut != 5 {
		t.Errorf("timed out = %d", x.TimedOut)
	}
	if len(x.Replicas) != 1 || x.Replicas[0].Replica != "switchboard-1" || x.Replicas[0].FirstFastSeconds == nil ||
		*x.Replicas[0].FirstFastSeconds != 4 || x.Replicas[0].DialsBefore != 1 || x.Replicas[0].SlowCacheFailures != 3 {
		t.Errorf("replicas = %+v", x.Replicas)
	}
	if len(o.AfterReady) != 1 || o.AfterReady[0].Failures != 1 || o.AfterReady[0].ByClass[ErrClassDNS] != 1 ||
		o.AfterReady[0].LastSeconds == nil || *o.AfterReady[0].LastSeconds != 0.6 {
		t.Errorf("after ready = %+v", o.AfterReady)
	}
	if len(o.Restarts) != 1 || o.Restarts[0].ReadySeconds == nil || *o.Restarts[0].ReadySeconds != 0.4 {
		t.Errorf("restarts = %+v", o.Restarts)
	}
}

func TestAnalyseOutageNeedsLogs(t *testing.T) {
	t.Parallel()
	in := Input{Meta: meta(), Events: events(), Streams: map[string]*Stream{StreamMain: synthMain(nil)}}
	if _, err := AnalyseOutage(in); err == nil {
		t.Fatal("analysed a run without logs")
	}
}

// A replica started between two outages is marked restarted in the
// second, since go-redis's failed-dial count starts again with the
// process.
func TestAnalyseOutageMarksRestartedReplicas(t *testing.T) {
	t.Parallel()
	ev := func(at float64, action, target string) Event {
		return Event{Name: action + " " + target, Action: action, Target: target, Phase: target + " " + action,
			PlannedSeconds: at, IssuedUnix: unixAt(at), DoneUnix: unixAt(at + 0.2)}
	}
	events := []Event{
		ev(5, ActionDisconnect, RedisTarget), ev(10, ActionConnect, RedisTarget),
		ev(12, ActionKill, "switchboard-2"), ev(15, ActionStart, "switchboard-2"),
		ev(20, ActionDisconnect, RedisTarget), ev(25, ActionConnect, RedisTarget),
	}
	const dial = "cache: get: dial tcp 172.18.0.2:6379: i/o timeout"
	logs := map[string][]LogLine{
		"switchboard-1": {cacheFailLine(5.3, 250, dial), cacheFailLine(20.3, 250, dial)},
		"switchboard-2": {cacheFailLine(5.3, 250, dial), cacheFailLine(20.3, 250, dial)},
	}
	st := synthMain(func(int, float64) (string, float64) { return "", 0 })
	o, err := AnalyseOutage(Input{Meta: meta(), Events: events, Streams: map[string]*Stream{StreamMain: st}, Logs: logs})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Outages) != 2 {
		t.Fatalf("outages = %+v", o.Outages)
	}
	got := map[string][2]bool{}
	for i, x := range o.Outages {
		for _, r := range x.Replicas {
			v := got[r.Replica]
			v[i] = r.Restarted
			got[r.Replica] = v
		}
	}
	if got["switchboard-1"] != [2]bool{} || got["switchboard-2"] != [2]bool{false, true} {
		t.Errorf("restarted = %v", got)
	}
}

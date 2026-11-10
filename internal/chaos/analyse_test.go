package chaos

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func unixAt(sec float64) float64 { return float64(t0.UnixNano())/1e9 + sec }

func f(v float64) *float64 { return &v }

func meta() Meta {
	var m Meta
	m.Replicas = 3
	m.DurationSeconds = 40
	m.Limit.Model, m.Limit.Rate, m.Limit.PeriodSeconds = "lim", 10, 1
	m.Budget.Model, m.Budget.USD, m.Budget.GraceUSD = "bud", "0.001", "0.0001"
	m.Budget.InputPrice, m.Budget.OutputPrice = "3", "15"
	return m
}

// events kill replica 2 at 10 s, Redis at 20 s and start Redis at 30 s.
func events() []Event {
	ev := func(at float64, action, target, phase string) Event {
		return Event{Name: action + " " + target, Action: action, Target: target, Phase: phase,
			PlannedSeconds: at, IssuedUnix: unixAt(at), DoneUnix: unixAt(at + 0.3)}
	}
	// Out of order on purpose: Analyse sorts by time.
	return []Event{
		ev(20, ActionKill, RedisTarget, "both down"),
		ev(10, ActionKill, "switchboard-2", "replica down"),
		ev(30, ActionStart, RedisTarget, "redis back"),
	}
}

// synthMain is 100 requests per second for 40 s, 50 ms each, every
// tenth a hit; fail(i, at) may turn a request into a failure or make it
// slow.
func synthMain(fail func(i int, at float64) (outcome string, latency float64)) *Stream {
	st := &Stream{Result: bench.Result{StartedAt: t0}}
	st.Result.Config.DurationSec = 40
	for i := range 4000 {
		at := float64(i) / 100
		r := bench.Record{I: i, At: at, Outcome: bench.OutcomeOK, Status: 200, Cache: "miss"}
		lat := 50.0
		if i%10 == 0 {
			slot := i % 7
			r.Pool, r.Cache = &slot, "hit-exact"
		}
		if fail != nil {
			if o, l := fail(i, at); o != "" {
				r.Outcome, r.Cache, r.Status = o, "", 0
				lat = l
			} else if l > 0 {
				lat = l
			}
		}
		r.LatencyMS, r.TTFTMS = f(lat), f(lat/2)
		st.Records = append(st.Records, r)
	}
	return st
}

func TestPhasesAndBoundaries(t *testing.T) {
	t.Parallel()
	ps := phases(meta(), sortedEvents(), t0, 40)
	want := []struct {
		name     string
		from, to float64
		replicas int
		redis    bool
	}{
		{PhaseSteady, 0, 10, 3, true},
		{"replica down", 10, 20, 2, true},
		{"both down", 20, 30, 2, false},
		{"redis back", 30, 40, 2, true},
	}
	if len(ps) != len(want) {
		t.Fatalf("%d phases: %+v", len(ps), ps)
	}
	for i, w := range want {
		p := ps[i]
		if p.Name != w.name || p.FromSeconds != w.from || p.ToSeconds != w.to || p.ReplicasUp != w.replicas || p.RedisUp != w.redis {
			t.Errorf("phase %d = %+v, want %+v", i, p, w)
		}
	}
	for _, tt := range []struct {
		t    float64
		want int
	}{{-1, 0}, {0, 0}, {9.999, 0}, {10, 1}, {19.9, 1}, {20, 2}, {30, 3}, {45, 3}} {
		if got := phaseOf(ps, tt.t); got != tt.want {
			t.Errorf("phaseOf(%v) = %d, want %d", tt.t, got, tt.want)
		}
	}
}

func sortedEvents() []Event {
	ev := events()
	return []Event{ev[1], ev[0], ev[2]}
}

func TestMainPhaseByObservedTime(t *testing.T) {
	t.Parallel()
	// The ten streams in flight when replica 2 dies were scheduled before
	// the kill but fail after it: they count against "replica down".
	st := synthMain(func(i int, at float64) (string, float64) {
		if at >= 9.9 && at < 10 {
			return bench.OutcomeMalformedStream, 150
		}
		if at >= 20 && at < 20.5 {
			return "", 300 // Redis timeouts: slow, not failed
		}
		return "", 0
	})
	s, err := Analyse(Input{Meta: meta(), Events: events(), Streams: map[string]*Stream{StreamMain: st}}, DefaultRule)
	if err != nil {
		t.Fatal(err)
	}
	steady, down, both := s.Phases[0].Main, s.Phases[1].Main, s.Phases[2].Main
	// Requests observed in [0, 10): the 995 scheduled before 9.95 with
	// 50 ms, minus the five of the failing ten among them.
	if steady.Errors != 0 || steady.Requests != 990 {
		t.Errorf("steady: %d requests, %d errors", steady.Requests, steady.Errors)
	}
	if down.Errors != 10 || down.ErrorsByKind[bench.OutcomeMalformedStream] != 10 || down.ErrorRate != round6(10.0/float64(down.Requests)) {
		t.Errorf("replica down: %+v", down)
	}
	if steady.Latency.P50 != 50 || steady.TTFT.P50 != 25 {
		t.Errorf("steady latency %+v ttft %+v", steady.Latency, steady.TTFT)
	}
	if both.Latency.P99 != 300 || both.Slow != 50 || steady.Slow != 0 {
		t.Errorf("Redis-down p99 %v and %d slow (steady %d), want 300 and 50 (0)", both.Latency.P99, both.Slow, steady.Slow)
	}
	// Every tenth request hits, and those are the repeats.
	if steady.CacheHitRate < 0.099 || steady.CacheHitRate > 0.101 || steady.RepeatHitRate != 1 || steady.CacheStatuses["hit-exact"] != steady.CacheHits {
		t.Errorf("steady cache: %+v", steady)
	}
	total := 0
	for _, p := range s.Phases {
		total += p.Main.Requests
	}
	if total != 4000 {
		t.Errorf("phases hold %d requests, want 4000", total)
	}
	if s.Events[0].Name != "kill switchboard-2" || s.Events[0].AtSeconds != 10 || s.Events[0].DoneSeconds != 10.3 {
		t.Errorf("first event %+v", s.Events[0])
	}
}

func TestRecoveryRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fail func(i int, at float64) (string, float64)
		// want recovery seconds per event; -1 for not recovered.
		want [3]float64
		// wantErrors before recovery after the first event.
		wantErrors int
	}{
		{"clean", nil, [3]float64{0, 0, 0}, 0},
		{
			// Errors in 10..12.5 s: the first good run starts at 13.
			"errors for 2.5 s", func(i int, at float64) (string, float64) {
				if at >= 10 && at < 12.5 && i%5 == 0 {
					return "http_502", 1
				}
				return "", 0
			}, [3]float64{3, 0, 0}, 50,
		},
		{
			// 1% errors is at the floor of max(1%, 2 × 0) and still good.
			"at the error floor", func(i int, at float64) (string, float64) {
				if at >= 10 && i%100 == 0 {
					return "http_502", 1
				}
				return "", 0
			}, [3]float64{0, 0, 0}, 0,
		},
		{
			// Slow (p99 above 2 × 50 ms) for all of the Redis outage: the
			// rule never holds before Redis is back.
			"slow until the next event", func(i int, at float64) (string, float64) {
				if at >= 20 && at < 30 && i%20 == 0 {
					return "", 101
				}
				return "", 0
			}, [3]float64{0, -1, 0}, 0,
		},
		{
			// Good for only four buckets in a row at the end: the run ends
			// before five.
			"not recovered by the end", func(i int, at float64) (string, float64) {
				if at >= 30 && at < 35.9 {
					return bench.OutcomeTransport, 1
				}
				return "", 0
			}, [3]float64{0, 0, -1}, 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := Analyse(Input{Meta: meta(), Events: events(), Streams: map[string]*Stream{StreamMain: synthMain(tt.fail)}}, DefaultRule)
			if err != nil {
				t.Fatal(err)
			}
			if s.Reference.P99MS != 50 || s.Reference.P99LimitMS != 100 || s.Reference.ErrorLimit != 0.01 {
				t.Fatalf("reference %+v", s.Reference)
			}
			for i, want := range tt.want {
				e := s.Events[i]
				if want < 0 {
					if e.Recovered || e.RecoverySeconds != nil {
						t.Errorf("%s: recovered after %v s, want not recovered", e.Name, *e.RecoverySeconds)
					}
					continue
				}
				if !e.Recovered || e.RecoverySeconds == nil || *e.RecoverySeconds != want {
					t.Errorf("%s: recovered %v after %v, want %v s", e.Name, e.Recovered, e.RecoverySeconds, want)
				}
			}
			if got := s.Events[0].ErrorsBeforeRecovery; got != tt.wantErrors {
				t.Errorf("errors before recovery %d, want %d", got, tt.wantErrors)
			}
			if s.Events[0].WindowSeconds != 10 {
				t.Errorf("window %v, want 10", s.Events[0].WindowSeconds)
			}
		})
	}
}

func TestRecoveryStartsAfterTheEvent(t *testing.T) {
	t.Parallel()
	// An event at 10.4 s: the first candidate bucket is [11, 12).
	ev := events()
	for i := range ev {
		ev[i].IssuedUnix += 0.4
	}
	s, err := Analyse(Input{Meta: meta(), Events: ev, Streams: map[string]*Stream{StreamMain: synthMain(nil)}}, DefaultRule)
	if err != nil {
		t.Fatal(err)
	}
	if e := s.Events[0]; e.RecoverySeconds == nil || *e.RecoverySeconds != 0.6 {
		t.Errorf("recovery %v, want 0.6", e.RecoverySeconds)
	}
}

func ledgerAt(model string, secs ...float64) []fake.Entry {
	var out []fake.Entry
	for _, s := range secs {
		out = append(out, fake.Entry{Time: t0.Add(time.Duration(s * float64(time.Second))), Model: model, InputTokens: 5, OutputTokens: 30, Completed: true})
	}
	return out
}

func spread(from, to float64, n int) []float64 {
	var out []float64
	for i := range n {
		out = append(out, from+(to-from)*float64(i)/float64(n))
	}
	return out
}

func TestOverAdmissions(t *testing.T) {
	t.Parallel()
	var ledger []fake.Entry
	for s := range 40 {
		n := 10
		switch {
		case s == 5:
			n = 13 // three over with Redis up
		case s == 20:
			n = 14 // the window Redis dies in: shared and local counts
		case s > 20 && s < 30:
			n = 6 // two replicas at a third of the rate each
		}
		ledger = append(ledger, ledgerAt("lim", spread(float64(s), float64(s)+0.99, n)...)...)
	}
	ledger = append(ledger, ledgerAt("other", spread(0, 40, 400)...)...)
	lim := &Stream{Result: bench.Result{StartedAt: t0.Add(100 * time.Millisecond)}}
	for i := range 1200 {
		o := bench.OutcomeOK
		if i%3 != 0 {
			o = "http_429"
		}
		if i == 1 {
			o = "http_502"
		}
		lim.Records = append(lim.Records, bench.Record{I: i, At: float64(i) / 30, Outcome: o, LatencyMS: f(10)})
	}
	s, err := Analyse(Input{Meta: meta(), Events: events(), Ledger: ledger,
		Streams: map[string]*Stream{StreamMain: synthMain(nil), StreamLimited: lim}}, DefaultRule)
	if err != nil {
		t.Fatal(err)
	}
	want := []LimitedPhase{
		{Admitted: 103, Windows: 10, MostInWindow: 13, OverAdmissions: 3, LocalBound: 10},
		{Admitted: 100, Windows: 10, MostInWindow: 10, OverAdmissions: 0, LocalBound: 10},
		{Admitted: 68, Windows: 10, MostInWindow: 14, OverAdmissions: 4, LocalBound: 6},
		{Admitted: 100, Windows: 10, MostInWindow: 10, OverAdmissions: 0, LocalBound: 10},
	}
	offered := 0
	for i, w := range want {
		got := s.Phases[i].Limited
		offered += got.Offered
		w.Offered, w.Refused, w.OtherErrors = got.Offered, got.Refused, got.OtherErrors
		if got != w {
			t.Errorf("phase %s: %+v, want %+v", s.Phases[i].Name, got, w)
		}
	}
	if offered != 1200 || s.Phases[0].Limited.OtherErrors != 1 || s.Phases[0].Limited.Refused == 0 {
		t.Errorf("client view: offered %d, phase 0 %+v", offered, s.Phases[0].Limited)
	}
}

func TestBudgetOvershoot(t *testing.T) {
	t.Parallel()
	// $0.000465 a reply: two replies before the budget of $0.001 is
	// reached, three more while Redis is down (grace bound 2 × $0.0001),
	// two after it is back.
	ledger := ledgerAt("bud", 1, 2, 22, 23, 24, 31, 32)
	b := &Stream{Result: bench.Result{StartedAt: t0}}
	for i, o := range []string{"ok", "ok", "http_429", "ok", "ok", "ok", "http_429", "ok", "ok", "timeout"} {
		b.Records = append(b.Records, bench.Record{I: i, At: float64(i) * 4, Outcome: o, LatencyMS: f(10)})
	}
	s, err := Analyse(Input{Meta: meta(), Events: events(), Ledger: ledger,
		Streams: map[string]*Stream{StreamMain: synthMain(nil), StreamBudget: b}}, DefaultRule)
	if err != nil {
		t.Fatal(err)
	}
	const reply = cost.Nanos(465_000)
	want := []BudgetPhase{
		{Admitted: 2, Spent: 2 * reply, Cumulative: 2 * reply},
		{},
		{Admitted: 3, Spent: 3 * reply, Cumulative: 5 * reply, Overshoot: 5*reply - cost.Dollar/1000, GraceBound: 2 * cost.Dollar / 10000},
		{Admitted: 2, Spent: 2 * reply, Cumulative: 7 * reply, Overshoot: 7*reply - cost.Dollar/1000},
	}
	want[1].Cumulative = want[0].Cumulative
	for i, w := range want {
		got := s.Phases[i].Budget
		w.Offered, w.OK, w.Refused, w.OtherErrors = got.Offered, got.OK, got.Refused, got.OtherErrors
		if got != w {
			t.Errorf("phase %s: %+v, want %+v", s.Phases[i].Name, got, w)
		}
	}
	if p := s.Phases[0].Budget; p.Offered != 3 || p.OK != 2 || p.Refused != 1 {
		t.Errorf("steady client view %+v", p)
	}
	if p := s.Phases[3].Budget; p.OtherErrors != 1 {
		t.Errorf("last phase client view %+v", p)
	}
}

func TestAnalyseRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := Analyse(Input{Meta: meta()}, DefaultRule); err == nil {
		t.Error("no main stream accepted")
	}
	m := meta()
	m.Budget.USD = "lots"
	if _, err := Analyse(Input{Meta: m, Streams: map[string]*Stream{StreamMain: synthMain(nil), StreamBudget: {}}}, DefaultRule); err == nil {
		t.Error("bad budget accepted")
	}
	if _, err := Analyse(Input{Meta: meta(), Streams: map[string]*Stream{StreamMain: synthMain(nil)}}, Rule{}); err == nil {
		t.Error("zero rule accepted")
	}
}

func writeGz(t *testing.T, path string, write func(*json.Encoder)) {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	write(json.NewEncoder(z))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMainReadsRawFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := synthMain(nil)
	writeFile(t, filepath.Join(dir, MetaFile), meta())
	writeFile(t, filepath.Join(dir, EventsFile), events())
	writeFile(t, filepath.Join(dir, "main.json"), st.Result)
	writeGz(t, filepath.Join(dir, "main.jsonl.gz"), func(e *json.Encoder) {
		for _, r := range st.Records {
			_ = e.Encode(r)
		}
	})
	writeGz(t, filepath.Join(dir, LedgerFile), func(e *json.Encoder) {
		_ = e.Encode(map[string]any{"entries": ledgerAt("lim", 1, 2)})
	})
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"report", "--dir", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("Main = %d: %s", code, stderr.String())
	}
	var s Summary
	b, err := os.ReadFile(filepath.Join(dir, SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Phases) != 4 || s.Phases[0].Main.Requests == 0 || len(s.Buckets) != 40 || len(s.Streams) != 1 {
		t.Errorf("summary: %d phases, %d buckets, %d streams", len(s.Phases), len(s.Buckets), len(s.Streams))
	}
	if !strings.Contains(stderr.String(), "kill switchboard-2") {
		t.Errorf("report output %q", stderr.String())
	}
	if code := Main(context.Background(), []string{"nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("bad subcommand: exit %d", code)
	}
}

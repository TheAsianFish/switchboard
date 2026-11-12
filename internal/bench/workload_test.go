package bench

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cache"
)

func TestCacheHeadersMatchGateway(t *testing.T) {
	t.Parallel()
	if CacheDirectiveHeader != cache.DirectiveHeader || CacheStatusHeader != cache.StatusHeader {
		t.Errorf("bench headers %q, %q; gateway %q, %q", CacheDirectiveHeader, CacheStatusHeader, cache.DirectiveHeader, cache.StatusHeader)
	}
}

func TestPoolSlot(t *testing.T) {
	t.Parallel()
	const n = 20000
	tests := []struct {
		name         string
		seed         uint64
		repeat       float64
		pool         int
		wantFraction float64
	}{
		{"off", 1, 0, 50, 0},
		{"no pool", 1, 0.3, 0, 0},
		{"thirty percent", 1, 0.3, 50, 0.3},
		{"other seed", 7, 0.3, 50, 0.3},
		{"all", 1, 1, 5, 1},
		{"pool of one", 3, 0.5, 1, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repeats := 0
			used := map[int]int{}
			for i := range n {
				slot := PoolSlot(tt.seed, i, tt.repeat, tt.pool)
				if slot != PoolSlot(tt.seed, i, tt.repeat, tt.pool) {
					t.Fatalf("request %d: slot is not deterministic", i)
				}
				if slot < 0 {
					continue
				}
				if slot >= tt.pool {
					t.Fatalf("request %d: slot %d outside a pool of %d", i, slot, tt.pool)
				}
				repeats++
				used[slot]++
			}
			got := float64(repeats) / n
			if d := got - tt.wantFraction; d > 0.015 || d < -0.015 {
				t.Errorf("repeat fraction %.4f, want %.2f", got, tt.wantFraction)
			}
			if tt.wantFraction > 0 && len(used) != tt.pool {
				t.Errorf("%d of %d pool prompts used", len(used), tt.pool)
			}
		})
	}
	// A different seed picks different requests.
	same := 0
	for i := range 1000 {
		if PoolSlot(1, i, 0.3, 50) == PoolSlot(2, i, 0.3, 50) {
			same++
		}
	}
	if same > 900 {
		t.Errorf("seeds 1 and 2 agree on %d of 1000 requests", same)
	}
}

func TestPromptRepeatsOnlyPoolPrompts(t *testing.T) {
	t.Parallel()
	cfg := Config{Repeat: 0.3, RepeatPool: 10, Seed: 1}
	seen := map[string][]int{}
	for i := range 2000 {
		p := prompt(&cfg, i)
		seen[p] = append(seen[p], i)
	}
	pool := 0
	for p, is := range seen {
		if strings.HasPrefix(p, "bench pool prompt ") {
			pool++
			continue
		}
		if len(is) != 1 {
			t.Errorf("unique prompt %q sent by %d requests", p, len(is))
		}
	}
	if pool != 10 {
		t.Errorf("%d distinct pool prompts, want 10", pool)
	}
	// Without Repeat the prompts are the ones Q1 and Q4 were measured with.
	if got := prompt(&Config{}, 7); got != "bench request 7: say something" {
		t.Errorf("prompt without repeat = %q", got)
	}
}

func TestRunRejectsBadRepeat(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{URL: "http://x", Rate: 1, Duration: time.Second, Repeat: -0.1},
		{URL: "http://x", Rate: 1, Duration: time.Second, Repeat: 1.5},
		{URL: "http://x", Rate: 1, Duration: time.Second, Repeat: 0.5, RepeatPool: -1},
	} {
		if _, err := Run(context.Background(), cfg); err == nil {
			t.Errorf("Run(%+v) accepted a bad repeat", cfg)
		}
	}
}

// cachingServer answers like a gateway with an exact cache: the first
// request with a prompt is a miss, later ones are hits. Requests for
// "fail" prompts get a 503.
func cachingServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]bool{}
	var directives []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request body: %v", err)
			return
		}
		p := req.Messages[0].Content
		mu.Lock()
		hit := seen[p]
		seen[p] = true
		directives = append(directives, r.Header.Get(CacheDirectiveHeader))
		mu.Unlock()
		if hit {
			w.Header().Set(CacheStatusHeader, "hit-exact")
		} else {
			w.Header().Set(CacheStatusHeader, "miss")
		}
		okJSON(w)
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), directives...)
	}
}

func readRecords(t *testing.T, r io.Reader) []Record {
	t.Helper()
	var out []Record
	s := bufio.NewScanner(r)
	for s.Scan() {
		var rec Record
		if err := json.Unmarshal(s.Bytes(), &rec); err != nil {
			t.Fatalf("log line %q: %v", s.Text(), err)
		}
		out = append(out, rec)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunWritesLog(t *testing.T) {
	t.Parallel()
	srv, directives := cachingServer(t)
	defer srv.Close()
	var log bytes.Buffer
	cfg := Config{URL: srv.URL, Model: "m", Rate: 100, Warmup: 100 * time.Millisecond, Duration: 400 * time.Millisecond,
		Repeat: 0.5, RepeatPool: 3, Seed: 1, CacheDirective: "exact", Log: &log}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, &log)
	if len(recs) != res.Requests+res.WarmupRequests || len(recs) != 50 {
		t.Fatalf("%d log lines for %d + %d requests", len(recs), res.WarmupRequests, res.Requests)
	}
	hits, warm := 0, 0
	for i, r := range recs {
		if r.I != i {
			t.Fatalf("line %d has index %d", i, r.I)
		}
		if want := float64(i) / 100; r.At < want-1e-6 || r.At > want+1e-6 {
			t.Errorf("request %d scheduled at %v s, want %v", i, r.At, want)
		}
		if r.Warmup {
			warm++
		}
		if r.Outcome != OutcomeOK || r.Status != 200 || r.Sent == nil || r.LatencyMS == nil || r.TTFTMS == nil {
			t.Fatalf("request %d: %+v", i, r)
		}
		if *r.Sent < r.At {
			t.Errorf("request %d sent at %v before its schedule %v", i, *r.Sent, r.At)
		}
		wantPool := PoolSlot(1, i, 0.5, 3)
		switch {
		case wantPool < 0 && r.Pool != nil, wantPool >= 0 && (r.Pool == nil || *r.Pool != wantPool):
			t.Errorf("request %d: pool %v, want %d", i, r.Pool, wantPool)
		}
		if r.Cache == "hit-exact" {
			hits++
			if r.Pool == nil {
				t.Errorf("request %d: a unique prompt hit", i)
			}
		} else if r.Cache != "miss" {
			t.Errorf("request %d: cache %q", i, r.Cache)
		}
	}
	if warm != 10 {
		t.Errorf("%d warm-up lines, want 10", warm)
	}
	// About 25 repeats over 3 prompts: all but the first use of each hit
	// (a few more can miss while the first is in flight).
	if hits < 10 {
		t.Errorf("%d hits, want most repeats to hit", hits)
	}
	for _, d := range directives() {
		if d != "exact" {
			t.Fatalf("cache directive %q, want exact", d)
		}
	}
	if res.Config.Repeat != 0.5 || res.Config.RepeatPool != 3 || res.Config.Seed != 1 || res.Config.CacheDirective != "exact" {
		t.Errorf("config echo %+v", res.Config)
	}
}

func TestLogRecordsFailures(t *testing.T) {
	t.Parallel()
	at := time.Unix(1000, 0)
	cfg := Config{}
	rs := []scheduled{
		{at: at, sample: sample{outcome: "http_502", status: 502, sent: at.Add(time.Millisecond), done: at.Add(3 * time.Millisecond)}},
		{at: at.Add(10 * time.Millisecond), sample: sample{outcome: OutcomeDropped}},
		{at: at.Add(20 * time.Millisecond), sample: sample{outcome: OutcomeTransport, sent: at.Add(20 * time.Millisecond), done: at.Add(1020 * time.Millisecond)}},
	}
	var buf bytes.Buffer
	if err := writeLog(&buf, &cfg, rs, at); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{
		`{"i":0,"at_s":0,"sent_s":0.001,"latency_ms":3,"outcome":"http_502","status":502}`,
		`{"i":1,"at_s":0.01,"outcome":"dropped"}`,
		`{"i":2,"at_s":0.02,"sent_s":0.02,"latency_ms":1000,"outcome":"transport_error"}`,
	}
	if len(lines) != len(want) {
		t.Fatalf("log:\n%s", buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d:\n got %s\nwant %s", i, lines[i], want[i])
		}
	}
}

func TestMainWritesGzipLog(t *testing.T) {
	t.Parallel()
	srv, _ := cachingServer(t)
	defer srv.Close()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.jsonl.gz")
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"--url", srv.URL, "--rate", "50", "--duration", "200ms", "--warmup", "0s",
		"--repeat", "1", "--repeat-pool", "1", "--cache", "exact", "--log", logPath, "--out", filepath.Join(dir, "r.json")}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Main = %d, stderr %s", code, stderr.String())
	}
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, z)
	if len(recs) != 10 {
		t.Fatalf("%d records, want 10", len(recs))
	}
	if recs[0].Cache != "miss" || recs[9].Cache != "hit-exact" {
		t.Errorf("first %q, last %q; want miss then hit-exact", recs[0].Cache, recs[9].Cache)
	}
}

// TestResultWithoutRepeatIsUnchanged keeps the raw result files of Q1 and
// Q4 readable: a run without the new options writes no new fields.
func TestResultWithoutRepeatIsUnchanged(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(echo(Config{URL: "http://x", Format: FormatOpenAI, Rate: 1}))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"repeat", "seed", "cache_directive"} {
		if strings.Contains(string(b), field) {
			t.Errorf("config echo without repeat has %q: %s", field, b)
		}
	}
}

func TestLogRecordsRetryAfter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	var log bytes.Buffer
	if _, err := Run(context.Background(), Config{URL: srv.URL, Model: "m", Rate: 50, Warmup: time.Millisecond,
		Duration: 100 * time.Millisecond, Log: &log}); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, &log)
	if len(recs) == 0 {
		t.Fatal("no log lines")
	}
	for _, r := range recs {
		if r.Outcome != "http_503" || r.RetryAfter != "7" {
			t.Fatalf("record %+v, want http_503 with Retry-After 7", r)
		}
	}
}

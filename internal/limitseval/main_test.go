package limitseval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

func ledgerServer(t *testing.T, entries []fake.Entry) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func benchFile(t *testing.T, outcomes map[string]int) string {
	t.Helper()
	b, err := json.Marshal(bench.Result{Config: bench.ConfigEcho{Rate: 5}, StartedAt: t0, Requests: 10, Outcomes: outcomes})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLimitsMain(t *testing.T) {
	t.Parallel()
	var entries []fake.Entry
	for i := range 30 {
		entries = append(entries, fake.Entry{Time: t0.Add(time.Duration(i) * 100 * time.Millisecond), Model: "fake-limits"})
	}
	entries = append(entries, fake.Entry{Time: t0, Model: "someone-else"})
	start := float64(t0.UnixNano()) / 1e9
	var out bytes.Buffer
	code := Main(context.Background(), []string{"limits", "--ledger", ledgerServer(t, entries), "--model", "fake-limits",
		"--bench", benchFile(t, map[string]int{"ok": 30, "http_429": 10}), "--scenario", "s",
		"--rate", "60", "--per", "1m", "--burst", "5",
		"--start", fmt.Sprint(start), "--end", fmt.Sprint(start + 10), "--outage-from", fmt.Sprint(start + 1), "--outage-to", fmt.Sprint(start + 2)},
		&out, &out)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var r LimitsResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	// 30 requests in 3 s against 5 + 1/s: the worst window is all 3 s, 30
	// against a bound of 8. The outage phase [1 s, 2 s) holds 10.
	if r.Admitted != 30 || r.Whole.Worst.Over != 22 || r.Whole.Worst.Seconds != 3 || len(r.Phases) != 3 || r.Phases[1].Admitted != 10 || r.Phases[1].Over != 4 {
		t.Errorf("result %+v", r)
	}
}

func TestBudgetMain(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	defer func() { _ = rdb.Close() }()
	b := budget.Budget{Limit: 10 * cost.Cent, Period: budget.PeriodTotal}
	// The gateway's ledger: two settled requests, priced at $3/$15.
	led := budget.NewRedis(rdb, "sb:", time.Minute, time.Now)
	for _, amount := range []cost.Nanos{5*3000 + 30*15000, 7*3000 + 30*15000} {
		if err := led.Settle(context.Background(), "budget", b, budget.Reservation{}, amount); err != nil {
			t.Fatal(err)
		}
	}
	entries := []fake.Entry{
		{Model: "fake-budget", InputTokens: 5, OutputTokens: 30, Completed: true},
		{Model: "fake-budget", InputTokens: 7, OutputTokens: 30, Completed: true},
	}
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "# TYPE switchboard_cost_dollars_total counter\n"+
			`switchboard_cost_dollars_total{key="budget",model="fake-budget"} 0.000936`+"\n"+
			`switchboard_cost_dollars_total{key="other",model="x"} 1`+"\n")
	}))
	defer metrics.Close()
	var out bytes.Buffer
	code := Main(context.Background(), []string{"budget", "--ledger", ledgerServer(t, entries), "--model", "fake-budget",
		"--bench", benchFile(t, map[string]int{"ok": 2, "http_429": 8}), "--limit", "0.10",
		"--input-price", "3", "--output-price", "15", "--redis", "redis://" + m.Addr() + "/0", "--metrics", metrics.URL},
		&out, &out)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var r BudgetResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	want := cost.Nanos(12*3000 + 60*15000) // $0.000936
	if r.LedgerNanos != want || r.GatewayNanos != want || r.DiffNanos != 0 || !r.MatchesToCent || !r.MetricsToCent || !r.WithinBudget || r.MetricsDollars != 0.000936 {
		t.Errorf("result %+v", r)
	}
	if !strings.Contains(out.String(), `"scraped_replicas": 1`) {
		t.Errorf("output %s", out.String())
	}
}

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

	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// The orphan fixture: the load starts at Unix 1000, switchboard-2
// (10.0.0.12) is killed at +40 s and its connections close at +40.05 s.
// At the fake-budget price ($3 in, $15 out per MTok) a complete reply (5
// in, 30 out) is billed 465,000 nanodollars and each hold (15 estimated
// in, max_tokens 64) is 1,005,000.
const (
	orphanStart = 1000.0
	fullReply   = cost.Nanos(465_000)
	oneHold     = cost.Nanos(1_005_000)
)

func unix(s float64) time.Time {
	return time.Unix(0, int64(s*1e9)).UTC()
}

func orphanEntry(caller string, arrived, ended float64, out int, completed bool) fake.Entry {
	return fake.Entry{
		Time: unix(orphanStart + arrived), Ended: unix(orphanStart + ended), Model: "fake-budget", Stream: true,
		InputTokens: 5, OutputTokens: out, Completed: completed, Caller: caller, MaxTokens: 64, EstimatedInputTokens: 15,
	}
}

// orphanFixture returns a run whose two requests in flight on the killed
// replica were billed 315,000 (20 tokens out) and 90,000 (5 out), 405,000
// together, against 2,010,000 held; everything else, 8 complete replies,
// was billed 3,720,000. spentAfter is spent as the ledger reads it at
// +100 s.
func orphanFixture(run string, spentAfter cost.Nanos) OrphanInput {
	var m OrphanMeta
	m.Run, m.Schedule, m.GatewayCommit = run, ScheduleOrphan, "abc1234"
	m.ReplicaIPs = map[string]string{"switchboard-1": "10.0.0.11", "switchboard-2": "10.0.0.12", "switchboard-3": "10.0.0.13"}
	m.StartUnix, m.LoadSeconds, m.ObserveSeconds = orphanStart, 60, 100
	m.Kill.Target, m.Kill.PlannedSeconds = "switchboard-2", 40
	m.Budget.Model, m.Budget.InputPrice, m.Budget.OutputPrice = "fake-budget", "3", "15"
	m.Budget.MaxTokens, m.Budget.ReservationTTL = 64, "30s"
	ledger := []fake.Entry{
		orphanEntry("10.0.0.11", 1, 3, 30, true),
		orphanEntry("10.0.0.12", 2, 4, 30, true),
		orphanEntry("10.0.0.13", 3, 5, 30, true),
		// Done on the killed replica before it died: settled, not in flight.
		orphanEntry("10.0.0.12", 37, 39, 30, true),
		// In flight on the killed replica: the connection closed at 40.05.
		orphanEntry("10.0.0.12", 38.5, 40.05, 20, false),
		orphanEntry("10.0.0.12", 39.6, 40.06, 5, false),
		// Live replicas, before and after the kill.
		orphanEntry("10.0.0.11", 39, 41, 30, true),
		orphanEntry("10.0.0.13", 50, 52, 30, true),
		orphanEntry("10.0.0.11", 59.9, 61.9, 30, true),
		orphanEntry("10.0.0.13", 59.95, 61.95, 30, true),
		// Another key's model: not the budget's.
		{Time: unix(orphanStart + 10), Model: "fake-chaos", Caller: "10.0.0.12"},
	}
	others := 8 * fullReply
	usd := func(n cost.Nanos) *LedgerBalance { return &LedgerBalance{SpentUSD: n.USD()} }
	return OrphanInput{
		Meta:   m,
		Events: []Event{{Name: "kill switchboard-2", Action: ActionKill, Target: "switchboard-2", IssuedUnix: orphanStart + 40, DoneUnix: orphanStart + 40.3}},
		Ledger: ledger,
		Snapshots: []OrphanSnapshot{
			// Before the kill nothing is missing once the replies settle.
			{Label: "t=35", Unix: orphanStart + 35, RawSpent: 3 * fullReply, LedgerBalance: usd(3 * fullReply)},
			// Load over, live requests settled, orphan holds not lapsed.
			{Label: "t=65", Unix: orphanStart + 65, RawSpent: others, RawReserved: 2 * oneHold,
				LedgerBalance: &LedgerBalance{SpentUSD: others.USD(), ReservedUSD: (2 * oneHold).USD()}},
			{Label: "t=100", Unix: orphanStart + 100, RawSpent: others, LedgerBalance: usd(spentAfter)},
		},
	}
}

func TestAnalyseOrphan(t *testing.T) {
	t.Parallel()
	others := 8 * fullReply
	billed := cost.Nanos(405_000)
	tests := []struct {
		name       string
		run        string
		spentAfter cost.Nanos
		wantGap    cost.Nanos
		wantBefore bool
		wantAfter  bool
	}{
		// The lapsed holds are released and nothing is charged: the
		// provider's bill for the two orphans is missing from spent.
		{"before: holds released", "before", others, billed, true, false},
		// Charged at their held amount: an over-count of held minus billed.
		{"after: holds charged", "after", others + 2*oneHold, billed - 2*oneHold, false, true},
		// Charged twice: more over-count than the holds allow.
		{"after: charged twice", "after", others + 4*oneHold, billed - 4*oneHold, false, false},
		// Charged at the provider's price exactly: gap 0, both arms' rules
		// give their verdicts independently of the arm.
		{"after: charged exactly", "after", others + billed, 0, false, true},
		// A gap off by more than one request is not the orphans'.
		{"before: unexplained gap", "before", others - 2*fullReply, billed + 2*fullReply, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := AnalyseOrphan(orphanFixture(tt.run, tt.spentAfter))
			if err != nil {
				t.Fatal(err)
			}
			f := s.InFlight
			if f.Count != 2 || f.Completed != 0 || f.OutputTokens != 25 || f.Billed != billed || f.Held != 2*oneHold {
				t.Errorf("in flight = %+v, want 2 requests, 25 tokens, billed %d, held %d", f, billed, 2*oneHold)
			}
			if s.DiedSeconds != 40.05 || s.ExpiredBySeconds != 70.05 {
				t.Errorf("died %v, expired by %v; want 40.05, 70.05", s.DiedSeconds, s.ExpiredBySeconds)
			}
			if s.Requests != 10 || s.ByReplica["switchboard-2"] != 4 || s.Unattributed != 0 {
				t.Errorf("requests %d by replica %v, %d unattributed", s.Requests, s.ByReplica, s.Unattributed)
			}
			if h := s.HeldCheck; h == nil || h.Label != "t=65" || !h.Equal {
				t.Errorf("held check = %+v, want t=65 equal", h)
			}
			if s.AfterTTL == nil || s.AfterTTL.Label != "t=100" || s.AfterTTL.Gap != tt.wantGap {
				t.Fatalf("after TTL = %+v, want t=100 with gap %d", s.AfterTTL, tt.wantGap)
			}
			if s.Timeline[0].Gap != 0 || s.Timeline[0].PastTTL {
				t.Errorf("before the kill: %+v, want gap 0", s.Timeline[0])
			}
			if s.Before.Holds != tt.wantBefore || s.After.Holds != tt.wantAfter {
				t.Errorf("before holds %v (%s), after holds %v (%s); want %v, %v",
					s.Before.Holds, s.Before.Note, s.After.Holds, s.After.Note, tt.wantBefore, tt.wantAfter)
			}
			if s.Before.Expected != (tt.run == "before") || s.After.Expected != (tt.run == "after") {
				t.Errorf("expected flags %v %v for run %s", s.Before.Expected, s.After.Expected, tt.run)
			}
			if s.After.Bound != 2*oneHold-billed || s.After.BoundOverBilled != 3.963 {
				t.Errorf("after bound %d (%v×), want %d (3.963×)", s.After.Bound, s.After.BoundOverBilled, 2*oneHold-billed)
			}
		})
	}
}

func TestAnalyseOrphanEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		edit    func(*OrphanInput)
		wantErr string
		check   func(*testing.T, *OrphanSummary)
	}{
		{"no address for the killed replica", func(in *OrphanInput) { delete(in.Meta.ReplicaIPs, "switchboard-2") }, "no address", nil},
		{"no kill event", func(in *OrphanInput) { in.Events = nil }, "no kill", nil},
		{"not an orphan run", func(in *OrphanInput) { in.Meta.Schedule = ScheduleFlap }, "is not", nil},
		{"a hold without max_tokens", func(in *OrphanInput) { in.Ledger[4].MaxTokens = 0 }, "max_tokens", nil},
		{"no read through the ledger past the TTL", func(in *OrphanInput) { in.Snapshots[2].LedgerBalance = nil }, "", func(t *testing.T, s *OrphanSummary) {
			if s.AfterTTL != nil || s.Before.Holds || s.After.Holds || s.Before.Note == "" {
				t.Errorf("after TTL %+v, hypotheses %+v %+v; want none and a note", s.AfterTTL, s.Before, s.After)
			}
			// The timeline falls back to the raw read.
			if p := s.Timeline[2]; p.Spent != nil || p.Gap != p.Billed-p.RawSpent {
				t.Errorf("timeline point %+v, want the raw spent", p)
			}
		}},
		{"an unknown caller", func(in *OrphanInput) { in.Ledger[0].Caller = "10.0.0.99" }, "", func(t *testing.T, s *OrphanSummary) {
			if s.Unattributed != 1 {
				t.Errorf("unattributed %d, want 1", s.Unattributed)
			}
		}},
		{"no unfinished request: died at the kill", func(in *OrphanInput) {
			in.Ledger[4].Completed, in.Ledger[5].Completed = true, true
		}, "", func(t *testing.T, s *OrphanSummary) {
			if s.DiedSeconds != 40 || s.InFlight.Count != 2 || s.InFlight.Completed != 2 {
				t.Errorf("died %v, in flight %+v", s.DiedSeconds, s.InFlight)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := orphanFixture("before", 8*fullReply)
			tt.edit(&in)
			s, err := AnalyseOrphan(in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, s)
		})
	}
}

// The files chaos/run.sh writes, read back by `switchboard chaos orphan`.
func TestOrphanMain(t *testing.T) {
	t.Parallel()
	in := orphanFixture("before", 8*fullReply)
	dir := t.TempDir()
	write := func(name string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, ".gz") {
			var z bytes.Buffer
			w := gzip.NewWriter(&z)
			_, _ = w.Write(b)
			_ = w.Close()
			b = z.Bytes()
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(MetaFile, in.Meta)
	write(EventsFile, in.Events)
	write(LedgerFile, map[string]any{"entries": in.Ledger})
	write(SnapshotsFile, in.Snapshots)
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"orphan", "--dir", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var s OrphanSummary
	b, err := os.ReadFile(filepath.Join(dir, SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Before.Holds || s.InFlight.Count != 2 || !strings.Contains(stderr.String(), "in flight on switchboard-2: 2") {
		t.Errorf("summary %+v, stderr %q", s.Before, stderr.String())
	}
}

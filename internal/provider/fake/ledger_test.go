package fake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
)

func TestLedger(t *testing.T) {
	t.Parallel()
	p := New(Config{Tokens: 5})
	ctx := context.Background()
	if _, err := p.Complete(WithCaller(ctx, "10.0.0.2"), request("one two three", 0)); err != nil {
		t.Fatal(err)
	}
	full, err := p.Stream(ctx, request("four five", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Collect(full); err != nil {
		t.Fatal(err)
	}
	// A stream closed after its start event and two deltas.
	cut, err := p.Stream(ctx, request("six", 0))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := cut.Next(); err != nil {
			t.Fatal(err)
		}
	}
	_ = cut.Close()

	got := p.Ledger()
	want := []Entry{
		// The estimate is the gateway's: 4 per message plus a token per
		// three characters, rounded up.
		{Model: "fake", InputTokens: 3, OutputTokens: 5, Completed: true, Caller: "10.0.0.2", EstimatedInputTokens: 9},
		{Model: "fake", InputTokens: 2, OutputTokens: 5, Completed: true, MaxTokens: 64, EstimatedInputTokens: 7},
		{Model: "fake", InputTokens: 1, OutputTokens: 2, EstimatedInputTokens: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("ledger has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		g := got[i]
		if g.Time.IsZero() {
			t.Errorf("entry %d has no time", i)
		}
		if g.Ended.Before(g.Time) {
			t.Errorf("entry %d ended %v, before it arrived at %v", i, g.Ended, g.Time)
		}
		g.Time, g.Ended, g.Stream = want[i].Time, want[i].Ended, false
		if g != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, g, want[i])
		}
	}

	srv := httptest.NewServer(p.LedgerHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ledger")
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Entries []Entry }
	err = json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	if err != nil || len(body.Entries) != 3 {
		t.Fatalf("GET /ledger: %d entries, %v", len(body.Entries), err)
	}
	resp, err = http.Post(srv.URL+"/ledger/reset", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(p.Ledger()) != 0 {
		t.Errorf("reset: status %d, %d entries left", resp.StatusCode, len(p.Ledger()))
	}
}

// A request still open when the ledger is reset does not mark the new
// ledger's entry with the same index as ended.
func TestEndAfterReset(t *testing.T) {
	t.Parallel()
	p := New(Config{Tokens: 2})
	ctx := context.Background()
	old, err := p.Stream(ctx, request("one", 0))
	if err != nil {
		t.Fatal(err)
	}
	p.ResetLedger()
	open, err := p.Stream(ctx, request("two", 0))
	if err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	if e := p.Ledger()[0]; !e.Ended.IsZero() {
		t.Errorf("open request marked ended at %v by one from before the reset", e.Ended)
	}
	_ = open.Close()
	if e := p.Ledger()[0]; e.Ended.IsZero() {
		t.Error("closed request not marked ended")
	}
}

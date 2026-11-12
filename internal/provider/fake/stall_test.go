package fake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

func TestStallHoldsRequestsUntilTheCallerLeaves(t *testing.T) {
	t.Parallel()
	p := New(Config{Tokens: 3})
	p.SetStalled(true)

	ctx, cancel := context.WithCancel(context.Background())
	s, err := p.Stream(ctx, request("held stream", 0))
	if err != nil {
		t.Fatal(err)
	}
	completeErr := make(chan error, 1)
	go func() {
		_, err := p.Complete(ctx, request("held complete", 0))
		completeErr <- err
	}()
	nextErr := make(chan error, 1)
	go func() {
		_, err := s.Next()
		nextErr <- err
	}()
	waitHeld(t, p, 2)

	// Ending the stall does not answer what it already holds.
	p.SetStalled(false)
	select {
	case err := <-nextErr:
		t.Fatalf("a held stream answered after the stall ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// A new request is answered.
	if _, err := p.Complete(context.Background(), request("after", 0)); err != nil {
		t.Fatalf("after the stall: %v", err)
	}

	cancel()
	for _, ch := range []chan error{nextErr, completeErr} {
		var e *core.Error
		if err := <-ch; !errors.As(err, &e) || e.Kind != core.KindTimeout {
			t.Errorf("held request ended with %v, want the caller's cancellation", err)
		}
	}
	waitHeld(t, p, 0)

	ledger := p.Ledger()
	if len(ledger) != 3 || !ledger[0].Stalled || !ledger[1].Stalled || ledger[2].Stalled || ledger[0].Completed {
		t.Errorf("ledger = %+v, want two stalled entries then one answered", ledger)
	}
}

func waitHeld(t *testing.T, p *Provider, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.Held() != n {
		if time.Now().After(deadline) {
			t.Fatalf("held = %d, want %d", p.Held(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStallHandler(t *testing.T) {
	t.Parallel()
	p := New(Config{Tokens: 3})
	srv := httptest.NewServer(p.StallHandler())
	defer srv.Close()
	for _, path := range []string{"/stall/on", "/stall/on", "/stall/off"} {
		resp, err := http.Post(srv.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST %s: %d", path, resp.StatusCode)
		}
	}
	resp, err := http.Get(srv.URL + "/stall")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Stalled     bool         `json:"stalled"`
		Held        int64        `json:"held"`
		Transitions []Transition `json:"transitions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// The repeated "on" is not a transition.
	if body.Stalled || len(body.Transitions) != 2 || !body.Transitions[0].Stalled || body.Transitions[1].Stalled ||
		body.Transitions[1].Time.Before(body.Transitions[0].Time) {
		t.Errorf("stall = %+v, want on then off", body)
	}
}

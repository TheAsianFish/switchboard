package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/admin"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/hedge"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/provider/openaicompat"
	"github.com/TheAsianFish/switchboard/internal/server"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

func TestResilienceConfig(t *testing.T) {
	t.Parallel()
	zero, budget := 0, 0.25
	got := resilienceConfig(config.Resilience{
		Retries:           &zero,
		FirstTokenTimeout: 2 * time.Second,
		Hedge:             config.Hedge{Enabled: true, Window: 50, Budget: &budget},
	})
	if got.Retries != 0 || got.Breaker.Failures != hedge.DefaultConfig.Breaker.Failures {
		t.Errorf("retries %d, breaker %d", got.Retries, got.Breaker.Failures)
	}
	if got.FirstTokenTimeout != 2*time.Second {
		t.Errorf("first-token timeout = %v", got.FirstTokenTimeout)
	}
	if !got.Hedge.Enabled || got.Hedge.Budget != 0.25 || got.Hedge.Window != 50 || got.Hedge.MinSamples != 50 {
		t.Errorf("hedge = %+v", got.Hedge)
	}
	if def := resilienceConfig(config.Resilience{}); def.Hedge.Enabled || def.FirstTokenTimeout != 0 || def.Retries != 2 || def.BackoffMax != 2*time.Second {
		t.Errorf("defaults = %+v", def)
	}
}

func TestAdminReportsProviderHealth(t *testing.T) {
	t.Parallel()
	h := hedge.New("up", fake.New(fake.DefaultConfig), hedge.DefaultConfig)
	srv := httptest.NewServer(adminHandler(telemetry.NewMetrics(false), []*hedge.Provider{h}, admin.NewServer(admin.Options{Recorder: admin.NewRecorder(admin.RecorderOptions{})})))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Providers []hedge.Health `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, err %v", resp.StatusCode, err)
	}
	if len(body.Providers) != 1 || body.Providers[0].Provider != "up" || body.Providers[0].Breaker != "closed" {
		t.Fatalf("providers = %+v", body.Providers)
	}
}

// A stalled fake-provider process accepts the request and sends nothing,
// not even headers; the first-token timeout fails the attempt across real
// HTTP, and the provider's whole-call timeout never gets a say.
func TestFirstTokenTimeoutAgainstAStalledFakeProvider(t *testing.T) {
	t.Parallel()
	fp := fake.New(fake.DefaultConfig)
	fp.SetStalled(true)
	upstream := httptest.NewServer(server.Handler(pipeline.Open{Provider: fp, Names: []string{"fake"}}, slog.New(slog.DiscardHandler)))
	defer upstream.Close()
	prov, err := openaicompat.New(openaicompat.Config{BaseURL: upstream.URL + "/v1", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	cfg := resilienceConfig(config.Resilience{Retries: &zero, FirstTokenTimeout: 100 * time.Millisecond})
	h := hedge.New("fake", prov, cfg)
	start := time.Now()
	_, err = h.Stream(context.Background(), &core.Request{Model: "fake", Stream: true,
		Messages: []core.Message{{Role: core.RoleUser, Text: "hello"}}})
	if !errors.Is(err, hedge.ErrFirstToken) {
		t.Fatalf("err = %v, want a first-token timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("failed after %v", elapsed)
	}
	// The held connection is released once the gateway gives up.
	deadline := time.Now().Add(2 * time.Second)
	for fp.Held() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("fake provider still holds %d requests", fp.Held())
		}
		time.Sleep(5 * time.Millisecond)
	}
	fp.SetStalled(false)
	s, err := h.Stream(context.Background(), &core.Request{Model: "fake", Stream: true,
		Messages: []core.Message{{Role: core.RoleUser, Text: "hello"}}})
	if err != nil {
		t.Fatalf("after the stall: %v", err)
	}
	if _, err := core.Collect(s); err != nil {
		t.Fatalf("after the stall: %v", err)
	}
}

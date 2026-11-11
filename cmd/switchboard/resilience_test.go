package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/admin"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/hedge"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

func TestResilienceConfig(t *testing.T) {
	t.Parallel()
	zero, budget := 0, 0.25
	got := resilienceConfig(config.Resilience{
		Retries: &zero,
		Hedge:   config.Hedge{Enabled: true, Window: 50, Budget: &budget},
	})
	if got.Retries != 0 || got.Breaker.Failures != hedge.DefaultConfig.Breaker.Failures {
		t.Errorf("retries %d, breaker %d", got.Retries, got.Breaker.Failures)
	}
	if !got.Hedge.Enabled || got.Hedge.Budget != 0.25 || got.Hedge.Window != 50 || got.Hedge.MinSamples != 50 {
		t.Errorf("hedge = %+v", got.Hedge)
	}
	if def := resilienceConfig(config.Resilience{}); def.Hedge.Enabled || def.Retries != 2 || def.BackoffMax != 2*time.Second {
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

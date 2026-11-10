package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/router"
)

func writeWeights(t *testing.T) string {
	t.Helper()
	d := len(router.CheapFeatureNames)
	sc := router.Scaler{Mean: make([]float64, d), Std: slices.Repeat([]float64{1}, d)}
	w := &router.Weights{
		Version: router.WeightsVersion, Features: slices.Clone(router.CheapFeatureNames), Scaler: sc,
		CostUnit: router.CostGFLOPs, Fallback: "big", QualityTarget: 0.6,
		Candidates: []router.Candidate{
			{Model: "small", Cost: 1, Accuracy: 0.4, Logistic: router.Logistic{Weights: make([]float64, d)}},
			{Model: "big", Cost: 9, Accuracy: 0.8, Logistic: router.Logistic{Weights: make([]float64, d)}},
		},
	}
	b, err := w.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "weights.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildRouter(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := writeWeights(t)
	models := []config.Model{{Name: "small"}, {Name: "big"}}

	if r, err := buildRouter(&config.Config{Models: models}, log); r != nil || err != nil {
		t.Errorf("router off: %v, %v; want nil, nil", r, err)
	}
	target := 0.9
	cfg := &config.Config{Models: models, Router: config.Router{Enabled: true, Weights: path, QualityTarget: &target, EmbedURL: "http://x", Timeout: 1}}
	r, err := buildRouter(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if r.Target() != 0.9 {
		t.Errorf("target %v, want the configured 0.9 over the file's 0.6", r.Target())
	}
	cfg.Router.QualityTarget = nil
	if r, _ := buildRouter(cfg, log); r.Target() != 0.6 {
		t.Errorf("target %v, want the file's 0.6", r.Target())
	}

	cfg.Models = models[:1]
	if _, err := buildRouter(cfg, log); err == nil || !strings.Contains(err.Error(), "big is not configured") {
		t.Errorf("a missing candidate: %v", err)
	}
	cfg.Models = models
	cfg.Router.Weights = filepath.Join(t.TempDir(), "missing.json")
	if _, err := buildRouter(cfg, log); err == nil {
		t.Error("a missing weights file was accepted")
	}
}

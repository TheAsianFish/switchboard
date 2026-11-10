package main

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/router"
)

// buildRouter loads the router's weights and checks that every model they
// may choose is configured, so a gateway that cannot serve auto refuses to
// start rather than failing auto requests. It returns nil when the router
// is off.
func buildRouter(cfg *config.Config, log *slog.Logger) (*router.Router, error) {
	rc := cfg.Router
	if !rc.Enabled {
		return nil, nil
	}
	if rc.Weights == "" {
		return nil, fmt.Errorf("router.weights is required: the path of a weights file from make eval-router")
	}
	w, err := router.Load(rc.Weights)
	if err != nil {
		return nil, fmt.Errorf("router.weights: %w", err)
	}
	configured := map[string]bool{}
	for _, m := range cfg.Models {
		configured[m.Name] = true
	}
	var missing, names []string
	for _, c := range w.Candidates {
		names = append(names, c.Model)
		if !configured[c.Model] {
			missing = append(missing, c.Model)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("router: the weights choose between %s, but %s %s not configured under models",
			strings.Join(names, ", "), strings.Join(missing, ", "), plural(len(missing), "is", "are"))
	}
	var e embed.Embedder
	if w.Embedding != "" {
		e = &embed.Ollama{BaseURL: rc.EmbedURL, Name: w.Embedding, Timeout: rc.Timeout}
	}
	target := -1.0
	if rc.QualityTarget != nil {
		target = *rc.QualityTarget
	}
	r, err := router.New(w, e, router.Options{Target: target, Timeout: rc.Timeout})
	if err != nil {
		return nil, err
	}
	log.Info("router on", slog.String("weights", rc.Weights), slog.String("candidates", strings.Join(names, ",")),
		slog.String("fallback", w.Fallback), slog.Float64("quality_target", r.Target()),
		slog.String("embedding", w.Embedding), slog.String("embed_url", rc.EmbedURL))
	return r, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

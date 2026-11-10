package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/router"
)

// buildRouter loads the router's weights and checks that every model they
// may choose is configured and that every key allowed auto may use one of
// them, so a gateway that cannot serve auto refuses to start rather than
// failing auto requests. It returns nil when the router
// is off.
func buildRouter(cfg *config.Config, log *slog.Logger) (*router.Router, error) {
	rc := cfg.Router
	if !rc.Enabled {
		return nil, nil
	}
	w, err := router.Default()
	source := "built in"
	if rc.Weights != "" {
		w, err = router.Load(rc.Weights)
		source = rc.Weights
	}
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
	// A key that lists auto but none of its candidates would be refused
	// on every auto request.
	for _, k := range cfg.Keys {
		if !slices.Contains(k.Models, config.AutoModel) {
			continue
		}
		if !slices.ContainsFunc(names, func(m string) bool { return slices.Contains(k.Models, m) }) {
			return nil, fmt.Errorf("key %q may use auto but none of the models it chooses between (%s); list them in the key's models",
				k.Name, strings.Join(names, ", "))
		}
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
	log.Info("router on", slog.String("weights", source), slog.String("candidates", strings.Join(names, ",")),
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

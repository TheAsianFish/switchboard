package config

import (
	"fmt"
	"math"
	"time"
)

// AutoModel is the model name that asks the router to choose.
const AutoModel = "auto"

// Router configures the model router behind model "auto".
type Router struct {
	// Enabled adds model "auto" to the gateway. Off by default: routing
	// changes which model answers, so an operator turns it on.
	Enabled bool `yaml:"enabled"`
	// Weights is the path of a weights file written by `make eval-router`;
	// empty means the weights built into the binary.
	Weights string `yaml:"weights"`
	// QualityTarget is the predicted chance of a correct answer a
	// candidate must reach to be chosen; unset means the weights' own
	// target, the one the eval chose. Higher sends more requests to the
	// dearer candidates.
	QualityTarget *float64 `yaml:"quality_target"`
	// EmbedURL is the Ollama server that embeds prompts for the router,
	// without /v1. The weights name the embedding model.
	EmbedURL string `yaml:"embed_url"`
	// Timeout bounds the embedding call; a slower one sends the request
	// to the router's fallback model instead of holding it up.
	Timeout time.Duration `yaml:"timeout"`
}

// DefaultRouterTimeout bounds the router's embedding call. One short
// prompt embeds in tens of milliseconds on a laptop CPU.
const DefaultRouterTimeout = time.Second

func defaultRouter() Router {
	return Router{EmbedURL: DefaultEmbedURL, Timeout: DefaultRouterTimeout}
}

func (c *Config) validateRouter() []error {
	r := c.Router
	var errs []error
	if r.QualityTarget != nil && (math.IsNaN(*r.QualityTarget) || *r.QualityTarget < 0 || *r.QualityTarget > 1) {
		errs = append(errs, fmt.Errorf("router.quality_target must be between 0 and 1, got %g", *r.QualityTarget))
	}
	if r.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("router.timeout must be positive, got %v", r.Timeout))
	}
	if !r.Enabled {
		return errs
	}
	if r.EmbedURL == "" {
		errs = append(errs, fmt.Errorf("router.embed_url is required when the router is enabled"))
	}
	for _, m := range c.Models {
		if m.Name == AutoModel {
			errs = append(errs, fmt.Errorf("model %q: the name is taken by the router while router.enabled is true", AutoModel))
		}
	}
	return errs
}

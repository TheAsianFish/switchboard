package config

import (
	"fmt"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cache"
)

// Cache configures the response cache.
type Cache struct {
	// Mode is the default for keys that do not set their own: off, exact
	// or semantic. The default is off: a cache changes what clients get
	// back, so it is something an operator turns on.
	Mode string `yaml:"mode"`
	// TTL is how long a reply stays cached.
	TTL time.Duration `yaml:"ttl"`
	// MaxTemperature is the highest temperature a request may ask for
	// and still be cached without the X-Switchboard-Cache header.
	MaxTemperature float64 `yaml:"max_temperature"`
	// MaxEntries caps the per-process store used without Redis.
	MaxEntries int `yaml:"max_entries"`
	// Timeout bounds each cache store and embedding call; a slower one
	// is treated as a miss.
	Timeout  time.Duration `yaml:"timeout"`
	Semantic Semantic      `yaml:"semantic"`
}

// Semantic configures the semantic layer.
type Semantic struct {
	// EmbedURL is the Ollama server that embeds prompts, without /v1.
	EmbedURL string `yaml:"embed_url"`
	// Model is the embedding model.
	Model string `yaml:"model"`
	// Threshold is the lowest cosine similarity served as a hit.
	Threshold float64 `yaml:"threshold"`
	// Index is the Redis vector index algorithm: flat or hnsw.
	Index string `yaml:"index"`
	// MatchNumbers refuses a semantic hit unless both prompts contain
	// the same numbers.
	MatchNumbers *bool `yaml:"match_numbers"`
}

// Cache defaults. The semantic defaults are the ones `make eval-cache`
// chose (eval/results/q3-cache/summary.json, RESULTS.md Q3); a test
// keeps them in step with that file.
const (
	DefaultCacheTTL        = 24 * time.Hour
	DefaultCacheMaxEntries = 10000
	DefaultCacheTimeout    = 250 * time.Millisecond
	DefaultEmbedURL        = "http://localhost:11434"
	DefaultEmbedModel      = "nomic-embed-text"
	DefaultCacheThreshold  = 0.9989
	// FLAT, not HNSW: every gateway lookup is filtered to one context,
	// where both are equally fast and FLAT is exact, builds without
	// background work and is smaller (RESULTS.md, Q3).
	DefaultCacheIndex = "flat"
)

func defaultCache() Cache {
	match := true
	return Cache{
		Mode: "off", TTL: DefaultCacheTTL, MaxEntries: DefaultCacheMaxEntries, Timeout: DefaultCacheTimeout,
		Semantic: Semantic{EmbedURL: DefaultEmbedURL, Model: DefaultEmbedModel, Threshold: DefaultCacheThreshold, Index: DefaultCacheIndex, MatchNumbers: &match},
	}
}

func (c *Config) validateCache() []error {
	var errs []error
	if _, err := cache.ParseMode(c.Cache.Mode); err != nil {
		errs = append(errs, fmt.Errorf("cache.mode: %w", err))
	}
	if c.Cache.TTL <= 0 {
		errs = append(errs, fmt.Errorf("cache.ttl must be positive, got %v", c.Cache.TTL))
	}
	if c.Cache.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("cache.timeout must be positive, got %v", c.Cache.Timeout))
	}
	if c.Cache.MaxEntries <= 0 {
		errs = append(errs, fmt.Errorf("cache.max_entries must be positive, got %d", c.Cache.MaxEntries))
	}
	if c.Cache.MaxTemperature < 0 {
		errs = append(errs, fmt.Errorf("cache.max_temperature must not be negative, got %g", c.Cache.MaxTemperature))
	}
	s := c.Cache.Semantic
	if s.Threshold <= 0 || s.Threshold > 1 {
		errs = append(errs, fmt.Errorf("cache.semantic.threshold must be in (0, 1], got %g", s.Threshold))
	}
	switch s.Index {
	case "flat", "hnsw":
	default:
		errs = append(errs, fmt.Errorf("cache.semantic.index %q is not one of flat, hnsw", s.Index))
	}
	m, _ := cache.ParseMode(c.Cache.Mode)
	semantic := m == cache.ModeSemantic
	for _, k := range c.Keys {
		if k.Cache == "" {
			continue
		}
		km, err := cache.ParseMode(k.Cache)
		if err != nil {
			errs = append(errs, fmt.Errorf("key %q: cache: %w", k.Name, err))
		}
		semantic = semantic || km == cache.ModeSemantic
	}
	if semantic && (s.EmbedURL == "" || s.Model == "") {
		errs = append(errs, fmt.Errorf("cache.semantic needs embed_url and model when any key uses the semantic cache"))
	}
	return errs
}

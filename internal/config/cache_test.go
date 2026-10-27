package config

import (
	"strings"
	"testing"
	"time"
)

const withCache = `
providers: [{name: fake, type: fake}]
models: [{name: fake, provider: fake}]
cache:
  mode: exact
  ttl: 1h
  semantic: {threshold: 0.97, index: hnsw, match_numbers: false}
keys:
  - {name: a, key: sk-a}
  - {name: b, key: sk-b, tenant: team, cache: semantic}
  - {name: c, key: sk-c, tenant: team, cache: "off"}
`

func TestLoadCache(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeFile(t, withCache), env(map[string]string{"SWITCHBOARD_CACHE_EMBED_URL": "http://ollama:11434"}))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Cache
	if c.Mode != "exact" || c.TTL != time.Hour || c.Timeout != DefaultCacheTimeout || c.MaxEntries != DefaultCacheMaxEntries {
		t.Errorf("cache = %+v", c)
	}
	s := c.Semantic
	if s.Threshold != 0.97 || s.Index != "hnsw" || s.MatchNumbers == nil || *s.MatchNumbers || s.Model != DefaultEmbedModel || s.EmbedURL != "http://ollama:11434" {
		t.Errorf("semantic = %+v", s)
	}
	// A key without its own mode takes cache.mode.
	for i, want := range []string{"exact", "semantic", "off"} {
		if got := cfg.Keys[i].Cache; got != want {
			t.Errorf("key %s cache = %q, want %q", cfg.Keys[i].Name, got, want)
		}
	}
	if cfg.Keys[1].Tenant != "team" {
		t.Errorf("tenant = %q", cfg.Keys[1].Tenant)
	}
}

func TestCacheDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load("", env(map[string]string{"SWITCHBOARD_CACHE_MODE": "exact"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Mode != "exact" || cfg.Cache.Semantic.MatchNumbers == nil || !*cfg.Cache.Semantic.MatchNumbers {
		t.Errorf("cache = %+v", cfg.Cache)
	}
	off, _ := Load("", env(nil))
	if off.Cache.Mode != "off" {
		t.Errorf("default mode = %q, want off", off.Cache.Mode)
	}
}

func TestValidateCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body, want string
	}{
		{"bad mode", "cache: {mode: always}", "cache.mode"},
		{"bad key mode", "keys: [{name: k, key: sk, cache: maybe}]", `key "k": cache`},
		{"threshold above 1", "cache: {semantic: {threshold: 1.5}}", "threshold must be in (0, 1]"},
		{"threshold zero", "cache: {semantic: {threshold: 0}}", "threshold must be in (0, 1]"},
		{"bad index", "cache: {semantic: {index: ivf}}", "cache.semantic.index"},
		{"no ttl", "cache: {ttl: 0s}", "cache.ttl"},
		{"negative temperature", "cache: {max_temperature: -1}", "max_temperature"},
		{"semantic without a model", "cache: {mode: semantic, semantic: {model: \"\"}}", "needs embed_url and model"},
		{"capitalised semantic without a model", "cache: {semantic: {model: \"\"}}\nkeys: [{name: k, key: sk, cache: Semantic}]", "needs embed_url and model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeFile(t, tt.body), env(nil))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

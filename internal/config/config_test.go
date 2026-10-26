package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "switchboard.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const valid = `
listen: 0.0.0.0:8080
log: {format: text, level: debug}
providers:
  - name: ollama
    type: openaicompat
    base_url: http://localhost:11434/v1
  - name: anthropic
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
    timeout: 30s
models:
  - {name: qwen, provider: ollama, upstream: "qwen2.5:0.5b"}
  - {name: claude-haiku-5-5, provider: anthropic}
keys:
  - {name: dev, key: sk-dev}
  - {name: team, sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef, models: [qwen]}
`

func TestLoad(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeFile(t, valid), env(map[string]string{
		"ANTHROPIC_API_KEY":                    "secret",
		"SWITCHBOARD_LISTEN":                   "127.0.0.1:9999",
		"SWITCHBOARD_PROVIDER_OLLAMA_BASE_URL": "http://ollama:11434/v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q, want the env override", cfg.Listen)
	}
	if cfg.Log.Format != "text" || cfg.Log.Level != "debug" {
		t.Errorf("log = %+v", cfg.Log)
	}
	if got := cfg.Providers[0].BaseURL; got != "http://ollama:11434/v1" {
		t.Errorf("ollama base_url = %q", got)
	}
	if cfg.Providers[0].Timeout != DefaultTimeout || cfg.Providers[1].Timeout != 30*time.Second {
		t.Errorf("timeouts = %v, %v", cfg.Providers[0].Timeout, cfg.Providers[1].Timeout)
	}
	if cfg.Providers[1].APIKey != "secret" {
		t.Error("api key not resolved from the environment")
	}
	if cfg.Models[0].Upstream != "qwen2.5:0.5b" || cfg.Models[1].Upstream != "claude-haiku-5-5" {
		t.Errorf("upstreams = %q, %q", cfg.Models[0].Upstream, cfg.Models[1].Upstream)
	}
}

func TestLoadEmpty(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", writeFile(t, "")} {
		cfg, err := Load(path, env(nil))
		if err != nil {
			t.Fatalf("Load(%q): %v", path, err)
		}
		if cfg.Listen != DefaultListen || cfg.Log.Format != "json" {
			t.Errorf("defaults = %+v", cfg)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"unknown field", "listne: x\n", []string{"field listne not found"}},
		{"secret in file", "providers: [{name: a, type: anthropic, api_key: sk}]\n", []string{"field api_key not found"}},
		{"bad references", `
providers:
  - {name: p, type: nope}
  - {name: p, type: openaicompat}
models:
  - {name: m, provider: missing}
keys:
  - {name: k, key: a, sha256: b}
  - {name: k2, sha256: xyz, models: [ghost]}
`, []string{
			`type "nope"`, `duplicate name "p"`, "base_url is required",
			`unknown provider "missing"`, "set exactly one of key and sha256",
			"64 hex characters", `unknown model "ghost"`,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeFile(t, tt.body), env(nil))
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestEnvName(t *testing.T) {
	t.Parallel()
	if got := envName("my-ollama.2"); got != "MY_OLLAMA_2" {
		t.Errorf("envName = %q", got)
	}
}

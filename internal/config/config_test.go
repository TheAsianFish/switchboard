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

// TestShippedImageConfig guards the config baked into the container image:
// it listens on every interface and its plain-text key is published in the
// README, so that key must not reach a paid provider.
func TestShippedImageConfig(t *testing.T) {
	t.Parallel()
	cfg, err := Load("../../deploy/switchboard.yaml", env(map[string]string{"ANTHROPIC_API_KEY": "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	providerType := map[string]string{}
	for _, p := range cfg.Providers {
		providerType[p.Name] = p.Type
	}
	modelProvider := map[string]string{}
	for _, m := range cfg.Models {
		modelProvider[m.Name] = m.Provider
	}
	for _, k := range cfg.Keys {
		if k.Key == "" {
			continue
		}
		if len(k.Models) == 0 {
			t.Errorf("plain key %q may use every model, Claude included", k.Name)
		}
		for _, m := range k.Models {
			if providerType[modelProvider[m]] == "anthropic" {
				t.Errorf("plain key %q may use %q, a paid model", k.Name, m)
			}
		}
	}
}

const shared = `
redis: {url: "redis://redis:6379/0", timeout: 50ms}
ratelimit: {algorithm: fixed_window, replicas: 3}
budget: {grace_usd: 0.25}
providers:
  - {name: fake, type: fake}
  - {name: anthropic, type: anthropic}
models:
  - {name: fake, provider: fake, price: {input: 3, output: "15.00", long_above: 1000, long: {input: 6, output: 30}}}
  - {name: claude-haiku-5-5, provider: anthropic}
  - {name: local, provider: fake}
keys:
  - name: team
    key: sk-team
    models: [fake, claude-haiku-5-5]
    limits:
      requests: {rate: 100, per: 1m, burst: 20}
      tokens: {rate: 100000, per: 1m, burst: 20000}
    budget: {usd: 5.00, period: monthly}
`

func TestLoadShared(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeFile(t, shared), env(map[string]string{"SWITCHBOARD_RATELIMIT_REPLICAS": "5"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.URL != "redis://redis:6379/0" || cfg.Redis.Timeout != 50*time.Millisecond || cfg.Redis.KeyPrefix != DefaultKeyPrefix {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	if cfg.RateLimit.Replicas != 5 || cfg.RateLimit.RetryInterval != DefaultRetryInterval {
		t.Errorf("ratelimit = %+v", cfg.RateLimit)
	}
	if cfg.Budget.GraceUSD != "0.25" || cfg.AdminListen != DefaultAdminListen {
		t.Errorf("budget = %+v, admin %q", cfg.Budget, cfg.AdminListen)
	}
	k := cfg.Keys[0]
	if k.Limits.Requests == nil || *k.Limits.Requests != (Rate{Rate: 100, Per: time.Minute, Burst: 20}) {
		t.Errorf("requests limit = %+v", k.Limits.Requests)
	}
	if k.Budget == nil || k.Budget.USD != "5.00" || k.Budget.Period != "monthly" {
		t.Errorf("budget = %+v", k.Budget)
	}
	p, ok, err := cfg.Models[0].ResolvedPrice()
	if err != nil || !ok || p.Base.Input != 3000 || p.Base.Output != 15000 || p.LongAbove != 1000 || p.Long.Output != 30000 {
		t.Errorf("fake price = %+v, %v, %v", p, ok, err)
	}
	if p, ok, _ := cfg.Models[1].ResolvedPrice(); !ok || p.Base.Input != 100 {
		t.Errorf("haiku default price = %+v, %v", p, ok)
	}
	if _, ok, _ := cfg.Models[2].ResolvedPrice(); ok {
		t.Error("a model with no price and no list price should be unpriced")
	}
	if cfg.Models[0].MaxOutputTokens != DefaultOutputEstimate {
		t.Errorf("max_output_tokens default = %d", cfg.Models[0].MaxOutputTokens)
	}
}

func TestValidateShared(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, replace, with, want string
	}{
		{"algorithm", "algorithm: fixed_window", "algorithm: leaky", `"leaky" is not one of`},
		{"replicas", "replicas: 3", "replicas: 0", "replicas must be at least 1"},
		{"redis timeout", "timeout: 50ms", "timeout: -1s", "redis.timeout must be positive"},
		{"grace", "grace_usd: 0.25", "grace_usd: -1", "budget.grace_usd"},
		{"hold outlives the call", "grace_usd: 0.25", "grace_usd: 0.25, reservation_ttl: 5m", `must be longer than provider "fake"'s timeout (10m0s)`},
		{"limit", "rate: 100, per: 1m", "rate: 0, per: 1m", "limits.requests needs a positive"},
		{"too fine", "rate: 100000, per: 1m", "rate: 100000000, per: 1m", "faster than one unit per microsecond"},
		{"budget usd", "usd: 5.00", "usd: 5.0000000001", "budget.usd"},
		{"period", "period: monthly", "period: weekly", `"weekly" is not one of`},
		{"price", `output: "15.00"`, `output: "15.0001"`, "price output"},
		{"long half", "long_above: 1000, ", "", "long_above and long must be set together"},
		{"unpriced reach", "models: [fake, claude-haiku-5-5]", "models: [fake, local]", `may use model "local", which has no price`},
		{"unpriced by default", "    models: [fake, claude-haiku-5-5]\n", "", `may use model "local"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(shared, tt.replace, tt.with, 1)
			if body == shared {
				t.Fatalf("replacement %q not found", tt.replace)
			}
			_, err := Load(writeFile(t, body), env(nil))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestGraceRefill(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, budget string
		want         time.Duration
		err          string
	}{
		{name: "default", budget: "budget: {grace_usd: 0.25}", want: time.Hour},
		{name: "set", budget: "budget: {grace_usd: 0.25, grace_refill: 30m}", want: 30 * time.Minute},
		{name: "zero", budget: "budget: {grace_usd: 0.25, grace_refill: 0s}", err: "budget.grace_refill must be positive, got 0s"},
		{name: "negative", budget: "budget: {grace_usd: 0.25, grace_refill: -1h}", err: "budget.grace_refill must be positive, got -1h0m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(shared, "budget: {grace_usd: 0.25}", tt.budget, 1)
			cfg, err := Load(writeFile(t, body), env(nil))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Budget.GraceRefill != tt.want {
				t.Errorf("grace_refill = %v, want %v", cfg.Budget.GraceRefill, tt.want)
			}
		})
	}
}

func TestReplicasFromEnvMustParse(t *testing.T) {
	t.Parallel()
	_, err := Load(writeFile(t, shared), env(map[string]string{"SWITCHBOARD_RATELIMIT_REPLICAS": "three"}))
	if err == nil || !strings.Contains(err.Error(), "SWITCHBOARD_RATELIMIT_REPLICAS") {
		t.Errorf("err = %v, want it to name SWITCHBOARD_RATELIMIT_REPLICAS", err)
	}
}

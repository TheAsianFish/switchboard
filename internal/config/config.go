// Package config loads switchboard.yaml and applies SWITCHBOARD_*
// environment overrides. Provider secrets are never read from the file:
// the file names the environment variable that holds each one.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider types.
const (
	TypeAnthropic    = "anthropic"
	TypeOpenAICompat = "openaicompat"
	TypeFake         = "fake"
)

// Config is the whole gateway configuration.
type Config struct {
	Listen    string     `yaml:"listen"`
	Log       Log        `yaml:"log"`
	Providers []Provider `yaml:"providers"`
	Models    []Model    `yaml:"models"`
	Keys      []Key      `yaml:"keys"`
	// AdminListen serves /metrics, the admin API and its page. It is a
	// separate listener so per-key names and spend never reach the public
	// port; the default is loopback only.
	AdminListen string       `yaml:"admin_listen"`
	Admin       Admin        `yaml:"admin"`
	Redis       Redis        `yaml:"redis"`
	RateLimit   RateLimit    `yaml:"ratelimit"`
	Budget      BudgetPolicy `yaml:"budget"`
	Cache       Cache        `yaml:"cache"`
	Tracing     Tracing      `yaml:"tracing"`
	Router      Router       `yaml:"router"`
}

// Admin configures the admin API and page on the admin listener.
type Admin struct {
	// ReplicaID names this process in the fleet view; the default is the
	// host name. Env SWITCHBOARD_REPLICA_ID.
	ReplicaID string `yaml:"replica_id"`
	// TraceURL is a link template for a request's trace, with
	// "{trace_id}" replaced, e.g. http://localhost:16686/trace/{trace_id}.
	// Empty shows trace ids without links. Env SWITCHBOARD_ADMIN_TRACE_URL.
	TraceURL string `yaml:"trace_url"`
	// AllowedHosts are Host header names the API and page answer besides
	// loopback names and IP literals, for an admin listener reached
	// through a proxy or a container name.
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// Log configures the process logger.
type Log struct {
	Format string `yaml:"format"` // json or text
	Level  string `yaml:"level"`  // debug, info, warn, error
}

// Provider is one upstream backend.
type Provider struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	// BaseURL is the upstream's base URL. For openaicompat it includes the
	// /v1 prefix; for anthropic it is empty for api.anthropic.com.
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the environment variable holding the upstream key.
	APIKeyEnv string `yaml:"api_key_env"`
	// Timeout bounds one upstream call, stream included.
	Timeout time.Duration `yaml:"timeout"`
	// DefaultMaxTokens is sent when a request has none and the upstream
	// requires one (Anthropic).
	DefaultMaxTokens int `yaml:"default_max_tokens"`
	// Fake configures a fake provider.
	Fake FakeProvider `yaml:"fake"`
	// Resilience sets retries, hedging and the circuit breaker; unset
	// fields keep the defaults (two retries, breaker after five failures,
	// no hedging).
	Resilience Resilience `yaml:"resilience"`

	// APIKey is resolved from APIKeyEnv at load time; never from the file.
	APIKey string `yaml:"-"`
}

// Resilience is a provider's retry, hedge and breaker policy. Zero values
// mean the default; the pointer fields distinguish an explicit 0.
type Resilience struct {
	// Retries is how many times a failed request is tried again (default
	// 2; 0 disables retries).
	Retries       *int          `yaml:"retries"`
	BackoffBase   time.Duration `yaml:"backoff_base"`
	BackoffMax    time.Duration `yaml:"backoff_max"`
	MaxRetryAfter time.Duration `yaml:"max_retry_after"`
	// FirstTokenTimeout fails a streamed attempt that has sent no first
	// token after this long, as a timeout the breaker counts and the
	// retries repeat (default 0: off). A cold local model loading its
	// weights legitimately takes longer than any useful value.
	FirstTokenTimeout time.Duration `yaml:"first_token_timeout"`
	Breaker           Breaker       `yaml:"breaker"`
	Hedge             Hedge         `yaml:"hedge"`
}

// Breaker configures a provider's circuit breaker.
type Breaker struct {
	// Failures in a row that open it (default 5; 0 disables it).
	Failures *int          `yaml:"failures"`
	Cooldown time.Duration `yaml:"cooldown"`
}

// Hedge configures hedged requests: a second attempt sent when the first
// has produced no token after Delay, or after the rolling Quantile of
// recent first-token times when Delay is zero.
type Hedge struct {
	Enabled    bool          `yaml:"enabled"`
	Delay      time.Duration `yaml:"delay"`
	Quantile   float64       `yaml:"quantile"`
	Window     int           `yaml:"window"`
	MinSamples int           `yaml:"min_samples"`
	// Budget is the share of each tenant's requests that may be hedged
	// (default 0.1), Burst the hedges a tenant may save up (default 10).
	Budget *float64 `yaml:"budget"`
	Burst  float64  `yaml:"burst"`
}

// FakeProvider shapes the replies of an in-process fake backend.
type FakeProvider struct {
	Latency       time.Duration `yaml:"latency"`
	Tokens        int           `yaml:"tokens"`
	TokenInterval time.Duration `yaml:"token_interval"`
}

// Model maps a client-facing model name to a provider.
type Model struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	// Upstream is the provider's own name for the model; defaults to Name.
	Upstream string `yaml:"upstream"`
	// Price overrides the built-in list price for the upstream name.
	Price *Price `yaml:"price"`
	// MaxOutputTokens is the output a request without max_tokens is
	// assumed to produce when it is admitted and reserved; the default is
	// DefaultOutputEstimate. For a key with a token limit or a budget it
	// is also sent upstream as the request's max_tokens, so the
	// reservation bounds the real cost.
	MaxOutputTokens int `yaml:"max_output_tokens"`
}

// Key is a client API key. Exactly one of Key and SHA256 is set; SHA256 is
// the hex digest of the key, so a deployed file need not hold the key.
type Key struct {
	Name   string `yaml:"name"`
	Key    string `yaml:"key"`
	SHA256 string `yaml:"sha256"`
	// Models restricts the key to these model names; empty allows all.
	Models []string `yaml:"models"`
	// Limits are the key's rate limits; none by default.
	Limits Limits `yaml:"limits"`
	// Budget is the key's dollar allowance; none by default.
	Budget *KeyBudget `yaml:"budget"`
	// Tenant groups keys that share cached replies; it defaults to the
	// key's name, so by default no two keys see each other's entries.
	Tenant string `yaml:"tenant"`
	// Cache overrides cache.mode for this key: off, exact or semantic.
	Cache string `yaml:"cache"`
}

// Defaults applied before the file is read.
const (
	DefaultListen      = "127.0.0.1:8080"
	DefaultAdminListen = "127.0.0.1:9090"
	DefaultTimeout     = 10 * time.Minute
)

// Load reads the file at path (which may be empty for an all-defaults
// config) and applies environment overrides from getenv.
func Load(path string, getenv func(string) string) (*Config, error) {
	cfg := &Config{
		Listen:      DefaultListen,
		AdminListen: DefaultAdminListen,
		Log:         Log{Format: "json", Level: "info"},
		Redis:       Redis{Timeout: DefaultRedisTimeout, KeyPrefix: DefaultKeyPrefix},
		RateLimit:   RateLimit{Algorithm: "fixed_window", Replicas: 1, RetryInterval: DefaultRetryInterval},
		Budget:      BudgetPolicy{GraceUSD: DefaultGraceUSD, GraceRefill: DefaultGraceRefill, ReservationTTL: 15 * time.Minute},
		Cache:       defaultCache(),
		Tracing:     defaultTracing(),
		Router:      defaultRouter(),
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		// An empty file decodes to io.EOF and leaves the defaults.
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := applyEnv(cfg, getenv); err != nil {
		return nil, err
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.Timeout == 0 {
			p.Timeout = DefaultTimeout
		}
		if p.APIKeyEnv != "" {
			p.APIKey = getenv(p.APIKeyEnv)
		}
	}
	for i := range cfg.Models {
		if cfg.Models[i].Upstream == "" {
			cfg.Models[i].Upstream = cfg.Models[i].Name
		}
		if cfg.Models[i].MaxOutputTokens == 0 {
			cfg.Models[i].MaxOutputTokens = DefaultOutputEstimate
		}
	}
	for i := range cfg.Keys {
		if cfg.Keys[i].Cache == "" {
			cfg.Keys[i].Cache = cfg.Cache.Mode
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv applies SWITCHBOARD_* overrides. Per-provider base URLs can be
// overridden as SWITCHBOARD_PROVIDER_<NAME>_BASE_URL, with the name upper
// cased and non-alphanumerics replaced by underscores, which is how compose
// points the same file at a container hostname.
func applyEnv(cfg *Config, getenv func(string) string) error {
	set := func(dst *string, name string) {
		if v := getenv(name); v != "" {
			*dst = v
		}
	}
	set(&cfg.Listen, "SWITCHBOARD_LISTEN")
	set(&cfg.Log.Format, "SWITCHBOARD_LOG_FORMAT")
	set(&cfg.Log.Level, "SWITCHBOARD_LOG_LEVEL")
	set(&cfg.AdminListen, "SWITCHBOARD_ADMIN_LISTEN")
	set(&cfg.Admin.ReplicaID, "SWITCHBOARD_REPLICA_ID")
	set(&cfg.Admin.TraceURL, "SWITCHBOARD_ADMIN_TRACE_URL")
	set(&cfg.Redis.URL, "SWITCHBOARD_REDIS_URL")
	set(&cfg.RateLimit.Algorithm, "SWITCHBOARD_RATELIMIT_ALGORITHM")
	set(&cfg.Cache.Mode, "SWITCHBOARD_CACHE_MODE")
	set(&cfg.Cache.Semantic.EmbedURL, "SWITCHBOARD_CACHE_EMBED_URL")
	set(&cfg.Router.EmbedURL, "SWITCHBOARD_ROUTER_EMBED_URL")
	set(&cfg.Router.Weights, "SWITCHBOARD_ROUTER_WEIGHTS")
	if v := getenv("SWITCHBOARD_RATELIMIT_REPLICAS"); v != "" {
		// A typo must not fall back to the file's count: with too few
		// replicas, each one takes too big a share during an outage.
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("SWITCHBOARD_RATELIMIT_REPLICAS: %q is not a whole number", v)
		}
		cfg.RateLimit.Replicas = n
	}
	if err := applyTracingEnv(&cfg.Tracing, getenv); err != nil {
		return err
	}
	for i := range cfg.Providers {
		set(&cfg.Providers[i].BaseURL, "SWITCHBOARD_PROVIDER_"+envName(cfg.Providers[i].Name)+"_BASE_URL")
	}
	return nil
}

func envName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, s)
}

// Validate checks references and required fields, reporting every problem
// at once.
func (c *Config) Validate() error {
	var errs []error
	providers := map[string]bool{}
	for i, p := range c.Providers {
		switch {
		case p.Name == "":
			errs = append(errs, fmt.Errorf("providers[%d]: name is required", i))
		case providers[p.Name]:
			errs = append(errs, fmt.Errorf("providers[%d]: duplicate name %q", i, p.Name))
		}
		providers[p.Name] = true
		switch p.Type {
		case TypeAnthropic, TypeFake:
		case TypeOpenAICompat:
			if p.BaseURL == "" {
				errs = append(errs, fmt.Errorf("provider %q: base_url is required for openaicompat", p.Name))
			}
		default:
			errs = append(errs, fmt.Errorf("provider %q: type %q is not one of anthropic, openaicompat, fake", p.Name, p.Type))
		}
	}
	models := map[string]bool{}
	byName := map[string]Model{}
	for i, m := range c.Models {
		byName[m.Name] = m
		switch {
		case m.Name == "":
			errs = append(errs, fmt.Errorf("models[%d]: name is required", i))
		case models[m.Name]:
			errs = append(errs, fmt.Errorf("models[%d]: duplicate name %q", i, m.Name))
		case !providers[m.Provider]:
			errs = append(errs, fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider))
		}
		models[m.Name] = true
	}
	keyNames := map[string]bool{}
	for i, k := range c.Keys {
		if k.Name == "" {
			errs = append(errs, fmt.Errorf("keys[%d]: name is required", i))
		} else if keyNames[k.Name] {
			errs = append(errs, fmt.Errorf("keys[%d]: duplicate name %q", i, k.Name))
		}
		keyNames[k.Name] = true
		if (k.Key == "") == (k.SHA256 == "") {
			errs = append(errs, fmt.Errorf("key %q: set exactly one of key and sha256", k.Name))
		}
		if k.SHA256 != "" && !isHex64(k.SHA256) {
			errs = append(errs, fmt.Errorf("key %q: sha256 must be 64 hex characters", k.Name))
		}
		for _, m := range k.Models {
			if !models[m] && (m != AutoModel || !c.Router.Enabled) {
				errs = append(errs, fmt.Errorf("key %q: unknown model %q", k.Name, m))
			}
		}
	}
	for _, p := range c.Providers {
		errs = append(errs, p.Resilience.validate(p.Name)...)
	}
	errs = append(errs, c.validateShared(byName)...)
	errs = append(errs, c.validateCache()...)
	errs = append(errs, c.validateTracing()...)
	errs = append(errs, c.validateRouter()...)
	return errors.Join(errs...)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func (r Resilience) validate(provider string) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("provider %q: resilience: "+format, append([]any{provider}, args...)...))
	}
	if r.Retries != nil && (*r.Retries < 0 || *r.Retries > 10) {
		bad("retries must be between 0 and 10")
	}
	if r.BackoffBase < 0 || r.BackoffMax < 0 || r.MaxRetryAfter < 0 || r.Breaker.Cooldown < 0 || r.Hedge.Delay < 0 || r.FirstTokenTimeout < 0 {
		bad("durations must not be negative")
	}
	if r.BackoffBase > 0 && r.BackoffMax > 0 && r.BackoffMax < r.BackoffBase {
		bad("backoff_max is below backoff_base")
	}
	if r.Breaker.Failures != nil && *r.Breaker.Failures < 0 {
		bad("breaker.failures must not be negative")
	}
	h := r.Hedge
	if h.Quantile != 0 && (h.Quantile <= 0 || h.Quantile >= 1) {
		bad("hedge.quantile must be between 0 and 1")
	}
	if h.Window < 0 || h.MinSamples < 0 || h.Burst < 0 {
		bad("hedge.window, hedge.min_samples and hedge.burst must not be negative")
	}
	if h.Window > 0 && h.MinSamples > h.Window {
		bad("hedge.min_samples exceeds hedge.window")
	}
	if h.Budget != nil && (*h.Budget < 0 || *h.Budget > 1) {
		bad("hedge.budget must be between 0 and 1 (a share of requests)")
	}
	return errs
}

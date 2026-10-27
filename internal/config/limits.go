package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TheAsianFish/switchboard/internal/cost"
)

// Redis is the shared store for limits and budgets. An empty URL runs the
// gateway without one: limits and budgets are then per process, which is
// right for a single replica on a laptop and wrong for a fleet.
type Redis struct {
	// URL is a redis:// or rediss:// URL, for example redis://redis:6379/0.
	URL string `yaml:"url"`
	// Timeout bounds every Redis call. A call that misses it counts as the
	// store being unreachable and the degrade policy takes over.
	Timeout time.Duration `yaml:"timeout"`
	// KeyPrefix namespaces every key the gateway writes.
	KeyPrefix string `yaml:"key_prefix"`
}

// RateLimit chooses the limiter and its degraded-mode behaviour.
type RateLimit struct {
	// Algorithm is fixed_window or gcra.
	Algorithm string `yaml:"algorithm"`
	// Replicas is how many gateway replicas share the limits. While Redis
	// is unreachable each replica enforces rate ÷ replicas on its own, so
	// the fleet as a whole stays near the configured limit.
	Replicas int `yaml:"replicas"`
	// RetryInterval is how long a replica stays on its local fallback
	// after a Redis failure before it tries Redis again.
	RetryInterval time.Duration `yaml:"retry_interval"`
}

// BudgetPolicy configures the budget ledger.
type BudgetPolicy struct {
	// GraceUSD is how much each replica may let a key spend while Redis is
	// unreachable before it refuses that key.
	GraceUSD Decimal `yaml:"grace_usd"`
	// ReservationTTL is how long an unsettled reservation holds budget.
	ReservationTTL time.Duration `yaml:"reservation_ttl"`
}

// Limits are a key's rate limits. Either may be absent.
type Limits struct {
	// Requests counts one unit per request.
	Requests *Rate `yaml:"requests"`
	// Tokens counts estimated input plus maximum output tokens on
	// admission and is corrected to the real usage afterwards.
	Tokens *Rate `yaml:"tokens"`
}

// Rate is a sustained rate with a burst.
type Rate struct {
	Rate  int64         `yaml:"rate"`
	Per   time.Duration `yaml:"per"`
	Burst int64         `yaml:"burst"`
}

// KeyBudget is a key's dollar allowance.
type KeyBudget struct {
	USD Decimal `yaml:"usd"`
	// Period is total (never resets), daily or monthly.
	Period string `yaml:"period"`
}

// Price is a model's price in dollars per million tokens. A model without
// a price uses the built-in list price for its upstream name if there is
// one; otherwise it is unpriced: its spend is not counted and keys with a
// budget may not use it.
type Price struct {
	Input      Decimal `yaml:"input"`
	Output     Decimal `yaml:"output"`
	CacheRead  Decimal `yaml:"cache_read"`
	CacheWrite Decimal `yaml:"cache_write"`
	// LongAbove and Long describe a higher rate for every token of a
	// request whose prompt is longer than LongAbove tokens.
	LongAbove int    `yaml:"long_above"`
	Long      *Price `yaml:"long"`
}

// Decimal is a number kept as the text written in the file, so "0.10"
// reaches the money parser without a trip through floating point.
type Decimal string

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Decimal) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a number", n.Line)
	}
	*d = Decimal(n.Value)
	return nil
}

// Defaults for the shared-state settings.
const (
	DefaultRedisTimeout   = 100 * time.Millisecond
	DefaultKeyPrefix      = "sb:"
	DefaultRetryInterval  = time.Second
	DefaultGraceUSD       = "0.10"
	DefaultOutputEstimate = 4096
)

// ResolvedPrice is the price that applies to m, from its config or the
// built-in table, and whether it has one at all.
func (m Model) ResolvedPrice() (cost.Price, bool, error) {
	if m.Price == nil {
		p, ok := cost.Defaults[m.Upstream]
		return p, ok, nil
	}
	base, err := m.Price.rates()
	if err != nil {
		return cost.Price{}, false, err
	}
	p := cost.Price{Base: base}
	if m.Price.LongAbove > 0 || m.Price.Long != nil {
		if m.Price.LongAbove <= 0 || m.Price.Long == nil {
			return cost.Price{}, false, errors.New("price: long_above and long must be set together")
		}
		if p.Long, err = m.Price.Long.rates(); err != nil {
			return cost.Price{}, false, err
		}
		p.LongAbove = m.Price.LongAbove
	}
	return p, true, nil
}

func (p *Price) rates() (cost.Rates, error) {
	var r cost.Rates
	var errs []error
	for _, f := range []struct {
		name string
		in   Decimal
		out  *cost.PerToken
	}{{"input", p.Input, &r.Input}, {"output", p.Output, &r.Output}, {"cache_read", p.CacheRead, &r.CacheRead}, {"cache_write", p.CacheWrite, &r.CacheWrite}} {
		if f.in == "" {
			continue // an omitted rate is free
		}
		v, err := cost.ParsePerMTok(string(f.in))
		if err != nil {
			errs = append(errs, fmt.Errorf("price %s: %w", f.name, err))
		}
		*f.out = v
	}
	return r, errors.Join(errs...)
}

// validateShared checks the Redis, rate limit and budget sections and each
// key's limits and budget.
func (c *Config) validateShared(models map[string]Model) []error {
	var errs []error
	switch c.RateLimit.Algorithm {
	case "fixed_window", "gcra":
	default:
		errs = append(errs, fmt.Errorf("ratelimit.algorithm %q is not one of fixed_window, gcra", c.RateLimit.Algorithm))
	}
	if c.Redis.Timeout <= 0 {
		// Zero would hand go-redis its own multi-second defaults.
		errs = append(errs, fmt.Errorf("redis.timeout must be positive, got %v", c.Redis.Timeout))
	}
	if c.RateLimit.Replicas < 1 {
		errs = append(errs, fmt.Errorf("ratelimit.replicas must be at least 1, got %d", c.RateLimit.Replicas))
	}
	if _, err := cost.ParseUSD(string(c.Budget.GraceUSD)); err != nil {
		errs = append(errs, fmt.Errorf("budget.grace_usd: %w", err))
	}
	for _, p := range c.Providers {
		// A hold that expires while its call is still running frees
		// budget the call is about to spend.
		if c.Budget.ReservationTTL <= p.Timeout {
			errs = append(errs, fmt.Errorf("budget.reservation_ttl (%v) must be longer than provider %q's timeout (%v)", c.Budget.ReservationTTL, p.Name, p.Timeout))
		}
	}
	for _, m := range c.Models {
		if _, _, err := m.ResolvedPrice(); err != nil {
			errs = append(errs, fmt.Errorf("model %q: %w", m.Name, err))
		}
		if m.MaxOutputTokens < 0 {
			errs = append(errs, fmt.Errorf("model %q: max_output_tokens must not be negative", m.Name))
		}
	}
	for _, k := range c.Keys {
		for name, r := range map[string]*Rate{"requests": k.Limits.Requests, "tokens": k.Limits.Tokens} {
			if r == nil {
				continue
			}
			if r.Rate <= 0 || r.Per <= 0 || r.Burst <= 0 {
				errs = append(errs, fmt.Errorf("key %q: limits.%s needs a positive rate, per and burst", k.Name, name))
			} else if r.Per/time.Duration(r.Rate) < time.Microsecond {
				// The limiters keep time in microseconds.
				errs = append(errs, fmt.Errorf("key %q: limits.%s refills faster than one unit per microsecond", k.Name, name))
			}
		}
		if k.Budget == nil {
			continue
		}
		if _, err := cost.ParseUSD(string(k.Budget.USD)); err != nil {
			errs = append(errs, fmt.Errorf("key %q: budget.usd: %w", k.Name, err))
		}
		switch k.Budget.Period {
		case "total", "daily", "monthly":
		default:
			errs = append(errs, fmt.Errorf("key %q: budget.period %q is not one of total, daily, monthly", k.Name, k.Budget.Period))
		}
		// A budget is only as good as the prices behind it: a budgeted key
		// that could reach an unpriced model would spend without limit.
		reach := k.Models
		if len(reach) == 0 {
			for _, m := range c.Models {
				reach = append(reach, m.Name)
			}
		}
		for _, name := range reach {
			m, ok := models[name]
			if !ok {
				continue
			}
			if _, priced, _ := m.ResolvedPrice(); !priced {
				errs = append(errs, fmt.Errorf("key %q has a budget but may use model %q, which has no price; add one (price: {input: 0, output: 0} for a free model) or restrict the key's models", k.Name, name))
			}
		}
	}
	return errs
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

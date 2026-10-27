// Package cost turns token usage into dollars. Money is an integer count of
// nanodollars, so a ledger summed over millions of requests is exact and
// the gateway's spend can be compared with a provider's to the cent.
package cost

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// Nanos is an amount of money in nanodollars (1e-9 USD). A price of $X per
// million tokens is X*1000 nanodollars per token, an integer for any price
// quoted to a tenth of a cent per million tokens.
type Nanos int64

// Common amounts.
const (
	Nanodollar Nanos = 1
	Cent       Nanos = 10_000_000
	Dollar     Nanos = 1_000_000_000
)

// USD is n in dollars, for metrics and display. Ledgers keep Nanos.
func (n Nanos) USD() float64 { return float64(n) / float64(Dollar) }

// String formats n as dollars with nine decimals, the full precision.
func (n Nanos) String() string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s$%d.%09d", sign, n/Dollar, n%Dollar)
}

// ParseUSD parses a decimal dollar amount such as "5", "0.10" or "$12.5"
// without going through floating point. More than nine decimals is an
// error, not a silent rounding.
func ParseUSD(s string) (Nanos, error) {
	return parseDecimal(strings.TrimPrefix(strings.TrimSpace(s), "$"), 9)
}

// parseDecimal parses a non-negative decimal with at most scale fractional
// digits into an integer count of 10^-scale units.
func parseDecimal(s string, scale int) (Nanos, error) {
	if s == "" {
		return 0, errors.New("empty amount")
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > scale {
		return 0, fmt.Errorf("amount %q has more than %d decimal places", s, scale)
	}
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("amount %q is not a non-negative decimal", s)
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", s, err)
	}
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", scale-len(frac)), 10, 64)
	}
	unit := int64(1)
	for range scale {
		unit *= 10
	}
	if w > (1<<63-1-f)/unit {
		return 0, fmt.Errorf("amount %q is too large", s)
	}
	return Nanos(w*unit + f), nil
}

// PerToken is a price in nanodollars per token. ParsePerMTok reads the
// usual "$ per million tokens" form.
type PerToken int64

// ParsePerMTok parses a price in dollars per million tokens, for example
// "0.10" for $0.10/MTok (100 nanodollars per token). At most three decimals
// are accepted, the precision at which the per-token price is an integer.
func ParsePerMTok(s string) (PerToken, error) {
	n, err := parseDecimal(strings.TrimPrefix(strings.TrimSpace(s), "$"), 3)
	return PerToken(n), err
}

// PerMTok formats p back as dollars per million tokens.
func (p PerToken) PerMTok() string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%03d", p/1000, p%1000), "0"), ".")
}

// Rates are the four per-token prices of a model.
type Rates struct {
	Input, Output, CacheRead, CacheWrite PerToken
}

// Price is a model's pricing. Some models charge more for long prompts:
// when LongAbove is positive and a request's whole prompt (cached or not)
// is longer than LongAbove tokens, every token of that request is charged
// at Long instead of Base.
type Price struct {
	Base      Rates
	LongAbove int
	Long      Rates
}

// Rates returns the rates that apply to a request with the given usage.
func (p Price) Rates(u core.Usage) Rates {
	if p.LongAbove > 0 && u.PromptTokens() > p.LongAbove {
		return p.Long
	}
	return p.Base
}

// Of is what usage u costs at p.
func (p Price) Of(u core.Usage) Nanos {
	r := p.Rates(u)
	return Nanos(int64(u.InputTokens)*int64(r.Input) +
		int64(u.OutputTokens)*int64(r.Output) +
		int64(u.CacheReadTokens)*int64(r.CacheRead) +
		int64(u.CacheWriteTokens)*int64(r.CacheWrite))
}

// Free is the price of a local model.
var Free = Price{}

// Defaults are list prices for models the gateway knows, by upstream model
// name, checked on 2026-10-27. Cache writes are the five-minute rate (1.25×
// input); the one-hour rate (2×) is not distinguished in usage. Haiku 5.5
// charges its long-context rate above 100K prompt tokens.
var Defaults = map[string]Price{
	"claude-opus-5-5":   {Base: rates("4", "20", "0.20", "5")},
	"claude-sonnet-5-5": {Base: rates("2", "10", "0.20", "2.50")},
	"claude-haiku-5-5": {
		Base:      rates("0.10", "0.50", "0.01", "0.125"),
		LongAbove: 100_000,
		Long:      rates("0.50", "2.50", "0.05", "0.625"),
	},
}

func rates(in, out, read, write string) Rates {
	must := func(s string) PerToken {
		p, err := ParsePerMTok(s)
		if err != nil {
			panic(err)
		}
		return p
	}
	return Rates{Input: must(in), Output: must(out), CacheRead: must(read), CacheWrite: must(write)}
}

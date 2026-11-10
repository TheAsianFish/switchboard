// Package auth resolves client API keys to principals.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
)

// Principal is the identity behind an API key.
type Principal struct {
	// Name is the key's configured name; it is what logs and metrics show,
	// never the key itself.
	Name string
	// models is the allow list; nil allows every model.
	models map[string]bool
	// Requests and Tokens are the key's rate limits; nil means none.
	Requests, Tokens *ratelimit.Limit
	// Budget is the key's dollar allowance; nil means none.
	Budget *budget.Budget
	// Tenant is the cache namespace: keys with the same configured tenant
	// share cached replies, and no lookup ever crosses tenants. It is
	// "tenant:" and the tenant, or "key:" and the key's name for a key
	// without one, so a tenant named like another key never shares that
	// key's entries.
	Tenant string
	// Cache is the most caching the key's requests get.
	Cache cache.Mode
}

// Allows reports whether the principal may use model.
func (p *Principal) Allows(model string) bool {
	return p.models == nil || p.models[model]
}

// Keys is an immutable key table, safe for concurrent use.
type Keys struct {
	byDigest map[[sha256.Size]byte]*Principal
}

// NewKeys builds the table from configured keys. Plain keys are hashed on
// load so that lookups for plain and pre-hashed keys take the same path.
// A key without a cache mode gets none (config.Load fills in cache.mode).
func NewKeys(keys []config.Key) *Keys {
	t := &Keys{byDigest: make(map[[sha256.Size]byte]*Principal, len(keys))}
	for _, k := range keys {
		var digest [sha256.Size]byte
		if k.Key != "" {
			digest = sha256.Sum256([]byte(k.Key))
		} else {
			b, err := hex.DecodeString(strings.ToLower(k.SHA256))
			if err != nil || len(b) != sha256.Size {
				continue // config.Validate rejects these
			}
			copy(digest[:], b)
		}
		p := &Principal{Name: k.Name, Requests: limit(k.Limits.Requests), Tokens: limit(k.Limits.Tokens), Budget: keyBudget(k.Budget), Tenant: "tenant:" + k.Tenant}
		if k.Tenant == "" {
			p.Tenant = "key:" + k.Name
		}
		if k.Cache != "" {
			p.Cache, _ = cache.ParseMode(k.Cache) // config.Validate rejects bad modes
		}
		if len(k.Models) > 0 {
			p.models = make(map[string]bool, len(k.Models))
			for _, m := range k.Models {
				p.models[m] = true
			}
		}
		t.byDigest[digest] = p
	}
	return t
}

// limit converts a validated config rate.
func limit(r *config.Rate) *ratelimit.Limit {
	if r == nil {
		return nil
	}
	return &ratelimit.Limit{Rate: r.Rate, Period: r.Per, Burst: r.Burst}
}

// keyBudget converts a validated config budget.
func keyBudget(b *config.KeyBudget) *budget.Budget {
	if b == nil {
		return nil
	}
	usd, _ := cost.ParseUSD(string(b.USD)) // config.Validate rejects bad amounts
	return &budget.Budget{Limit: usd, Period: budget.Period(b.Period)}
}

// Principals lists every configured key's principal, sorted by name, for
// the admin API.
func (t *Keys) Principals() []*Principal {
	out := make([]*Principal, 0, len(t.byDigest))
	for _, p := range t.byDigest {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b *Principal) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Models is the principal's allow list, sorted; nil allows every model.
func (p *Principal) Models() []string {
	if p.models == nil {
		return nil
	}
	out := make([]string, 0, len(p.models))
	for m := range p.models {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// Lookup resolves a presented key. The lookup is by SHA-256 digest, so the
// map comparison never touches the key itself and its timing reveals
// nothing about how close a guess was.
func (t *Keys) Lookup(key string) (*Principal, error) {
	if key == "" {
		return nil, core.Errorf(core.KindAuthentication, "missing API key")
	}
	p, ok := t.byDigest[sha256.Sum256([]byte(key))]
	if !ok {
		return nil, core.Errorf(core.KindAuthentication, "invalid API key")
	}
	return p, nil
}

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the authenticated principal, or nil.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

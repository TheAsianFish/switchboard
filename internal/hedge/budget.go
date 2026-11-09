package hedge

import (
	"math"
	"sync"
)

// Budgets caps hedging per tenant: every request earns the tenant ratio of
// a hedge, up to burst hedges saved, and every hedge spends one. Over any
// stretch of traffic a tenant sends at most ratio × requests + burst
// hedges, so hedging can never more than (1 + ratio) a tenant's upstream
// requests however slow the provider gets. That matters most exactly when
// it would otherwise hedge the most: when the provider is overloaded.
//
// The counts are per replica. The bound is a ratio, and a ratio enforced
// on each replica's share of the traffic holds for the fleet's total, so
// there is nothing to gain from a Redis round trip per request.
type Budgets struct {
	// ratio and burst are in thousandths of a hedge, so that ten requests
	// at a ratio of 0.1 earn exactly one hedge.
	ratio int64
	burst int64

	mu      sync.Mutex
	credits map[string]int64
}

const unit = 1000

// NewBudgets returns per-tenant hedge budgets. A tenant starts with burst
// credits, so the first requests after a restart can hedge.
func NewBudgets(ratio, burst float64) *Budgets {
	return &Budgets{ratio: int64(math.Round(ratio * unit)), burst: int64(math.Round(burst * unit)), credits: map[string]int64{}}
}

// Earn credits tenant with one request's share of a hedge.
func (b *Budgets) Earn(tenant string) {
	b.mu.Lock()
	c, ok := b.credits[tenant]
	if !ok {
		c = b.burst
	}
	b.credits[tenant] = min(b.burst, c+b.ratio)
	b.mu.Unlock()
}

// Spend takes one hedge from tenant's budget and reports whether there was
// one to take.
func (b *Budgets) Spend(tenant string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.credits[tenant]
	if !ok {
		c = b.burst
	}
	if c < unit {
		b.credits[tenant] = c
		return false
	}
	b.credits[tenant] = c - unit
	return true
}

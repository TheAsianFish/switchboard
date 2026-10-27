// Package budget keeps per-key dollar budgets shared by every replica.
//
// A request reserves its worst-case cost before it is sent and settles the
// real cost when usage arrives. A reservation that is never settled (the
// replica holding it died) expires after a TTL, so a crash cannot leak
// budget forever. Because reservations are worst case, concurrent requests
// on any number of replicas cannot together spend past the limit unless an
// estimate was low.
package budget

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cost"
)

// Period is how often a budget resets.
type Period string

// Periods. Boundaries are UTC.
const (
	PeriodTotal   Period = "total"
	PeriodDaily   Period = "daily"
	PeriodMonthly Period = "monthly"
)

// Budget is a key's allowance.
type Budget struct {
	Limit  cost.Nanos
	Period Period
}

// Window names the period that contains t, for example "2026-10" for a
// monthly budget; ledgers keep one balance per window.
func (b Budget) Window(t time.Time) string {
	t = t.UTC()
	switch b.Period {
	case PeriodDaily:
		return t.Format("2006-01-02")
	case PeriodMonthly:
		return t.Format("2006-01")
	default:
		return "total"
	}
}

// Validate reports whether b is usable.
func (b Budget) Validate() error {
	if b.Limit < 0 {
		return fmt.Errorf("budget: limit must not be negative, got %v", b.Limit)
	}
	switch b.Period {
	case PeriodTotal, PeriodDaily, PeriodMonthly:
		return nil
	default:
		return fmt.Errorf("budget: period %q is not one of total, daily, monthly", b.Period)
	}
}

// Reservation is a hold on part of a budget for one request in flight.
type Reservation struct {
	// ID identifies the hold in the ledger; it is unique per request.
	ID string
	// Window is the budget window the hold was made in. Settlement lands
	// in the same window even if the request crosses a boundary.
	Window string
	Amount cost.Nanos
	// grace is the generation of the Degrading grace ledger that holds
	// this reservation, or 0 if the primary ledger holds it.
	grace uint64
}

// Balance is a key's position in one window.
type Balance struct {
	Window string
	Limit  cost.Nanos
	// Spent is the sum of settled costs.
	Spent cost.Nanos
	// Reserved is the sum of unexpired, unsettled reservations.
	Reserved cost.Nanos
}

// Remaining is what can still be reserved.
func (b Balance) Remaining() cost.Nanos { return b.Limit - b.Spent - b.Reserved }

// Ledger is the interface the Redis and in-memory ledgers implement. They
// are safe for concurrent use.
type Ledger interface {
	// Reserve holds amount against key's budget in the window containing
	// now, or fails with an error wrapping ErrExhausted (and holds nothing)
	// if spent + reserved + amount would exceed the limit.
	Reserve(ctx context.Context, key string, b Budget, id string, amount cost.Nanos) (Reservation, error)
	// Settle releases r and records actual as spent. Settling an expired
	// or unknown reservation still records actual: the money was spent.
	Settle(ctx context.Context, key string, b Budget, r Reservation, actual cost.Nanos) error
	// Balance reports key's position in the window containing now.
	Balance(ctx context.Context, key string, b Budget) (Balance, error)
}

// ErrExhausted is wrapped by Reserve when the budget cannot cover a
// reservation.
var ErrExhausted = errors.New("budget exhausted")

// DefaultReservationTTL is how long an unsettled reservation holds budget.
// It must outlast the longest request: an expired hold that is then
// settled is still counted, but while it is expired the budget could be
// over-reserved by its amount.
const DefaultReservationTTL = 15 * time.Minute

package budget

import (
	"context"
	"sync"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cost"
)

// Memory is an in-process Ledger with the same semantics as Redis. It
// serves single-replica deployments without Redis. It is safe for
// concurrent use.
type Memory struct {
	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	accounts  map[accountKey]*account
	nextSweep time.Time
}

type accountKey struct{ key, window string }

type account struct {
	spent, reserved cost.Nanos
	holds           map[string]hold
	// tombs holds the amount of every hold that expired unsettled and was
	// charged as spent, until its late settle corrects the charge. They
	// live as long as the account, as Redis's live as long as the window.
	tombs map[string]cost.Nanos
	// expires is when the window is dropped; zero means never.
	expires time.Time
}

type hold struct {
	amount  cost.Nanos
	expires time.Time
}

// sweepEvery bounds how often Memory scans for windows past their TTL.
const sweepEvery = time.Hour

var _ Ledger = (*Memory)(nil)

// NewMemory returns an empty ledger. ttl is how long an unsettled
// reservation holds budget (DefaultReservationTTL if not positive); now is
// the clock (time.Now if nil).
func NewMemory(ttl time.Duration, now func() time.Time) *Memory {
	if ttl <= 0 {
		ttl = DefaultReservationTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Memory{ttl: ttl, now: now, accounts: make(map[accountKey]*account)}
}

// account returns the account for key and window, purged of expired holds
// and with its TTL refreshed, creating it if create is set. Callers hold mu.
func (l *Memory) account(key string, p Period, window string, now time.Time, create bool) *account {
	if !now.Before(l.nextSweep) {
		for k, a := range l.accounts {
			if !a.expires.IsZero() && !now.Before(a.expires) {
				delete(l.accounts, k)
			}
		}
		l.nextSweep = now.Add(sweepEvery)
	}
	k := accountKey{key, window}
	a := l.accounts[k]
	if a == nil {
		if !create {
			return nil
		}
		a = &account{holds: make(map[string]hold), tombs: make(map[string]cost.Nanos)}
		l.accounts[k] = a
	}
	for id, h := range a.holds {
		if !now.Before(h.expires) {
			a.expire(id)
		}
	}
	if ttl := windowTTL(p); ttl > 0 && create {
		a.expires = now.Add(ttl)
	}
	return a
}

// release drops id's hold and reports whether there was one.
func (a *account) release(id string) bool {
	h, ok := a.holds[id]
	if !ok {
		return false
	}
	delete(a.holds, id)
	a.reserved = max(a.reserved-h.amount, 0)
	return true
}

// expire charges id's hold as spent at its held amount and keeps a
// tombstone for a late settle, as the Redis scripts do.
func (a *account) expire(id string) {
	h := a.holds[id]
	a.release(id)
	if h.amount != 0 {
		a.spent += h.amount
		a.tombs[id] = h.amount
	}
}

// Reserve implements Ledger, with Redis.Reserve's rules for zero amounts
// and repeated ids.
func (l *Memory) Reserve(_ context.Context, key string, b Budget, id string, amount cost.Nanos) (Reservation, error) {
	if err := checkReserve(b, id, amount); err != nil {
		return Reservation{}, err
	}
	now := l.now()
	window := b.Window(now)
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.account(key, b.Period, window, now, true)
	a.release(id)
	rem := b.Limit - a.spent - a.reserved
	if amount > rem || (amount == 0 && rem <= 0) {
		return Reservation{}, exhausted(key, window, rem, amount)
	}
	a.reserved += amount
	a.holds[id] = hold{amount: amount, expires: now.Add(l.ttl)}
	return Reservation{ID: id, Window: window, Amount: amount}, nil
}

// Settle implements Ledger, with Redis.Settle's rules for the window, an
// empty id and an expired hold.
func (l *Memory) Settle(_ context.Context, key string, b Budget, r Reservation, actual cost.Nanos) error {
	if err := checkSettle(b, actual); err != nil {
		return err
	}
	now := l.now()
	window := r.Window
	if window == "" {
		window = b.Window(now)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.account(key, b.Period, window, now, true)
	if r.ID != "" && !a.release(r.ID) {
		if held, ok := a.tombs[r.ID]; ok {
			delete(a.tombs, r.ID)
			actual -= held
		}
	}
	a.spent += actual
	return nil
}

// Balance implements Ledger.
func (l *Memory) Balance(_ context.Context, key string, b Budget) (Balance, error) {
	if err := b.Validate(); err != nil {
		return Balance{}, err
	}
	now := l.now()
	window := b.Window(now)
	l.mu.Lock()
	defer l.mu.Unlock()
	bal := Balance{Window: window, Limit: b.Limit}
	if a := l.account(key, b.Period, window, now, false); a != nil {
		bal.Spent, bal.Reserved = a.spent, a.reserved
	}
	return bal, nil
}

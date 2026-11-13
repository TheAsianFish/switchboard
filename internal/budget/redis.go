package budget

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/cost"
)

// MaxExact is the largest limit, amount or balance a ledger accepts. The
// Redis scripts compare in Lua, whose numbers are doubles, so comparisons
// are exact only below 2^53 nanodollars (about $9M). Sums are kept with
// HINCRBY and stay exact int64 regardless, but a budget or a balance above
// this ceiling could be admitted or refused one nanodollar wrong; the
// ledgers refuse such inputs instead of rounding silently.
const MaxExact cost.Nanos = 1 << 53

// windowTTL is how long a window's keys live after their last write: long
// enough to read yesterday's or last month's spend, short enough that old
// windows do not pile up. Total budgets never expire, and neither do their
// tombstones: one hash field per hold that expired unsettled, which a
// healthy fleet makes only when a replica dies with requests in flight.
func windowTTL(p Period) time.Duration {
	switch p {
	case PeriodDaily:
		return 3 * 24 * time.Hour
	case PeriodMonthly:
		return 70 * 24 * time.Hour
	default:
		return 0
	}
}

// luaPrelude is shared by every script. KEYS[1] is the balance hash
// (fields spent and reserved), KEYS[2] a ZSET of reservation id to expiry
// in milliseconds, KEYS[3] a hash of reservation id to amount and KEYS[4]
// a hash of tombstones: the id and held amount of every hold that expired
// unsettled. Expiry uses the Redis server's clock, so every replica agrees
// on when a hold lapses.
//
// A hold that expires is charged as spent at its held amount: the request
// it covered may have been billed by the provider and its replica may be
// gone, and the hold is the most the request could cost. Its tombstone
// lives as long as the window's keys, so a settle that arrives after the
// expiry (a slow request, or one whose settle waited in a replica's
// pending charges through an outage) corrects spent by actual − held
// instead of charging the cost a second time.
//
// Amounts travel as strings and are written with HINCRBY so that sums are
// exact integers; Lua numbers (doubles) are used only for comparisons.
const luaPrelude = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local function unhold(id)
  local a = redis.call('HGET', KEYS[3], id)
  redis.call('ZREM', KEYS[2], id)
  if a then
    redis.call('HDEL', KEYS[3], id)
    if a ~= '0' then
      local r = redis.call('HINCRBY', KEYS[1], 'reserved', '-' .. a)
      if r < 0 then redis.call('HSET', KEYS[1], 'reserved', '0') end
    end
  end
  return a
end

local expired = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now)
for _, id in ipairs(expired) do
  local a = unhold(id)
  if a and a ~= '0' then
    redis.call('HINCRBY', KEYS[1], 'spent', a)
    redis.call('HSET', KEYS[4], id, a)
  end
end

local function touch(ttl)
  if ttl ~= '0' then
    for i = 1, 4 do redis.call('EXPIRE', KEYS[i], ttl) end
  end
end

local function position()
  return {redis.call('HGET', KEYS[1], 'spent') or '0', redis.call('HGET', KEYS[1], 'reserved') or '0'}
end
`

// reserveScript: ARGV limit, amount, id, hold TTL ms, key TTL s. Returns
// {admitted, spent, reserved} with the position before the hold.
var reserveScript = redis.NewScript(luaPrelude + `
local limit, amount, id = tonumber(ARGV[1]), tonumber(ARGV[2]), ARGV[3]
unhold(id)
local pos = position()
local rem = limit - tonumber(pos[1]) - tonumber(pos[2])
if amount > rem or (amount == 0 and rem <= 0) then
  touch(ARGV[5])
  return {'0', pos[1], pos[2]}
end
redis.call('HINCRBY', KEYS[1], 'reserved', ARGV[2])
redis.call('ZADD', KEYS[2], now + tonumber(ARGV[4]), id)
redis.call('HSET', KEYS[3], id, ARGV[2])
touch(ARGV[5])
return {'1', pos[1], pos[2]}
`)

// settleScript: ARGV id (may be empty), actual, key TTL s. A live hold is
// released and actual charged; a tombstone (the hold expired and was
// charged at its held amount) is removed and spent corrected by
// actual − held; an unknown or empty id is a plain charge.
var settleScript = redis.NewScript(luaPrelude + `
local id = ARGV[1]
local held = false
if id ~= '' then
  if not unhold(id) then
    held = redis.call('HGET', KEYS[4], id)
    if held then redis.call('HDEL', KEYS[4], id) end
  end
end
redis.call('HINCRBY', KEYS[1], 'spent', ARGV[2])
if held then redis.call('HINCRBY', KEYS[1], 'spent', '-' .. held) end
touch(ARGV[3])
return position()
`)

// balanceScript: ARGV key TTL s. Purging expired holds is a write, so
// Balance is a script rather than a plain HMGET.
var balanceScript = redis.NewScript(luaPrelude + `
touch(ARGV[1])
return position()
`)

// Redis is the shared Ledger. Each key and window has four Redis keys
// under one hash tag, so a cluster keeps them on one slot and a script can
// touch all of them atomically.
//
// The window a request falls in comes from the replica's clock (Budget.Window
// of now), so a window boundary is only as accurate as the replicas' clocks:
// a request a second either side of midnight may land in either day.
// Reservation expiry uses the Redis server's clock inside the scripts.
type Redis struct {
	rdb    redis.Scripter
	prefix string
	ttl    time.Duration
	now    func() time.Time
	// dropSettle is a planted bug for the fault-schedule simulator's
	// check on itself (sim.go): a settle whose reservation id it reports
	// releases the hold and records nothing as spent. Nothing outside the
	// simulator sets it.
	dropSettle func(id string, actual cost.Nanos) bool
}

var _ Ledger = (*Redis)(nil)

// NewRedis returns a ledger over rdb. Keys start with prefix; ttl is how
// long an unsettled reservation holds budget (DefaultReservationTTL if not
// positive); now gives the replica clock (time.Now if nil).
func NewRedis(rdb redis.Scripter, prefix string, ttl time.Duration, now func() time.Time) *Redis {
	if ttl <= 0 {
		ttl = DefaultReservationTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Redis{rdb: rdb, prefix: prefix, ttl: ttl, now: now}
}

func (l *Redis) keys(key, window string) []string {
	base := l.prefix + "bud:{" + key + ":" + window + "}"
	return []string{base + ":bal", base + ":exp", base + ":amt", base + ":tomb"}
}

func ttlArg(p Period) string {
	return strconv.FormatInt(int64(windowTTL(p)/time.Second), 10)
}

// Reserve implements Ledger. A zero amount is admitted while anything
// remains and refused once the budget is fully used, so a request whose
// estimate is free still stops at an exhausted budget. Reserving again
// with an id that is already held replaces the earlier hold; an id that
// expired keeps its charge and its tombstone.
func (l *Redis) Reserve(ctx context.Context, key string, b Budget, id string, amount cost.Nanos) (Reservation, error) {
	if err := checkReserve(b, id, amount); err != nil {
		return Reservation{}, err
	}
	window := b.Window(l.now())
	res, err := reserveScript.Run(ctx, l.rdb, l.keys(key, window),
		int64(b.Limit), int64(amount), id, l.ttl.Milliseconds(), ttlArg(b.Period)).StringSlice()
	if err != nil {
		return Reservation{}, fmt.Errorf("budget reserve %s: %w", key, err)
	}
	ok, spent, reserved, err := parsePosition(res)
	if err != nil {
		return Reservation{}, fmt.Errorf("budget reserve %s: %w", key, err)
	}
	if !ok {
		return Reservation{}, exhausted(key, window, b.Limit-spent-reserved, amount)
	}
	return Reservation{ID: id, Window: window, Amount: amount}, nil
}

// Settle implements Ledger. The money lands in r.Window, or in the current
// window if r has none; an empty r.ID is a plain charge, and an r.ID whose
// hold expired corrects the charge made at expiry.
func (l *Redis) Settle(ctx context.Context, key string, b Budget, r Reservation, actual cost.Nanos) error {
	if err := checkSettle(b, actual); err != nil {
		return err
	}
	window := r.Window
	if window == "" {
		window = b.Window(l.now())
	}
	if r.ID != "" && l.dropSettle != nil && l.dropSettle(r.ID, actual) {
		actual = 0
	}
	err := settleScript.Run(ctx, l.rdb, l.keys(key, window), r.ID, int64(actual), ttlArg(b.Period)).Err()
	if err != nil {
		return fmt.Errorf("budget settle %s: %w", key, err)
	}
	return nil
}

// Balance implements Ledger.
func (l *Redis) Balance(ctx context.Context, key string, b Budget) (Balance, error) {
	if err := b.Validate(); err != nil {
		return Balance{}, err
	}
	window := b.Window(l.now())
	res, err := balanceScript.Run(ctx, l.rdb, l.keys(key, window), ttlArg(b.Period)).StringSlice()
	if err != nil {
		return Balance{}, fmt.Errorf("budget balance %s: %w", key, err)
	}
	_, spent, reserved, err := parsePosition(append([]string{"1"}, res...))
	if err != nil {
		return Balance{}, fmt.Errorf("budget balance %s: %w", key, err)
	}
	return Balance{Window: window, Limit: b.Limit, Spent: spent, Reserved: reserved}, nil
}

func parsePosition(res []string) (ok bool, spent, reserved cost.Nanos, err error) {
	if len(res) != 3 {
		return false, 0, 0, fmt.Errorf("unexpected script reply %q", res)
	}
	s, err1 := strconv.ParseInt(res[1], 10, 64)
	r, err2 := strconv.ParseInt(res[2], 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return false, 0, 0, fmt.Errorf("unexpected script reply %q: %w", res, err)
	}
	return res[0] == "1", cost.Nanos(s), cost.Nanos(r), nil
}

// checkReserve validates Reserve's arguments; both ledgers share it so they
// refuse the same inputs.
func checkReserve(b Budget, id string, amount cost.Nanos) error {
	if err := b.Validate(); err != nil {
		return err
	}
	switch {
	case id == "":
		return errors.New("budget: reservation id must not be empty")
	case amount < 0:
		return fmt.Errorf("budget: reservation must not be negative, got %v", amount)
	case amount >= MaxExact || b.Limit >= MaxExact:
		return fmt.Errorf("budget: amounts must be below %v", MaxExact)
	}
	return nil
}

func checkSettle(b Budget, actual cost.Nanos) error {
	if err := b.Validate(); err != nil {
		return err
	}
	switch {
	case actual < 0:
		return fmt.Errorf("budget: settled cost must not be negative, got %v", actual)
	case actual >= MaxExact:
		return fmt.Errorf("budget: amounts must be below %v", MaxExact)
	}
	return nil
}

func exhausted(key, window string, remaining, needed cost.Nanos) error {
	return fmt.Errorf("budget %s window %s: remaining %v, needed %v: %w", key, window, remaining, needed, ErrExhausted)
}

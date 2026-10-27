package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// FixedWindow is the baseline limiter: a counter per key per window of
// length Limit.Period, allowing Limit.Rate units per window. Burst is
// deliberately ignored; this is the classic "N per minute" counter, and
// its flaw (up to 2×Rate admitted across a window boundary) is what Q2
// measures against GCRA.
//
// Each call is one Lua script, so the read, the decision and the write are
// atomic across replicas. The window index comes from Redis's TIME, so
// replicas with skewed clocks agree on which window they are in.
type FixedWindow struct {
	rdb    redis.Scripter
	prefix string
}

// NewFixedWindow returns a fixed-window limiter storing its counters under
// prefix in rdb.
func NewFixedWindow(rdb redis.Scripter, prefix string) *FixedWindow {
	return &FixedWindow{rdb: rdb, prefix: prefix}
}

var _ Limiter = (*FixedWindow)(nil)

// fwPrelude reads the clock and the current window's count. The window
// index is appended to KEYS[1] inside the script because only the script
// knows the time; the hash tag in KEYS[1] keeps every window of one key in
// the same cluster slot. %.0f formats the index without the exponent that
// Lua's default number formatting would use for large values.
const fwPrelude = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local period = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local n = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])
local win = math.floor(now / period)
local key = KEYS[1] .. string.format('%.0f', win)
local count = tonumber(redis.call('GET', key) or '0')
`

// fwAllow returns {allowed, remaining, retry_after_us}; retry -1 is Never.
var fwAllow = redis.NewScript(fwPrelude + `
if n > rate then
  return {0, math.max(rate - count, 0), -1}
end
if count + n <= rate then
  count = redis.call('INCRBY', key, n)
  redis.call('PEXPIRE', key, ttl)
  return {1, math.max(rate - count, 0), 0}
end
return {0, math.max(rate - count, 0), (win + 1) * period - now}
`)

// fwAdjust moves the current window's count by n, never below zero.
var fwAdjust = redis.NewScript(fwPrelude + `
local next = count + n
if next < 0 then next = 0 end
if next == count then return count end
if next == 0 then
  redis.call('DEL', key)
else
  redis.call('SET', key, string.format('%.0f', next), 'PX', ttl)
end
return next
`)

// key is the stem of key's counters; the script appends the window index.
func (f *FixedWindow) key(key string) string {
	return f.prefix + "rl:fw:{" + key + "}:"
}

// Allow implements Limiter.
func (f *FixedWindow) Allow(ctx context.Context, key string, l Limit, cost int64) (Decision, error) {
	if err := checkArgs(l, cost); err != nil {
		return Decision{}, err
	}
	p := periodMicros(l)
	r, err := fwAllow.Run(ctx, f.rdb, []string{f.key(key)}, p, l.Rate, cost, windowTTL(p)).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: fixed window allow: %w", err)
	}
	if len(r) != 3 {
		return Decision{}, fmt.Errorf("ratelimit: fixed window allow: unexpected reply %v", r)
	}
	d := Decision{Allowed: r[0] == 1, Remaining: r[1]}
	switch {
	case d.Allowed:
	case r[2] < 0:
		d.RetryAfter = Never
	default:
		d.RetryAfter = time.Duration(r[2]) * time.Microsecond
	}
	return d, nil
}

// Adjust implements Limiter. The correction lands in the current window:
// a charge made after a window rolled over counts against the new one,
// which is the baseline's behaviour and part of what Q2 measures.
func (f *FixedWindow) Adjust(ctx context.Context, key string, l Limit, delta int64) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if delta == 0 {
		return nil
	}
	p := periodMicros(l)
	if err := fwAdjust.Run(ctx, f.rdb, []string{f.key(key)}, p, l.Rate, delta, windowTTL(p)).Err(); err != nil {
		return fmt.Errorf("ratelimit: fixed window adjust: %w", err)
	}
	return nil
}

// checkArgs validates the arguments every Allow takes, before any store is
// touched.
func checkArgs(l Limit, cost int64) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if cost < 1 {
		return fmt.Errorf("ratelimit: cost must be at least 1, got %d", cost)
	}
	return nil
}

// periodMicros is the period in whole microseconds, the precision of
// Redis's clock, and at least one so a window index is always defined.
func periodMicros(l Limit) int64 {
	return max(l.Period.Microseconds(), 1)
}

// windowTTL keeps a counter for about two periods, in milliseconds: long
// enough to outlive its window, short enough that idle keys disappear.
func windowTTL(periodUS int64) int64 {
	return max((2*periodUS+999)/1000, 1)
}

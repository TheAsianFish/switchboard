package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"time"
)

// Gateway headers the generator sets and reads. They are the values of
// internal/cache's DirectiveHeader and StatusHeader; a test keeps them in
// step, so the generator does not import the gateway.
const (
	CacheDirectiveHeader = "X-Switchboard-Cache"
	CacheStatusHeader    = "X-Switchboard-Cache-Status"
)

// PoolSlot is the pool prompt request i reuses, or -1 if it sends a
// unique prompt. A request repeats with probability repeat and then picks
// one of pool prompts uniformly; both draws come from a generator seeded
// with (seed, i), so the choice does not depend on timing or on which
// goroutine sends the request.
func PoolSlot(seed uint64, i int, repeat float64, pool int) int {
	if repeat <= 0 || pool <= 0 {
		return -1
	}
	r := rand.New(rand.NewPCG(seed, uint64(i)))
	if r.Float64() >= repeat {
		return -1
	}
	return r.IntN(pool)
}

// prompt is request i's user turn: a pool prompt when it repeats, else one
// no other request in the run sends.
func prompt(cfg *Config, i int) string {
	if slot := PoolSlot(cfg.Seed, i, cfg.Repeat, cfg.RepeatPool); slot >= 0 {
		return fmt.Sprintf("bench pool prompt %d: say something", slot)
	}
	return fmt.Sprintf("bench request %d: say something", i)
}

// Record is one line of the per-request log: what happened to one
// scheduled request. Offsets are seconds from the run's start (the first
// scheduled request, Result.StartedAt), so a log can be lined up with
// events recorded elsewhere on the same clock.
type Record struct {
	// I is the request's index in the schedule.
	I      int  `json:"i"`
	Warmup bool `json:"warmup,omitempty"`
	// At is the scheduled offset; Sent the actual send offset, absent
	// for a request that was dropped.
	At   float64  `json:"at_s"`
	Sent *float64 `json:"sent_s,omitempty"`
	// LatencyMS runs from the scheduled time to the complete reply or
	// the failure; TTFTMS to the first generated text, absent if none
	// arrived. Both are absent for a dropped request.
	LatencyMS *float64 `json:"latency_ms,omitempty"`
	TTFTMS    *float64 `json:"ttft_ms,omitempty"`
	// Outcome is one of the Outcome* values or http_<status>; it is the
	// error class of a failed request.
	Outcome string `json:"outcome"`
	// Status is the HTTP status, absent when no response arrived.
	Status int `json:"status,omitempty"`
	// Cache is the gateway's X-Switchboard-Cache-Status, absent when the
	// response had none.
	Cache string `json:"cache,omitempty"`
	// Pool is the pool prompt the request reused, absent for a unique
	// prompt.
	Pool *int `json:"pool,omitempty"`
}

// record builds request i's log line.
func record(cfg *Config, i int, r scheduled, start time.Time) Record {
	rec := Record{
		I:       i,
		Warmup:  r.warmup,
		At:      round6(r.at.Sub(start).Seconds()),
		Outcome: r.outcome,
		Status:  r.status,
		Cache:   r.cache,
	}
	if !r.sent.IsZero() {
		v := round6(r.sent.Sub(start).Seconds())
		rec.Sent = &v
	}
	if !r.done.IsZero() {
		v := round3(ms(r.done.Sub(r.at)))
		rec.LatencyMS = &v
	}
	if !r.firstToken.IsZero() {
		v := round3(ms(r.firstToken.Sub(r.at)))
		rec.TTFTMS = &v
	}
	if slot := PoolSlot(cfg.Seed, i, cfg.Repeat, cfg.RepeatPool); slot >= 0 {
		rec.Pool = &slot
	}
	return rec
}

// writeLog writes one Record per scheduled request to w.
func writeLog(w io.Writer, cfg *Config, rs []scheduled, start time.Time) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	for i, r := range rs {
		if err := enc.Encode(record(cfg, i, r, start)); err != nil {
			return fmt.Errorf("bench: write log: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("bench: write log: %w", err)
	}
	return nil
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

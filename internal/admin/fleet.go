package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Fleet timings. A replica reports every Interval; the reader calls it
// stale after StaleAfter (a few missed reports, as a GC pause or a slow
// Redis could cause), down after DownAfter, and forgets it after
// ForgetAfter, so a killed replica stays on the page long enough to be
// seen and a scaled-down one eventually leaves.
const (
	DefaultInterval = 2 * time.Second
	StaleAfter      = 3 * DefaultInterval
	DownAfter       = 15 * time.Second
	ForgetAfter     = 10 * time.Minute
	// recentTTL expires a replica's recent calls once it stops
	// reporting; its row outlives them.
	recentTTL = 5 * time.Minute
)

// Fleet publishes this replica's report to Redis and reads every
// replica's back. Reports live in one hash (replica id → report) and
// each replica's recent calls under their own key, so reading the fleet
// is two round trips whatever its size.
type Fleet struct {
	rdb    redis.Cmdable
	prefix string
	log    *slog.Logger
}

// NewFleet returns a fleet over rdb, with keys under prefix (the
// gateway's redis.key_prefix).
func NewFleet(rdb redis.Cmdable, prefix string, log *slog.Logger) *Fleet {
	return &Fleet{rdb: rdb, prefix: prefix, log: log}
}

func (f *Fleet) reportsKey() string         { return f.prefix + "fleet:replicas" }
func (f *Fleet) recentKey(id string) string { return f.prefix + "fleet:recent:" + id }

// publish writes one report and its recent calls in one pipeline.
func (f *Fleet) publish(ctx context.Context, rep report, recent []Call) error {
	r, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	c, err := json.Marshal(recent)
	if err != nil {
		return fmt.Errorf("encode recent calls: %w", err)
	}
	_, err = f.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, f.reportsKey(), rep.ID, r)
		p.Set(ctx, f.recentKey(rep.ID), c, recentTTL)
		return nil
	})
	if err != nil {
		return fmt.Errorf("publish fleet report: %w", err)
	}
	return nil
}

// read returns every replica's last report and, for each, its recent
// calls, forgetting replicas silent for ForgetAfter.
func (f *Fleet) read(ctx context.Context, now time.Time) ([]report, map[string][]Call, error) {
	raw, err := f.rdb.HGetAll(ctx, f.reportsKey()).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("read fleet reports: %w", err)
	}
	var reps []report
	var forget []string
	for id, v := range raw {
		var rep report
		if err := json.Unmarshal([]byte(v), &rep); err != nil {
			f.log.Warn("unreadable fleet report", slog.String("replica", id), slog.String("error", err.Error()))
			continue
		}
		if now.Sub(rep.LastSeen) > ForgetAfter {
			forget = append(forget, id)
			continue
		}
		reps = append(reps, rep)
	}
	if len(forget) > 0 {
		// Best effort: another reader may race to the same deletion.
		_ = f.rdb.HDel(ctx, f.reportsKey(), forget...).Err()
	}
	recent := make(map[string][]Call, len(reps))
	if len(reps) == 0 {
		return reps, recent, nil
	}
	keys := make([]string, len(reps))
	for i, r := range reps {
		keys[i] = f.recentKey(r.ID)
	}
	vals, err := f.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return reps, recent, fmt.Errorf("read fleet recent calls: %w", err)
	}
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue // expired
		}
		var calls []Call
		if err := json.Unmarshal([]byte(s), &calls); err == nil {
			recent[reps[i].ID] = calls
		}
	}
	return reps, recent, nil
}

// status classifies a replica by the age of its last report.
func status(age time.Duration) string {
	switch {
	case age <= StaleAfter:
		return StatusUp
	case age <= DownAfter:
		return StatusStale
	default:
		return StatusDown
	}
}

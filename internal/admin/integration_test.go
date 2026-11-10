//go:build integration

package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// realRedis connects to the test Redis 8, or skips when no URL was given
// and nothing listens on the default one. A configured URL that does not
// answer is a failure, so CI cannot pass by skipping.
func realRedis(t *testing.T) (*redis.Client, string) {
	t.Helper()
	url, explicit := os.Getenv("SWITCHBOARD_TEST_REDIS_URL"), true
	if url == "" {
		url, explicit = "redis://127.0.0.1:6379/15", false
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		if explicit {
			t.Fatalf("Redis at %s: %v", url, err)
		}
		t.Skipf("no Redis at %s (set SWITCHBOARD_TEST_REDIS_URL): %v", url, err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	prefix := "switchboard-test:" + hex.EncodeToString(b[:]) + ":"
	t.Cleanup(func() {
		ctx := context.Background()
		iter := rdb.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			_ = rdb.Del(ctx, iter.Val()).Err()
		}
		_ = rdb.Close()
	})
	return rdb, prefix
}

// Two replicas publishing to a real Redis 8 see each other, with their
// recent calls merged newest first.
func TestIntegrationFleet(t *testing.T) {
	rdb, prefix := realRedis(t)
	clk := newClock()
	mk := func(id string) replica {
		rec := NewRecorder(RecorderOptions{Now: clk.Now})
		srv := NewServer(Options{ID: id, Recorder: rec, Fleet: NewFleet(rdb, prefix, quiet()), Now: clk.Now, Started: clk.Now(), Log: quiet()})
		return replica{rec: rec, srv: srv}
	}
	a, b := mk("a"), mk("b")
	a.rec.Observe(call(clk.Now(), "k", telemetry.OutcomeOK, time.Millisecond))
	clk.Add(time.Millisecond)
	b.rec.Observe(call(clk.Now(), "k", telemetry.OutcomeOK, time.Millisecond))
	a.publish(t)
	b.publish(t)
	ov := a.srv.Overview(context.Background())
	if ov.FleetError != "" || len(ov.Replicas) != 2 || ov.Replicas[1].ID != "b" || ov.Replicas[1].Status != StatusUp {
		t.Fatalf("overview = %+v", ov)
	}
	if len(ov.Recent) != 2 || ov.Recent[0].Replica != "b" || ov.Recent[1].Replica != "a" {
		t.Fatalf("recent = %+v", ov.Recent)
	}
	if ttl := rdb.TTL(context.Background(), prefix+"fleet:recent:b").Val(); ttl <= 0 || ttl > recentTTL {
		t.Fatalf("recent TTL = %v", ttl)
	}
}

//go:build integration

package budget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisURL is where the integration tests find Redis 8.
func redisURL() (url string, explicit bool) {
	if u := os.Getenv("SWITCHBOARD_TEST_REDIS_URL"); u != "" {
		return u, true
	}
	return "redis://127.0.0.1:6379/15", false
}

// realRedis connects a new client to the test Redis, or skips when no URL
// was given and nothing listens on the default one. A configured URL that
// does not answer is a failure, so CI cannot pass by skipping.
func realRedis(t *testing.T) *redis.Client {
	t.Helper()
	url, explicit := redisURL()
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
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// testPrefix is a key prefix unique to this test, deleted when it ends.
func testPrefix(t *testing.T, rdb *redis.Client) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	prefix := "switchboard-test:" + hex.EncodeToString(b[:]) + ":"
	t.Cleanup(func() {
		ctx := context.Background()
		iter := rdb.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			_ = rdb.Del(ctx, iter.Val()).Err()
		}
	})
	return prefix
}

func TestIntegrationContract(t *testing.T) {
	rdb := realRedis(t)
	l := NewRedis(rdb, testPrefix(t, rdb), time.Minute, nil)
	ctx := context.Background()
	b := Budget{Limit: 100, Period: PeriodDaily}

	r := mustReserve(t, l, "k", b, "a", 80)
	if _, err := l.Reserve(ctx, "k", b, "b", 21); !errors.Is(err, ErrExhausted) {
		t.Fatalf("Reserve past limit: err = %v", err)
	}
	mustSettle(t, l, "k", b, r, 30)
	mustSettle(t, l, "k", b, Reservation{}, 5)
	wantPosition(t, mustBalance(t, l, "k", b), 35, 0)
}

func TestIntegrationConcurrentReplicas(t *testing.T) {
	first := realRedis(t)
	prefix := testPrefix(t, first)
	replicas := []Ledger{NewRedis(first, prefix, time.Minute, nil)}
	for range 2 {
		replicas = append(replicas, NewRedis(realRedis(t), prefix, time.Minute, nil))
	}
	concurrentProperty(t, replicas, "key")
}

// TestIntegrationExpiry checks the expiry charge and the late settle on a
// real Redis, whose scripts run on its own clock: the hold is charged at
// its held amount once it lapses, frees nothing, and the late settle
// corrects the charge to the real cost, once.
func TestIntegrationExpiry(t *testing.T) {
	rdb := realRedis(t)
	prefix := testPrefix(t, rdb)
	l := NewRedis(rdb, prefix, time.Second, nil)
	ctx := context.Background()
	b := Budget{Limit: 100, Period: PeriodMonthly}
	r := mustReserve(t, l, "k", b, "a", 70)
	wantPosition(t, mustBalance(t, l, "k", b), 0, 70)
	time.Sleep(1200 * time.Millisecond)
	wantPosition(t, mustBalance(t, l, "k", b), 70, 0)
	if _, err := l.Reserve(ctx, "k", b, "b", 31); !errors.Is(err, ErrExhausted) {
		t.Fatalf("Reserve past the expired hold's charge: err = %v", err)
	}
	tomb := l.keys("k", r.Window)[3]
	if ttl, err := rdb.TTL(ctx, tomb).Result(); err != nil || ttl < 69*24*time.Hour {
		t.Fatalf("tombstone TTL = %v, %v; want the monthly window's 70 days", ttl, err)
	}
	mustSettle(t, l, "k", b, r, 25)
	wantPosition(t, mustBalance(t, l, "k", b), 25, 0)
	mustSettle(t, l, "k", b, r, 5)
	wantPosition(t, mustBalance(t, l, "k", b), 30, 0)
	mustReserve(t, l, "k", b, "b", 70)
}

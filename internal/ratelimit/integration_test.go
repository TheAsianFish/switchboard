//go:build integration

package ratelimit_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/ratelimit/ratelimittest"
)

const defaultTestRedis = "redis://127.0.0.1:6379/15"

// realRedis connects to the test Redis. With SWITCHBOARD_TEST_REDIS_URL
// set, an unreachable server fails the test (CI asked for it); unset, the
// default address is tried and the test skips when nothing listens.
func realRedis(t *testing.T) *redis.Client {
	t.Helper()
	url, explicit := os.LookupEnv("SWITCHBOARD_TEST_REDIS_URL")
	if !explicit {
		url = defaultTestRedis
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("SWITCHBOARD_TEST_REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	probe.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		probe.err = rdb.Ping(ctx).Err()
	})
	if err := probe.err; err != nil {
		if explicit {
			t.Fatalf("redis at SWITCHBOARD_TEST_REDIS_URL unreachable: %v", err)
		}
		t.Skipf("no redis at %s: %v", url, err)
	}
	return rdb
}

// probe remembers whether the test Redis answered, so a missing server
// costs one timeout rather than one per test.
var probe struct {
	once sync.Once
	err  error
}

var prefixSeq atomic.Int64

// testPrefix is a key prefix unique to this test run whose keys are
// deleted when the test ends.
func testPrefix(t *testing.T, rdb *redis.Client) string {
	t.Helper()
	p := fmt.Sprintf("switchboard-test:%d:%d:", time.Now().UnixNano(), prefixSeq.Add(1))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		iter := rdb.Scan(ctx, 0, p+"*", 100).Iterator()
		for iter.Next(ctx) {
			_ = rdb.Del(ctx, iter.Val()).Err()
		}
		if err := iter.Err(); err != nil {
			t.Errorf("cleaning up %s*: %v", p, err)
		}
	})
	return p
}

// realHarness runs a Redis limiter on the real Redis clock.
func realHarness(newLimiter func(redis.Scripter, string) ratelimit.Limiter) func(t *testing.T) ratelimittest.Harness {
	return func(t *testing.T) ratelimittest.Harness {
		t.Helper()
		rdb := realRedis(t)
		return ratelimittest.Harness{Limiter: newLimiter(rdb, testPrefix(t, rdb))}
	}
}

// evalshaCalls reads the server's EVALSHA call count.
func evalshaCalls(t *testing.T, rdb *redis.Client) int64 {
	t.Helper()
	info, err := rdb.Info(context.Background(), "commandstats").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(info, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "cmdstat_evalsha:calls=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.SplitN(rest, ",", 2)[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	return 0
}

func TestIntegrationFixedWindowScripts(t *testing.T) {
	rdb := realRedis(t)
	ctx := context.Background()
	fw := ratelimit.NewFixedWindow(rdb, testPrefix(t, rdb))
	// A window of 1000 hours: no boundary falls inside the test.
	l := ratelimit.Limit{Rate: 3, Period: 1000 * time.Hour, Burst: 1}

	// After a flush the first call takes the EVAL fallback and loads the
	// script; the next goes through EVALSHA.
	if err := rdb.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		before := evalshaCalls(t, rdb)
		d, err := fw.Allow(ctx, "k", l, 1)
		if err != nil {
			t.Fatalf("Allow %d: %v", i, err)
		}
		if !d.Allowed || d.Remaining != int64(2-i) {
			t.Fatalf("Allow %d = %+v, want allowed with %d remaining", i, d, 2-i)
		}
		if i > 0 && evalshaCalls(t, rdb) <= before {
			t.Fatalf("call %d did not go through EVALSHA", i)
		}
	}

	d, err := fw.Allow(ctx, "k", l, 1)
	if err != nil {
		t.Fatal(err)
	}
	// TIME drives the window: Retry-After is the time to the next
	// multiple of the period since the epoch, on this host's clock too.
	p := l.Period.Microseconds()
	now := time.Now().UnixMicro()
	want := time.Duration((now/p+1)*p-now) * time.Microsecond
	if d.Allowed || d.Remaining != 0 || d.RetryAfter <= 0 ||
		d.RetryAfter > want+5*time.Second || d.RetryAfter < want-5*time.Second {
		t.Fatalf("fourth Allow = %+v, want refused with Retry-After about %v", d, want)
	}
	if d, err := fw.Allow(ctx, "k", l, 4); err != nil || d.RetryAfter != ratelimit.Never {
		t.Fatalf("Allow above rate = %+v, %v; want Never", d, err)
	}

	if err := fw.Adjust(ctx, "k", l, -2); err != nil {
		t.Fatal(err)
	}
	if d, err := fw.Allow(ctx, "k", l, 2); err != nil || !d.Allowed {
		t.Fatalf("Allow after a refund of 2 = %+v, %v", d, err)
	}
}

func TestIntegrationFixedWindowContract(t *testing.T) {
	ratelimittest.RunContract(t, "fixed_window/redis", realHarness(newFixedWindow))
}

func TestIntegrationFixedWindowConcurrentReplicas(t *testing.T) {
	testConcurrentReplicas(t, newFixedWindow, func(l ratelimit.Limit) int64 { return l.Rate })
}

func TestIntegrationGCRAContract(t *testing.T) {
	ratelimittest.RunContract(t, "gcra/redis", realHarness(newGCRA))
}

func TestIntegrationGCRAConcurrentReplicas(t *testing.T) {
	testConcurrentReplicas(t, newGCRA, func(l ratelimit.Limit) int64 { return l.Burst })
}

// testConcurrentReplicas fires 300 one-unit requests through three
// clients, one per replica, at a limit that refills nothing during the
// test, and expects exactly want(l) admitted.
func testConcurrentReplicas(t *testing.T, newLimiter func(redis.Scripter, string) ratelimit.Limiter, want func(ratelimit.Limit) int64) {
	t.Helper()
	first := realRedis(t)
	prefix := testPrefix(t, first)
	replicas := []ratelimit.Limiter{newLimiter(first, prefix)}
	for range 2 {
		replicas = append(replicas, newLimiter(realRedis(t), prefix))
	}
	l := ratelimit.Limit{Rate: 100, Period: 1000 * time.Hour, Burst: 20}
	if got := admitConcurrently(t, replicas, "shared", l, 300); got != want(l) {
		t.Fatalf("admitted %d across three replicas, want %d", got, want(l))
	}
}

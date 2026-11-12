//go:build integration

package cache_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/redistest"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// recordingKV records the errors of the store it wraps.
type recordingKV struct {
	cache.KV
	errs *redistest.Errors
}

func (r recordingKV) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, ok, err := r.KV.Get(ctx, key)
	return v, ok, r.errs.Add(err)
}

func (r recordingKV) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	return r.errs.Add(r.KV.Set(ctx, key, val, ttl))
}

// lockedBuffer is a log destination safe for the cache's background fills.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A Redis that answers LOADING (restarting, reading its dataset back) is
// an unreachable store to the cache: lookups bypass it without an error
// reaching the caller, and once the load is over the entry stored before
// it is served again. Both ends are logged at warn. Not parallel: the
// reload refuses every client of the server.
func TestIntegrationLoadingBypassesAndRecovers(t *testing.T) {
	srv := redistest.Loading(t)
	rdb := srv.Client()
	errs := &redistest.Errors{}
	logs := &lockedBuffer{}
	ctx := telemetry.WithRequest(context.Background(), slog.New(slog.NewTextHandler(logs, nil)), "req_test")
	c := cache.New(cache.Options{
		KV:            recordingKV{cache.NewRedisKV(rdb, testPrefix()), errs},
		TTL:           time.Minute,
		Timeout:       500 * time.Millisecond,
		RetryInterval: 50 * time.Millisecond,
	})
	up := cache.Upstream{Provider: "p", Model: "m"}
	r := &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "What is the capital of France?"}}}
	lookup := func() *cache.Lookup { return c.Lookup(ctx, "tenant", up, cache.ModeExact, r) }

	l := lookup()
	if l.Status != cache.StatusMiss {
		t.Fatalf("first lookup: %s (%s), want miss", l.Status, l.Reason)
	}
	c.Fill(ctx, l, &core.Response{Text: "Paris.", StopReason: core.StopEndTurn})
	c.Wait()
	if l := lookup(); l.Status != cache.StatusExact {
		t.Fatalf("lookup after the fill: %s (%s), want hit", l.Status, l.Reason)
	}

	srv.StartLoading()
	if l := lookup(); l.Status != cache.StatusError || l.Fillable() {
		t.Fatalf("lookup during LOADING: %s (%s), want error, not fillable", l.Status, l.Reason)
	}
	if l := lookup(); l.Status != cache.StatusSkip {
		t.Fatalf("lookup in the bypass: %s (%s), want skip", l.Status, l.Reason)
	}
	// A lookup after the retry interval tries the store and meets LOADING
	// again.
	time.Sleep(60 * time.Millisecond)
	if l := lookup(); l.Status != cache.StatusError {
		t.Fatalf("lookup after the interval during LOADING: %s (%s), want error", l.Status, l.Reason)
	}
	srv.EndLoading()
	failures := errs.RequireLoading(t)
	if len(failures) < 2 {
		t.Fatalf("store errors during the reload = %v, want two lookups'", failures)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(60 * time.Millisecond)
		l := lookup()
		if l.Status == cache.StatusExact {
			break
		}
		if l.Status != cache.StatusSkip || time.Now().After(deadline) {
			t.Fatalf("lookup after the reload: %s (%s), want the entry stored before it", l.Status, l.Reason)
		}
	}
	if extra := errs.All(); len(extra) > 0 {
		t.Fatalf("store errors after the reload: %v", extra)
	}
	out := logs.String()
	for _, want := range []struct {
		line string
		n    int
	}{
		{`level=WARN msg="cache store failed; bypassing the cache" request_id=req_test op=get error="cache: get: LOADING `, len(failures)},
		{`level=WARN msg="cache store reachable again" request_id=req_test op=get`, 1},
	} {
		if n := strings.Count(out, want.line); n != want.n {
			t.Errorf("%d log lines with %s, want %d; logs:\n%s", n, want.line, want.n, out)
		}
	}
}

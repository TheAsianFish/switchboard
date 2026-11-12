// Package redistest puts a real Redis 8 into the state it is in while it
// loads its dataset after a restart, when every command is refused with
// "LOADING Redis is loading the dataset in memory". The integration tests
// of the stores that degrade (budget, rate limits, cache) use it to check
// that a LOADING reply degrades like an unreachable store and recovers.
//
// LOADING is produced for real, not faked: DEBUG RELOAD saves the dataset
// and loads it back, the hidden debug config key-load-delay slows the load
// down to a known length (it sleeps that many microseconds per key), and
// loading-process-events-interval-bytes makes the server answer other
// clients during the load rather than only every 2 MB of RDB, which a
// small test dataset never reaches. DEBUG needs a server started with
// --enable-debug-command yes; the flag cannot be set at runtime.
//
// A reload refuses every client of the server, so it must not run on the
// Redis other integration tests share: go test runs packages in parallel.
// The tests use a server of their own, named by LoadingURLEnv
// (scripts/with-redis.sh starts one), and the packages that share it take
// turns through a lock key on it.
package redistest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// LoadingURLEnv names the Redis the LOADING tests may reload. It must not
// be the server the other integration tests use.
const LoadingURLEnv = "SWITCHBOARD_TEST_REDIS_LOADING_URL"

const (
	// fillKeys keys are loaded at keyLoadDelay microseconds each, so a
	// reload refuses commands for about fillKeys × keyLoadDelay µs (1.2 s
	// plus sleep overhead).
	fillKeys     = 1200
	keyLoadDelay = 1000
	fillPrefix   = "redistest:fill"
	// The server answers other clients after every this many bytes of
	// RDB read, every few dozen keys here; 1024 is the least it accepts.
	eventsInterval = 1024

	lockKey = "redistest:loading-lock"
	// lockTTL outlives any one test, and frees the server if a test
	// process dies holding the lock.
	lockTTL = time.Minute
	// lockWait is how long a test waits for the packages ahead of it.
	lockWait = 2 * time.Minute
)

// Server is a Redis the test holds alone and may reload.
type Server struct {
	t     testing.TB
	opts  *redis.Options
	admin *redis.Client
	// reloading carries the result of the reload in progress, if any.
	reloading chan error
}

// Loading returns the server at LoadingURLEnv, held by this test until it
// ends. It skips the test when the variable is unset, and fails it when the
// server refuses DEBUG: a server named for these tests that cannot run them
// must not turn the job green with the LOADING path untested. When the test ends the server is reloaded no more, its debug
// configuration is restored and the next test may have it.
//
// Tests that call it must not be parallel: a reload refuses every other
// test in the package as well.
func Loading(t testing.TB) *Server {
	t.Helper()
	url := os.Getenv(LoadingURLEnv)
	if url == "" {
		t.Skipf("%s is not set: the LOADING tests need a Redis 8 of their own started with --enable-debug-command yes (make test-integration starts one)", LoadingURLEnv)
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("%s: %v", LoadingURLEnv, err)
	}
	aopts := *opts
	// DEBUG RELOAD answers when the load is done; nothing is retried.
	aopts.ReadTimeout = 30 * time.Second
	aopts.MaxRetries = -1
	s := &Server{t: t, opts: opts, admin: redis.NewClient(&aopts)}
	t.Cleanup(func() { _ = s.admin.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), lockWait+10*time.Second)
	defer cancel()
	if err := s.admin.Do(ctx, "DEBUG", "SLEEP", "0").Err(); err != nil {
		if strings.Contains(err.Error(), "DEBUG command not allowed") {
			t.Fatalf("Redis at %s refuses DEBUG; start it with --enable-debug-command yes: %v", LoadingURLEnv, err)
		}
		t.Fatalf("Redis at %s: %v", LoadingURLEnv, err)
	}
	s.lock(ctx)

	restore := map[string]string{}
	for _, k := range []string{"key-load-delay", "loading-process-events-interval-bytes"} {
		v, err := s.admin.ConfigGet(ctx, k).Result()
		if err != nil || v[k] == "" {
			t.Fatalf("CONFIG GET %s: %v %v", k, v, err)
		}
		restore[k] = v[k]
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = s.wait() // a failed reload is reported by EndLoading
		for k, v := range restore {
			if err := s.admin.ConfigSet(ctx, k, v).Err(); err != nil {
				t.Errorf("restore %s: %v", k, err)
			}
		}
	})
	for k, v := range map[string]int{"key-load-delay": keyLoadDelay, "loading-process-events-interval-bytes": eventsInterval} {
		if err := s.admin.ConfigSet(ctx, k, strconv.Itoa(v)).Err(); err != nil {
			t.Fatalf("CONFIG SET %s: %v", k, err)
		}
	}
	// POPULATE leaves existing keys alone, so this is cheap after the
	// first test.
	if err := s.admin.Do(ctx, "DEBUG", "POPULATE", fillKeys, fillPrefix, 16).Err(); err != nil {
		t.Fatalf("DEBUG POPULATE: %v", err)
	}
	return s
}

// lock takes the server from the other packages' LOADING tests and
// registers its release. A server busy loading for one of them refuses the
// SET too; that is waited out like a held lock.
func (s *Server) lock(ctx context.Context) {
	s.t.Helper()
	var b [8]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	deadline := time.Now().Add(lockWait)
	for {
		ok, err := s.admin.SetNX(ctx, lockKey, token, lockTTL).Result()
		if err == nil && ok {
			break
		}
		if err != nil && !redis.IsLoadingError(err) {
			s.t.Fatalf("lock %s: %v", lockKey, err)
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("lock %s: still held by another test after %s", lockKey, lockWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Cleanup(func() {
		ctx := context.Background()
		_ = s.wait()
		if v, err := s.admin.Get(ctx, lockKey).Result(); err == nil && v == token {
			_ = s.admin.Del(ctx, lockKey).Err()
		}
	})
}

// Client returns a client configured like the gateway's: no retries (a
// LOADING reply reaches the caller instead of being retried) and short
// timeouts. It is closed when the test ends.
func (s *Server) Client() *redis.Client {
	opts := *s.opts
	opts.MaxRetries = -1
	opts.DialTimeout = time.Second
	opts.ReadTimeout = time.Second
	opts.WriteTimeout = time.Second
	opts.PoolTimeout = time.Second
	c := redis.NewClient(&opts)
	s.t.Cleanup(func() { _ = c.Close() })
	return c
}

// StartLoading starts a reload and returns once the server has refused a
// command with LOADING, so the caller's next commands meet it too (the
// load lasts about a second). Call EndLoading to wait for the end.
func (s *Server) StartLoading() {
	s.t.Helper()
	if s.reloading != nil {
		s.t.Fatal("redistest: StartLoading while a reload is in progress")
	}
	done := make(chan error, 1)
	s.reloading = done
	go func() { done <- s.admin.Do(context.Background(), "DEBUG", "RELOAD").Err() }()
	// A test that fails during the load must not clean up before it is
	// over: cleanups registered before this one (key deletion) would meet
	// LOADING.
	s.t.Cleanup(func() { _ = s.wait() })

	probe := s.Client()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := probe.Get(context.Background(), lockKey).Err()
		if redis.IsLoadingError(err) {
			return
		}
		select {
		case err := <-done:
			s.reloading = nil
			s.t.Fatalf("redistest: DEBUG RELOAD ended (%v) before LOADING was seen", err)
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("redistest: no LOADING reply within 5s of DEBUG RELOAD (last: %v)", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// EndLoading waits for the reload started by StartLoading to finish and
// fails the test if it did not succeed. The server then answers again.
func (s *Server) EndLoading() {
	s.t.Helper()
	if err := s.wait(); err != nil {
		s.t.Fatalf("DEBUG RELOAD: %v", err)
	}
}

// wait waits for the reload in progress, if any, and returns its result.
func (s *Server) wait() error {
	if s.reloading == nil {
		return nil
	}
	err := <-s.reloading
	s.reloading = nil
	return err
}

// IsLoading reports whether err is a Redis LOADING reply, however wrapped.
func IsLoading(err error) bool { return err != nil && redis.IsLoadingError(err) }

// Errors records the errors a store returned, so a test can check that
// the failure it degraded on really was LOADING and not a timeout. It is
// safe for concurrent use.
type Errors struct {
	mu   sync.Mutex
	errs []error
}

// Add records err if it is not nil, and returns it.
func (e *Errors) Add(err error) error {
	if err != nil {
		e.mu.Lock()
		e.errs = append(e.errs, err)
		e.mu.Unlock()
	}
	return err
}

// All returns the errors recorded so far, in order, and forgets them.
func (e *Errors) All() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.errs
	e.errs = nil
	return out
}

// RequireLoading fails t unless at least one error was recorded and every
// one of them was a LOADING reply, and returns them.
func (e *Errors) RequireLoading(t testing.TB) []error {
	t.Helper()
	all := e.All()
	if len(all) == 0 {
		t.Fatal("the store returned no error during the reload")
	}
	var other []error
	for _, err := range all {
		if !IsLoading(err) {
			other = append(other, err)
		}
	}
	if len(other) > 0 {
		t.Fatalf("%d of %d store errors were not LOADING: %v", len(other), len(all), errors.Join(other...))
	}
	return all
}

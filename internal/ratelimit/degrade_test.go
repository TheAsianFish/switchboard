package ratelimit_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/ratelimit/ratelimittest"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// stubLimiter is a Primary whose failure mode the test controls.
type stubLimiter struct {
	mu    sync.Mutex
	err   error
	block bool // wait for the context instead of answering
	calls int
	// sawDeadline records whether the last call carried a deadline.
	sawDeadline bool
}

var errStore = errors.New("store down")

// primaryRemaining marks decisions that came from the stub.
const primaryRemaining = 4242

func (s *stubLimiter) set(err error, block bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err, s.block = err, block
}

func (s *stubLimiter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubLimiter) call(ctx context.Context) error {
	s.mu.Lock()
	s.calls++
	_, s.sawDeadline = ctx.Deadline()
	err, block := s.err, s.block
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (s *stubLimiter) Allow(ctx context.Context, _ string, _ ratelimit.Limit, _ int64) (ratelimit.Decision, error) {
	if err := s.call(ctx); err != nil {
		return ratelimit.Decision{}, err
	}
	return ratelimit.Decision{Allowed: true, Remaining: primaryRemaining}, nil
}

func (s *stubLimiter) Adjust(ctx context.Context, _ string, _ ratelimit.Limit, _ int64) error {
	return s.call(ctx)
}

// degradeFixture is a Degrading over a stub primary and an in-memory
// local limiter sharing one fake clock, recording transitions and logs.
type degradeFixture struct {
	d       *ratelimit.Degrading
	primary *stubLimiter
	clock   *ratelimittest.Clock
	logs    *syncBuffer
	ctx     context.Context

	mu     sync.Mutex
	states []bool
}

func newDegradeFixture(replicas int) *degradeFixture {
	f := &degradeFixture{primary: &stubLimiter{}, clock: ratelimittest.NewClock(ratelimittest.Start), logs: &syncBuffer{}}
	f.d = ratelimit.NewDegrading(f.primary, ratelimit.NewMemoryFixedWindow(f.clock.Now), replicas)
	f.d.Now = f.clock.Now
	f.d.OnState = func(degraded bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.states = append(f.states, degraded)
	}
	f.ctx = telemetry.WithRequest(context.Background(), slog.New(slog.NewTextHandler(f.logs, nil)), "req_test")
	return f
}

func (f *degradeFixture) transitions() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.states...)
}

func (f *degradeFixture) advance(dt time.Duration) { f.clock.Set(f.clock.Now().Add(dt)) }

// syncBuffer is a bytes.Buffer safe for the concurrent test's logging.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var q2Limit = ratelimit.Limit{Rate: 100, Period: time.Minute, Burst: 20}

func TestDegradingLocalLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		replicas    int
		rate, burst int64
	}{
		{0, 100, 20},
		{1, 100, 20},
		{3, 33, 6},
		{7, 14, 2},
		{1000, 1, 1},
	} {
		d := ratelimit.NewDegrading(nil, nil, tc.replicas)
		got := d.LocalLimit(q2Limit)
		if got.Rate != tc.rate || got.Burst != tc.burst || got.Period != q2Limit.Period {
			t.Errorf("replicas %d: LocalLimit = %+v, want rate %d burst %d", tc.replicas, got, tc.rate, tc.burst)
		}
	}
}

func TestDegradingFallsBackAndRecovers(t *testing.T) {
	t.Parallel()
	f := newDegradeFixture(3)
	f.primary.set(errStore, false)

	// The first failure degrades; the local limiter then enforces the
	// fixed window at 100/3 = 33 per minute.
	var admitted int64
	for range 50 {
		d, err := f.d.Allow(f.ctx, "k", q2Limit, 1)
		if err != nil {
			t.Fatalf("Allow while degraded: %v", err)
		}
		if d.Remaining == primaryRemaining {
			t.Fatal("decision came from the failed primary")
		}
		if d.Allowed {
			admitted++
		}
	}
	if admitted != 33 {
		t.Fatalf("admitted %d while degraded, want 33", admitted)
	}
	if !f.d.Degraded() {
		t.Fatal("not degraded after a primary failure")
	}
	if calls := f.primary.callCount(); calls != 1 {
		t.Fatalf("primary called %d times within the retry interval, want 1", calls)
	}

	// Within RetryInterval the primary is left alone; at it, one probe.
	f.advance(ratelimit.DefaultRetryInterval - time.Millisecond)
	if _, err := f.d.Allow(f.ctx, "k", q2Limit, 1); err != nil {
		t.Fatal(err)
	}
	if calls := f.primary.callCount(); calls != 1 {
		t.Fatalf("primary probed before the retry interval: %d calls", calls)
	}
	f.advance(time.Millisecond)
	for range 5 {
		if _, err := f.d.Allow(f.ctx, "k", q2Limit, 1); err != nil {
			t.Fatal(err)
		}
	}
	if calls := f.primary.callCount(); calls != 2 {
		t.Fatalf("primary called %d times after one retry interval, want 2 (one probe)", calls)
	}
	if !f.d.Degraded() {
		t.Fatal("a failed probe ended the degraded state")
	}

	// The store comes back: the next probe recovers.
	f.primary.set(nil, false)
	f.advance(ratelimit.DefaultRetryInterval)
	d, err := f.d.Allow(f.ctx, "k", q2Limit, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Remaining != primaryRemaining || f.d.Degraded() {
		t.Fatalf("after recovery: decision %+v, degraded %v; want the primary's decision", d, f.d.Degraded())
	}
	if got := f.transitions(); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("transitions = %v, want [true false]", got)
	}
	logs := f.logs.String()
	if strings.Count(logs, "level=WARN") != 1 || strings.Count(logs, "level=INFO") != 1 {
		t.Fatalf("want one warning and one info line, got:\n%s", logs)
	}
	if !strings.Contains(logs, "request_id=req_test") {
		t.Fatalf("log lines lack the request id:\n%s", logs)
	}
}

func TestDegradingCustomRetryInterval(t *testing.T) {
	t.Parallel()
	f := newDegradeFixture(1)
	f.d.RetryInterval = 10 * time.Second
	f.primary.set(errStore, false)
	if _, err := f.d.Allow(f.ctx, "k", q2Limit, 1); err != nil {
		t.Fatal(err)
	}
	f.primary.set(nil, false)
	f.advance(9 * time.Second)
	if d, _ := f.d.Allow(f.ctx, "k", q2Limit, 1); d.Remaining == primaryRemaining {
		t.Fatal("primary used before RetryInterval")
	}
	f.advance(time.Second)
	if d, _ := f.d.Allow(f.ctx, "k", q2Limit, 1); d.Remaining != primaryRemaining {
		t.Fatal("primary not probed at RetryInterval")
	}
}

func TestDegradingAdjust(t *testing.T) {
	t.Parallel()
	f := newDegradeFixture(3)
	f.primary.set(errStore, false)
	// A charge while degraded lands on the local limiter at its limit.
	if err := f.d.Adjust(f.ctx, "k", q2Limit, 30); err != nil {
		t.Fatal(err)
	}
	if !f.d.Degraded() {
		t.Fatal("a failed Adjust did not degrade")
	}
	var admitted int64
	for range 10 {
		d, err := f.d.Allow(f.ctx, "k", q2Limit, 1)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("admitted %d after a local charge of 30 of 33, want 3", admitted)
	}
}

func TestDegradingCallerErrorsDoNotDegrade(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		primary ratelimit.Limiter
		cost    int64
		wantErr error
	}{
		{"cancelled", cancelled, &stubLimiter{}, 1, context.Canceled},
		{"expired", expired, &stubLimiter{}, 1, context.DeadlineExceeded},
		{"not implemented", context.Background(), ratelimit.NewMemoryGCRA(nil), 1, ratelimit.ErrNotImplemented},
		{"invalid cost", context.Background(), &stubLimiter{err: errStore}, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var transitions atomic.Int64
			d := ratelimit.NewDegrading(tc.primary, ratelimit.NewMemoryFixedWindow(nil), 3)
			d.OnState = func(bool) { transitions.Add(1) }
			_, err := d.Allow(tc.ctx, "k", q2Limit, tc.cost)
			if err == nil {
				t.Fatal("Allow returned no error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.cost == 1 {
				if aerr := d.Adjust(tc.ctx, "k", q2Limit, 1); !errors.Is(aerr, tc.wantErr) {
					t.Fatalf("Adjust err = %v, want %v", aerr, tc.wantErr)
				}
			}
			if d.Degraded() || transitions.Load() != 0 {
				t.Fatalf("degraded = %v after %d transitions; the store did not fail", d.Degraded(), transitions.Load())
			}
			if s, ok := tc.primary.(*stubLimiter); ok && tc.cost < 1 && s.callCount() != 0 {
				t.Fatal("primary called with an invalid cost")
			}
		})
	}
}

func TestDegradingTimeout(t *testing.T) {
	t.Parallel()
	f := newDegradeFixture(3)
	f.d.Timeout = 20 * time.Millisecond
	f.primary.set(nil, true)
	start := time.Now()
	d, err := f.d.Allow(f.ctx, "k", q2Limit, 1)
	if err != nil {
		t.Fatalf("Allow with a hung primary: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Allow took %v with a 20ms timeout", elapsed)
	}
	if !d.Allowed || d.Remaining == primaryRemaining || !f.d.Degraded() {
		t.Fatalf("decision %+v, degraded %v; want a local admission", d, f.d.Degraded())
	}
	if !f.primary.sawDeadline {
		t.Fatal("primary call carried no deadline")
	}
}

func TestDegradingDefaultTimeoutApplied(t *testing.T) {
	t.Parallel()
	primary := &stubLimiter{}
	d := &ratelimit.Degrading{Primary: primary, Local: ratelimit.NewMemoryFixedWindow(nil)}
	if _, err := d.Allow(context.Background(), "k", q2Limit, 1); err != nil {
		t.Fatal(err)
	}
	if !primary.sawDeadline {
		t.Fatal("a zero Timeout left the primary call without a deadline")
	}
}

// TestDegradingMiniredis runs the policy over the real fixed-window
// limiter: the store goes away mid-run and comes back.
func TestDegradingMiniredis(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	m.SetTime(ratelimittest.Start)
	clock := ratelimittest.NewClock(ratelimittest.Start)
	d := ratelimit.NewDegrading(ratelimit.NewFixedWindow(newClient(t, m), "test:"), ratelimit.NewMemoryFixedWindow(clock.Now), 3)
	d.Now = clock.Now
	// Generous: under -race on a busy machine the first script call to
	// miniredis can outlast the 100 ms default, which degrades the
	// limiter while the store is up.
	d.Timeout = time.Second
	ctx := context.Background()
	l := ratelimit.Limit{Rate: 6, Period: time.Hour, Burst: 6}

	count := func(n int) (admitted int) {
		t.Helper()
		for range n {
			dec, err := d.Allow(ctx, "k", l, 1)
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if dec.Allowed {
				admitted++
			}
		}
		return admitted
	}
	if got := count(4); got != 4 || d.Degraded() {
		t.Fatalf("store up: admitted %d of 4, degraded %v", got, d.Degraded())
	}
	m.Close()
	if got := count(5); got != 2 || !d.Degraded() {
		t.Fatalf("store down: admitted %d, degraded %v; want the local 6/3 = 2", got, d.Degraded())
	}
	if err := m.Restart(); err != nil {
		t.Fatal(err)
	}
	clock.Set(ratelimittest.Start.Add(ratelimit.DefaultRetryInterval))
	// The counter survived in miniredis: 4 used, 2 left.
	if got := count(5); got != 2 || d.Degraded() {
		t.Fatalf("store back: admitted %d, degraded %v; want the shared 2 left", got, d.Degraded())
	}
}

func TestDegradingConcurrent(t *testing.T) {
	t.Parallel()
	f := newDegradeFixture(3)
	f.d.RetryInterval = time.Nanosecond
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				switch {
				case i == 0 && j%20 == 0:
					f.primary.set(errStore, false)
				case i == 0 && j%20 == 10:
					f.primary.set(nil, false)
				case i == 1:
					f.advance(time.Millisecond)
				}
				if _, err := f.d.Allow(f.ctx, "k", q2Limit, 1); err != nil {
					t.Error(err)
					return
				}
				if err := f.d.Adjust(f.ctx, "k", q2Limit, -1); err != nil {
					t.Error(err)
					return
				}
				_ = f.d.Degraded()
			}
		})
	}
	wg.Wait()
	// Transitions alternate, starting with entering degraded mode.
	for i, s := range f.transitions() {
		if s != (i%2 == 0) {
			t.Fatalf("transition %d = %v out of order: %v", i, s, f.transitions())
		}
	}
}

// A request the full limit would take but this replica's share cannot is
// the outage's fault: it is refused as a wait, never as impossible.
func TestDegradingShareTooSmallIsAWait(t *testing.T) {
	t.Parallel()
	f := newDegradeFixture(3)
	f.primary.set(errStore, false)
	l := ratelimit.Limit{Rate: 1000, Period: time.Minute, Burst: 1000}
	d, err := f.d.Allow(f.ctx, "tok", l, 505) // more than the local 333
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.RetryAfter != ratelimit.DefaultRetryInterval {
		t.Errorf("decision %+v, want a refusal retrying after the probe interval", d)
	}
}

// A request above the full limit cannot be admitted by any replica, with
// or without Redis, so the outage does not turn it into a wait.
func TestDegradingTooBigForTheFullLimitIsNever(t *testing.T) {
	t.Parallel()
	f := newDegradeFixture(3)
	f.primary.set(errStore, false)
	l := ratelimit.Limit{Rate: 1000, Period: time.Minute, Burst: 1000}
	d, err := f.d.Allow(f.ctx, "tok", l, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.RetryAfter != ratelimit.Never {
		t.Errorf("decision %+v, want a refusal with RetryAfter Never", d)
	}
}

// gatedLimiter answers its first call only when released, successfully,
// and fails every later call.
type gatedLimiter struct {
	ratelimit.Limiter
	started, release chan struct{}
	calls            atomic.Int64
}

func (g *gatedLimiter) Allow(ctx context.Context, _ string, _ ratelimit.Limit, _ int64) (ratelimit.Decision, error) {
	if g.calls.Add(1) == 1 {
		close(g.started)
		<-g.release
		return ratelimit.Decision{Allowed: true, Remaining: primaryRemaining}, nil
	}
	return ratelimit.Decision{}, errStore
}

// A slow call that started before the store failed and succeeds after it
// must not end the degraded state; only a probe can.
func TestDegradingStaleSuccessDoesNotRecover(t *testing.T) {
	t.Parallel()
	g := &gatedLimiter{started: make(chan struct{}), release: make(chan struct{})}
	clock := ratelimittest.NewClock(ratelimittest.Start)
	d := ratelimit.NewDegrading(g, ratelimit.NewMemoryFixedWindow(clock.Now), 3)
	d.Now, d.Timeout = clock.Now, time.Minute
	var states []bool
	var mu sync.Mutex
	d.OnState = func(degraded bool) { mu.Lock(); states = append(states, degraded); mu.Unlock() }
	ctx := context.Background()

	slow := make(chan error)
	go func() { _, err := d.Allow(ctx, "k", q2Limit, 1); slow <- err }()
	<-g.started
	if _, err := d.Allow(ctx, "k", q2Limit, 1); err != nil {
		t.Fatal(err)
	}
	if !d.Degraded() {
		t.Fatal("not degraded after a failure")
	}
	close(g.release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	if !d.Degraded() {
		t.Error("a success that started before the failure ended the degraded state")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 1 || !states[0] {
		t.Errorf("transitions = %v, want [true]", states)
	}
}

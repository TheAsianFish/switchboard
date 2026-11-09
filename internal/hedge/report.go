package hedge

import (
	"context"
	"sync"
)

// Report says what the resilience layer did for one request: how many
// attempts reached the provider, whether one was a hedge and which won, and
// how many were cancelled after they were sent. The pipeline reads it to
// charge cancelled attempts and to log the call.
type Report struct {
	mu        sync.Mutex
	attempts  int
	retries   int
	hedged    bool
	hedgeWon  bool
	cancelled int
}

type reportKey struct{}

// WithReport returns a context carrying a fresh report.
func WithReport(ctx context.Context) (context.Context, *Report) {
	r := &Report{}
	return context.WithValue(ctx, reportKey{}, r), r
}

// ReportFrom returns the context's report, or nil. Every method is safe on
// a nil report.
func ReportFrom(ctx context.Context) *Report {
	r, _ := ctx.Value(reportKey{}).(*Report)
	return r
}

// Snapshot is a report's values at one moment.
type Snapshot struct {
	// Attempts is the number of requests sent to the provider.
	Attempts int
	// Retries is the number of times the request was tried again after a
	// failure.
	Retries int
	// Hedged is true when a second attempt was sent while the first was
	// still waiting for its first token; HedgeWon when that one answered
	// first.
	Hedged, HedgeWon bool
	// Cancelled is the number of attempts that were sent and then
	// cancelled because another one answered first. The upstream may have
	// read their prompts and billed for them.
	Cancelled int
}

// Snapshot returns the report's current values.
func (r *Report) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{Attempts: r.attempts, Retries: r.retries, Hedged: r.hedged, HedgeWon: r.hedgeWon, Cancelled: r.cancelled}
}

func (r *Report) update(f func(r *Report)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	f(r)
	r.mu.Unlock()
}

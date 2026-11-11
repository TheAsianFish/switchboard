package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/hedge"
	"github.com/TheAsianFish/switchboard/internal/router"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Call is what the pipeline reports about one finished request to
// Policy.OnCall: who asked, what answered, how it ended, how long it took
// and what it cost. It never holds prompt or reply text.
type Call struct {
	Time      time.Time
	RequestID string
	TraceID   string
	Key       string
	// RequestedModel is the model the client asked for; Model is the
	// route that served it (they differ for auto). Model is empty when
	// the request was refused before a route was found.
	RequestedModel string
	Model          string
	Provider       string
	Stream         bool
	// Admitted is false for a request refused before it reached the
	// cache or a provider; its Duration is then zero, not measured.
	Admitted bool
	// Outcome is one of the telemetry Outcome constants.
	Outcome string
	// ErrorKind is the error's kind for outcome error, empty otherwise.
	ErrorKind string
	// Cache is the lookup's status, empty when no lookup ran.
	Cache cache.Status
	// Router is the router's decision for an auto request, nil otherwise.
	Router *router.Decision
	// Duration is from admission to the end of the reply; TTFT is to the
	// first token of a stream, zero otherwise.
	Duration, TTFT time.Duration
	Usage          core.Usage
	// Spent is what the call was charged; Saved is what a cache hit would
	// have cost upstream.
	Spent, Saved cost.Nanos
	Attempts     int
	Hedged       bool
}

// observe reports a finished call to Policy.OnCall, if set.
func (p *Pipeline) observe(ctx context.Context, c Call) {
	if p.policy.OnCall == nil {
		return
	}
	c.Time = p.now()
	c.RequestID = telemetry.RequestID(ctx)
	c.TraceID = telemetry.TraceID(ctx)
	if r := hedge.ReportFrom(ctx).Snapshot(); r.Attempts > 0 {
		c.Attempts, c.Hedged = r.Attempts, r.Hedged
	}
	p.policy.OnCall(c)
}

// observeRefused reports a request that never reached a provider: a bad
// model, a refused limit or budget, a malformed cache directive.
func (p *Pipeline) observeRefused(ctx context.Context, pr *auth.Principal, req *core.Request, rt Route, dec *router.Decision, err error) {
	if p.policy.OnCall == nil || pr == nil {
		return
	}
	c := Call{Key: pr.Name, RequestedModel: req.Model, Model: rt.Model, Provider: rt.ProviderName, Stream: req.Stream, Router: dec}
	c.Outcome, c.ErrorKind = outcomeOf(err)
	p.observe(ctx, c)
}

// outcomeOf maps a request's error to its outcome and error kind.
func outcomeOf(err error) (outcome, kind string) {
	switch {
	case err == nil:
		return telemetry.OutcomeOK, ""
	case errors.Is(err, errStreamAbandoned):
		return telemetry.OutcomeAbandoned, ""
	}
	switch k := core.AsError(err).Kind; k {
	case core.KindRateLimited:
		return telemetry.OutcomeRateLimited, ""
	case core.KindBudgetExceeded:
		return telemetry.OutcomeBudgetExhausted, ""
	default:
		return telemetry.OutcomeError, k.String()
	}
}

// callOf starts a Call for an admitted request.
func callOf(adm *admission, req *core.Request, stream bool, lk *cache.Lookup) Call {
	c := Call{
		Key: adm.pr.Name, RequestedModel: req.Model, Model: adm.route.Model, Provider: adm.route.ProviderName,
		Stream: stream, Router: adm.decision, Admitted: true,
	}
	if lk != nil {
		c.Cache = lk.Status
	}
	return c
}

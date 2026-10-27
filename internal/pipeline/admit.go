package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Policy is the shared-state part of the pipeline: limits, budgets and
// metrics. A zero Policy enforces nothing and records nothing.
type Policy struct {
	// Limiter enforces the keys' request and token limits.
	Limiter ratelimit.Limiter
	// Ledger enforces the keys' budgets.
	Ledger  budget.Ledger
	Metrics *telemetry.Metrics
	// SettleTimeout bounds the store calls made after a reply, which run
	// even when the client has gone; zero means DefaultSettleTimeout.
	SettleTimeout time.Duration
}

// DefaultSettleTimeout bounds settlement after a reply.
const DefaultSettleTimeout = 2 * time.Second

// admission is what a request was admitted on: the estimate it was charged
// against the token limit and the budget reservation it holds. settle
// corrects both to the real usage.
type admission struct {
	pr       *auth.Principal
	route    Route
	estimate core.Usage
	// tokens is what the token limit was charged, 0 without a token limit.
	tokens      int64
	reservation *budget.Reservation
	start       time.Time
}

func requestsKey(pr *auth.Principal) string { return "req:" + pr.Name }
func tokensKey(pr *auth.Principal) string   { return "tok:" + pr.Name }

// admit runs the rate limits and the budget reservation, in that order,
// and undoes what it charged if a later check refuses. The checks are
// separate store calls, so a refusal by a later one leaves a short window
// in which an earlier charge counted against the key.
func (p *Pipeline) admit(ctx context.Context, pr *auth.Principal, rt Route, req *core.Request) (*admission, error) {
	a := &admission{pr: pr, route: rt, estimate: cost.Estimate(req, rt.OutputEstimate), start: p.now()}
	lim := p.policy.Limiter
	if lim != nil && pr.Requests != nil {
		d, err := lim.Allow(ctx, requestsKey(pr), *pr.Requests, 1)
		if err != nil {
			return nil, p.storeError(ctx, "ratelimit", "allow", err)
		}
		if !d.Allowed {
			p.policy.Metrics.Refused(pr.Name, "requests")
			return nil, rateLimited(d, fmt.Sprintf("rate limit exceeded: this key may send %d requests per %v (burst %d)", pr.Requests.Rate, pr.Requests.Period, pr.Requests.Burst))
		}
	}
	if lim != nil && pr.Tokens != nil {
		need := int64(a.estimate.InputTokens + a.estimate.OutputTokens)
		d, err := lim.Allow(ctx, tokensKey(pr), *pr.Tokens, need)
		if err != nil {
			p.refundRequest(ctx, a)
			return nil, p.storeError(ctx, "ratelimit", "allow", err)
		}
		if !d.Allowed {
			p.refundRequest(ctx, a)
			if d.RetryAfter == ratelimit.Never {
				return nil, core.Errorf(core.KindInvalidRequest, "this request needs about %d tokens (input estimate plus max_tokens), more than this key's token limit admits at once (rate %d, burst %d per %v); lower max_tokens", need, pr.Tokens.Rate, pr.Tokens.Burst, pr.Tokens.Period)
			}
			p.policy.Metrics.Refused(pr.Name, "tokens")
			return nil, rateLimited(d, fmt.Sprintf("token rate limit exceeded: this key may use %d tokens per %v (burst %d)", pr.Tokens.Rate, pr.Tokens.Period, pr.Tokens.Burst))
		}
		a.tokens = need
	}
	if p.policy.Ledger != nil && pr.Budget != nil {
		amount := rt.Price.Of(a.estimate)
		res, err := p.policy.Ledger.Reserve(ctx, pr.Name, *pr.Budget, telemetry.NewRequestID(), amount)
		if err != nil {
			p.refundRequest(ctx, a)
			p.refundTokens(ctx, a)
			if errors.Is(err, budget.ErrExhausted) {
				return nil, core.Wrap(core.KindBudgetExceeded, err, fmt.Sprintf("this key's %s budget of %s is spent or reserved (this request could cost up to %s)", pr.Budget.Period, usd(pr.Budget.Limit), usd(amount)))
			}
			return nil, p.storeError(ctx, "budget", "reserve", err)
		}
		a.reservation = &res
	}
	return a, nil
}

func rateLimited(d ratelimit.Decision, msg string) error {
	return &core.Error{Kind: core.KindRateLimited, Message: msg, RetryAfter: d.RetryAfter}
}

// storeError turns a failure no fallback absorbed into a refusal. Failing
// closed is deliberate: the degrade policy already covers an unreachable
// store, so what reaches here is a bug or a misconfiguration.
func (p *Pipeline) storeError(ctx context.Context, component, op string, err error) error {
	if ctx.Err() != nil {
		return core.Wrap(core.KindTimeout, err, "request cancelled")
	}
	p.policy.Metrics.StoreError(component, op)
	return core.Wrap(core.KindInternal, err, component+" unavailable")
}

// refundCtx outlives the request: a refund that is skipped because the
// client went away would leave the charge standing for the rest of the
// window.
func (p *Pipeline) refundCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), p.settleTimeout())
}

func (p *Pipeline) refundRequest(ctx context.Context, a *admission) {
	if p.policy.Limiter == nil || a.pr.Requests == nil {
		return
	}
	ctx, cancel := p.refundCtx(ctx)
	defer cancel()
	if err := p.policy.Limiter.Adjust(ctx, requestsKey(a.pr), *a.pr.Requests, -1); err != nil {
		telemetry.Logger(ctx).Warn("rate limit refund failed", slog.String("error", err.Error()))
	}
}

func (p *Pipeline) refundTokens(ctx context.Context, a *admission) {
	if a.tokens == 0 {
		return
	}
	ctx, cancel := p.refundCtx(ctx)
	defer cancel()
	if err := p.policy.Limiter.Adjust(ctx, tokensKey(a.pr), *a.pr.Tokens, -a.tokens); err != nil {
		telemetry.Logger(ctx).Warn("token limit refund failed", slog.String("error", err.Error()))
	}
}

// settle corrects the admission to what the request really used and
// records it. A request that failed before any output was produced is
// charged nothing; one that streamed and then failed or was abandoned is
// charged for what it produced, estimated from its text when the provider
// never reported usage.
func (p *Pipeline) settle(ctx context.Context, a *admission, u core.Usage, outChars int, stream bool, outcome string) {
	if a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.settleTimeout())
	defer cancel()
	estimated := false
	if outcome == telemetry.OutcomeOK || outChars > 0 {
		// Part of the usage was never reported: a stream abandoned before
		// its stop event, or a server that does not send usage. Bill on
		// the estimate rather than for nothing.
		if u.PromptTokens() == 0 {
			u.InputTokens, estimated = a.estimate.InputTokens, true
		}
		if u.OutputTokens == 0 && outChars > 0 {
			u.OutputTokens, estimated = cost.EstimateOutputFromText(outChars), true
		}
	}
	key, model := a.pr.Name, a.route.Model
	if a.tokens != 0 {
		if delta := int64(u.PromptTokens()+u.OutputTokens) - a.tokens; delta != 0 {
			if err := p.policy.Limiter.Adjust(ctx, tokensKey(a.pr), *a.pr.Tokens, delta); err != nil {
				p.policy.Metrics.StoreError("ratelimit", "adjust")
				telemetry.Logger(ctx).Warn("token limit reconciliation failed", slog.String("error", err.Error()))
			}
		}
	}
	spent := a.route.Price.Of(u)
	if a.reservation != nil {
		if err := p.policy.Ledger.Settle(ctx, a.pr.Name, *a.pr.Budget, *a.reservation, spent); err != nil {
			p.policy.Metrics.StoreError("budget", "settle")
			telemetry.Logger(ctx).Warn("budget settlement failed", slog.String("error", err.Error()), slog.String("amount", spent.String()))
		}
	}
	m := p.policy.Metrics
	m.Call(key, model, outcome)
	m.Usage(key, model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, spent.USD(), a.route.Priced)
	m.Duration(model, stream, p.now().Sub(a.start))
	if estimated {
		telemetry.Logger(ctx).Info("usage estimated", slog.Int("input_tokens", u.InputTokens), slog.Int("output_tokens", u.OutputTokens))
	}
}

func (p *Pipeline) settleTimeout() time.Duration {
	if p.policy.SettleTimeout > 0 {
		return p.policy.SettleTimeout
	}
	return DefaultSettleTimeout
}

// usd formats an amount for an error message, to the cent when it is at
// least a cent.
func usd(n cost.Nanos) string {
	if n >= cost.Cent || n == 0 {
		return fmt.Sprintf("$%.2f", n.USD())
	}
	return fmt.Sprintf("$%.6f", n.USD())
}

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/hedge"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Policy is the shared-state part of the pipeline: limits, budgets, the
// response cache and metrics. A zero Policy enforces nothing, caches
// nothing and records nothing.
type Policy struct {
	// Limiter enforces the keys' request and token limits.
	Limiter ratelimit.Limiter
	// Ledger enforces the keys' budgets.
	Ledger  budget.Ledger
	Metrics *telemetry.Metrics
	// Cache is the response cache; nil disables it.
	Cache *cache.Cache
	// CacheMaxTemperature is the highest temperature cached without the
	// client's X-Switchboard-Cache header.
	CacheMaxTemperature float64
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
	// cache is the cache mode the request gets.
	cache cache.Decision
}

// charged reports whether the admission was charged on its output
// estimate, against a token limit or a budget.
func (a *admission) charged() bool {
	return (a.tokens != 0 || a.reservation != nil) && a.estimate.OutputTokens > 0
}

// MaxRequestTokens is the largest max_tokens the gateway accepts. It is far
// above any model's output limit and keeps the worst-case cost of a
// request well inside int64 nanodollars.
const MaxRequestTokens = 1 << 24

func requestsKey(pr *auth.Principal) string { return "req:" + pr.Name }
func tokensKey(pr *auth.Principal) string   { return "tok:" + pr.Name }

// admit runs the rate limits and the budget reservation, in that order,
// and undoes what it charged if a later check refuses. The checks are
// separate store calls, so a refusal by a later one leaves a short window
// in which an earlier charge counted against the key.
func (p *Pipeline) admit(ctx context.Context, pr *auth.Principal, rt Route, req *core.Request) (*admission, error) {
	if req.MaxTokens > MaxRequestTokens {
		return nil, core.Errorf(core.KindInvalidRequest, "max_tokens %d is above this gateway's limit of %d", req.MaxTokens, MaxRequestTokens)
	}
	a := &admission{pr: pr, route: rt, estimate: cost.Estimate(req, rt.OutputEstimate), start: p.now()}
	if lim := p.policy.Limiter; lim != nil && (pr.Requests != nil || pr.Tokens != nil) {
		sctx, span := telemetry.StartSpan(ctx, "ratelimit")
		err := p.rateLimit(sctx, a)
		telemetry.EndSpan(span, err)
		if err != nil {
			return nil, err
		}
	}
	if p.policy.Ledger != nil && pr.Budget != nil {
		sctx, span := telemetry.StartSpan(ctx, "budget.reserve")
		err := p.reserve(sctx, a)
		telemetry.EndSpan(span, err)
		if err != nil {
			return nil, err
		}
	}
	return a, nil
}

// rateLimit charges the request and token limits, refunding the request
// when the token limit refuses.
func (p *Pipeline) rateLimit(ctx context.Context, a *admission) error {
	pr, lim := a.pr, p.policy.Limiter
	if pr.Requests != nil {
		d, err := lim.Allow(ctx, requestsKey(pr), *pr.Requests, 1)
		if err != nil {
			return p.storeError(ctx, "ratelimit", "allow", err)
		}
		if !d.Allowed {
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("switchboard.ratelimit.refused", "requests"))
			p.policy.Metrics.Refused(pr.Name, "requests")
			return rateLimited(d, fmt.Sprintf("rate limit exceeded: this key may send %d requests per %v (burst %d)", pr.Requests.Rate, pr.Requests.Period, pr.Requests.Burst))
		}
	}
	if pr.Tokens != nil {
		need := int64(a.estimate.InputTokens + a.estimate.OutputTokens)
		d, err := lim.Allow(ctx, tokensKey(pr), *pr.Tokens, need)
		if err != nil {
			p.refundRequest(ctx, a)
			return p.storeError(ctx, "ratelimit", "allow", err)
		}
		if !d.Allowed {
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("switchboard.ratelimit.refused", "tokens"))
			p.refundRequest(ctx, a)
			if d.RetryAfter == ratelimit.Never {
				return core.Errorf(core.KindInvalidRequest, "this request needs about %d tokens (input estimate plus max_tokens), more than this key's token limit admits at once (rate %d, burst %d per %v); lower max_tokens", need, pr.Tokens.Rate, pr.Tokens.Burst, pr.Tokens.Period)
			}
			p.policy.Metrics.Refused(pr.Name, "tokens")
			return rateLimited(d, fmt.Sprintf("token rate limit exceeded: this key may use %d tokens per %v (burst %d)", pr.Tokens.Rate, pr.Tokens.Period, pr.Tokens.Burst))
		}
		a.tokens = need
	}
	return nil
}

// reserve holds the request's worst-case cost against the key's budget,
// refunding the rate limit charges when it cannot.
func (p *Pipeline) reserve(ctx context.Context, a *admission) error {
	pr := a.pr
	amount := a.route.Price.Of(a.estimate)
	trace.SpanFromContext(ctx).SetAttributes(attribute.Float64("switchboard.budget.reserve_usd", amount.USD()))
	if amount < 0 || amount >= budget.MaxExact {
		p.refundRequest(ctx, a)
		p.refundTokens(ctx, a)
		return core.Errorf(core.KindInvalidRequest, "this request could cost more than a budget can track (%s); send a smaller request", usd(amount))
	}
	res, err := p.policy.Ledger.Reserve(ctx, pr.Name, *pr.Budget, telemetry.NewRequestID(), amount)
	if err != nil {
		p.refundRequest(ctx, a)
		p.refundTokens(ctx, a)
		if errors.Is(err, budget.ErrExhausted) {
			return core.Wrap(core.KindBudgetExceeded, err, fmt.Sprintf("this key's %s budget of %s is spent or reserved (this request could cost up to %s)", pr.Budget.Period, usd(pr.Budget.Limit), usd(amount)))
		}
		return p.storeError(ctx, "budget", "reserve", err)
	}
	a.reservation = &res
	return nil
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
// records it. A request the upstream refused or failed before any output
// is charged nothing (one the client abandoned mid-call is charged its
// prompt estimate, see Complete); one that streamed and then failed or was abandoned is
// charged for what it produced, estimated from its text when the provider
// never reported usage.
func (p *Pipeline) settle(ctx context.Context, a *admission, u core.Usage, outChars int, stream bool, outcome string) {
	if a == nil {
		return
	}
	ctx, span := telemetry.StartSpan(context.WithoutCancel(ctx), "settle", attribute.String("switchboard.outcome", outcome))
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, p.settleTimeout())
	defer cancel()
	estimated := false
	if outcome == telemetry.OutcomeOK || outChars > 0 {
		u, estimated = a.fillUsage(u, outChars)
	}
	if n := hedge.ReportFrom(ctx).Snapshot().Cancelled; n > 0 {
		// Attempts cancelled because another answered first (a hedge and
		// the attempt it raced) may have had their prompts read and
		// billed upstream. Nothing reports what they cost, so each is
		// charged the prompt estimate: an over-count, never a cost the
		// ledger misses. They produced no output the client saw.
		u.InputTokens += n * a.estimate.InputTokens
	}
	key, model := a.pr.Name, a.route.Model
	if a.tokens != 0 {
		if delta := int64(u.PromptTokens()+u.OutputTokens) - a.tokens; delta != 0 {
			if err := p.policy.Limiter.Adjust(ctx, tokensKey(a.pr), *a.pr.Tokens, delta); err != nil {
				p.policy.Metrics.StoreError("ratelimit", "adjust")
				telemetry.Logger(ctx).Warn("token limit reconciliation failed", slog.String("error", err.Error()))
				telemetry.SpanError(span, err)
			}
		}
	}
	spent := a.route.Price.Of(u)
	if a.reservation != nil {
		if err := p.policy.Ledger.Settle(ctx, a.pr.Name, *a.pr.Budget, *a.reservation, spent); err != nil {
			p.policy.Metrics.StoreError("budget", "settle")
			telemetry.Logger(ctx).Warn("budget settlement failed", slog.String("error", err.Error()), slog.String("amount", spent.String()))
			telemetry.SpanError(span, err)
		}
	}
	m := p.policy.Metrics
	m.Call(key, model, outcome)
	m.Usage(key, model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, spent.USD(), a.route.Priced)
	m.Duration(model, stream, p.now().Sub(a.start))
	span.SetAttributes(attribute.Float64("switchboard.spent_usd", spent.USD()), attribute.Bool("switchboard.usage_estimated", estimated))
	if estimated {
		telemetry.Logger(ctx).Info("usage estimated", slog.Int("input_tokens", u.InputTokens), slog.Int("output_tokens", u.OutputTokens))
	}
}

// fillUsage completes usage the provider never reported (a stream
// abandoned before its stop event, or a server that does not send usage)
// from the admission's estimate and the length of the text, so it is
// billed, and cached, on the estimate rather than as nothing.
func (a *admission) fillUsage(u core.Usage, outChars int) (core.Usage, bool) {
	estimated := false
	if u.PromptTokens() == 0 {
		u.InputTokens, estimated = a.estimate.InputTokens, true
	}
	if u.OutputTokens == 0 && outChars > 0 {
		u.OutputTokens, estimated = cost.EstimateOutputFromText(outChars), true
	}
	return u, estimated
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

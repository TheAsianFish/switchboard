package pipeline

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// price is $3 in and $15 out per million tokens.
var price = cost.Price{Base: cost.Rates{Input: 3000, Output: 15000}}

// fleet is n pipelines, each with its own Redis client, sharing one
// miniredis and one fake provider: three replicas in one process.
type fleet struct {
	replicas []*Pipeline
	metrics  []*telemetry.Metrics
	fake     *fake.Provider
	redis    *miniredis.Miniredis
	ledger   *budget.Redis
}

func newFleet(t *testing.T, n int, key config.Key) *fleet {
	t.Helper()
	m := miniredis.RunT(t)
	m.SetTime(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	f := &fleet{fake: fake.New(fake.Config{Tokens: 30}), redis: m}
	for range n {
		rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		met := telemetry.NewMetrics(false)
		lim := ratelimit.NewDegrading(ratelimit.NewFixedWindow(rdb, "sb:"), ratelimit.NewMemoryFixedWindow(time.Now), n)
		lim.Timeout = time.Second // generous: -race slows miniredis
		lim.OnState = func(d bool) { met.Degraded("ratelimit", d) }
		led := budget.NewRedis(rdb, "sb:", time.Minute, time.Now)
		f.ledger = led
		p := New(auth.NewKeys([]config.Key{key}), []Route{
			{Model: "m", ProviderName: "fake", Upstream: "fake-m", Provider: f.fake, Price: price, Priced: true, OutputEstimate: 64},
		}, Policy{Limiter: lim, Ledger: led, Metrics: met})
		f.replicas = append(f.replicas, p)
		f.metrics = append(f.metrics, met)
	}
	return f
}

// fire sends total requests spread round-robin over the replicas, all at
// once, and returns the errors (nil for successes).
func (f *fleet) fire(t *testing.T, total int, stream bool) []error {
	t.Helper()
	errs := make([]error, total)
	var wg sync.WaitGroup
	for i := range total {
		wg.Go(func() {
			p := f.replicas[i%len(f.replicas)]
			ctx, err := p.Authenticate(context.Background(), "sk")
			if err != nil {
				errs[i] = err
				return
			}
			req := &core.Request{Model: "m", MaxTokens: 64, Messages: []core.Message{{Role: core.RoleUser, Text: fmt.Sprintf("request number %d please", i)}}}
			if !stream {
				_, errs[i] = p.Complete(ctx, req)
				return
			}
			s, err := p.Stream(ctx, req)
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = core.Collect(s)
		})
	}
	wg.Wait()
	return errs
}

func kinds(errs []error) map[core.ErrorKind]int {
	out := map[core.ErrorKind]int{}
	for _, err := range errs {
		if err == nil {
			out[-1]++
			continue
		}
		out[core.AsError(err).Kind]++
	}
	return out
}

// sum adds up a counter's series whose labels include want.
func sum(t *testing.T, ms []*telemetry.Metrics, name string, want map[string]string) float64 {
	t.Helper()
	total := 0.0
	for _, m := range ms {
		families, err := m.Registry().Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, fam := range families {
			if fam.GetName() != name {
				continue
			}
		series:
			for _, s := range fam.GetMetric() {
				labels := map[string]string{}
				for _, l := range s.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				for k, v := range want {
					if labels[k] != v {
						continue series
					}
				}
				if s.GetCounter() != nil {
					total += s.GetCounter().GetValue()
				} else {
					total += s.GetGauge().GetValue()
				}
			}
		}
	}
	return total
}

func TestFleetSharesOneRequestLimit(t *testing.T) {
	t.Parallel()
	// The clock is frozen, so the whole run is in one window: the fleet
	// admits exactly the limit, not three times it.
	f := newFleet(t, 3, config.Key{Name: "k", Key: "sk", Limits: config.Limits{Requests: &config.Rate{Rate: 25, Per: time.Minute, Burst: 25}}})
	errs := f.fire(t, 150, false)
	got := kinds(errs)
	if got[-1] != 25 || got[core.KindRateLimited] != 125 || f.fake.Calls() != 25 {
		t.Fatalf("outcomes %v, provider calls %d; want 25 admitted, 125 rate limited", got, f.fake.Calls())
	}
	for _, err := range errs {
		if e := core.AsError(err); err != nil && e.RetryAfter <= 0 {
			t.Fatalf("refusal without Retry-After: %v", err)
		}
	}
	if n := sum(t, f.metrics, "switchboard_requests_total", map[string]string{"key": "k", "outcome": "rate_limited"}); n != 125 {
		t.Errorf("rate_limited requests in metrics = %v, want 125", n)
	}
	if n := sum(t, f.metrics, "switchboard_ratelimit_refusals_total", map[string]string{"limit": "requests"}); n != 125 {
		t.Errorf("refusals in metrics = %v", n)
	}
}

func TestFleetSharesOneBudget(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			// Each request reserves about $0.001 (64 output tokens at worst)
			// and spends $0.000462 (4 input words, 30 output tokens).
			f := newFleet(t, 3, config.Key{Name: "k", Key: "sk", Budget: &config.KeyBudget{USD: "0.05", Period: "total"}})
			errs := f.fire(t, 300, stream)
			got := kinds(errs)
			if got[-1] == 0 || got[core.KindBudgetExceeded] == 0 || got[-1]+got[core.KindBudgetExceeded] != 300 {
				t.Fatalf("outcomes %v; want some admitted and the rest refused for budget", got)
			}
			var want cost.Nanos
			for _, e := range f.fake.Ledger() {
				want += cost.Nanos(e.InputTokens*3000 + e.OutputTokens*15000)
			}
			b := budget.Budget{Limit: 5 * cost.Cent, Period: budget.PeriodTotal}
			bal, err := f.ledger.Balance(context.Background(), "k", b)
			if err != nil {
				t.Fatal(err)
			}
			if bal.Spent != want || bal.Reserved != 0 {
				t.Errorf("gateway spent %v reserved %v; provider ledger prices to %v", bal.Spent, bal.Reserved, want)
			}
			if bal.Spent > b.Limit {
				t.Errorf("spent %v past the %v budget", bal.Spent, b.Limit)
			}
			if d := sum(t, f.metrics, "switchboard_cost_dollars_total", map[string]string{"key": "k"}); fmt.Sprintf("%.9f", d) != fmt.Sprintf("%.9f", want.USD()) {
				t.Errorf("metrics spend $%.9f, ledger $%.9f", d, want.USD())
			}
			if n := sum(t, f.metrics, "switchboard_requests_total", map[string]string{"outcome": "budget_exhausted"}); int(n) != got[core.KindBudgetExceeded] {
				t.Errorf("budget_exhausted in metrics = %v, want %d", n, got[core.KindBudgetExceeded])
			}
		})
	}
}

// single is one pipeline on in-memory state with a frozen clock.
func single(t *testing.T, key config.Key, prov core.Provider) (*Pipeline, ratelimit.Limiter, budget.Ledger, *telemetry.Metrics) {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) }
	lim := ratelimit.NewMemoryFixedWindow(now)
	led := budget.NewMemory(time.Minute, now)
	met := telemetry.NewMetrics(false)
	p := New(auth.NewKeys([]config.Key{key}), []Route{
		{Model: "m", ProviderName: "x", Upstream: "m", Provider: prov, Price: price, Priced: true, OutputEstimate: 64},
	}, Policy{Limiter: lim, Ledger: led, Metrics: met})
	return p, lim, led, met
}

func TestTokenLimitReconciles(t *testing.T) {
	t.Parallel()
	tokens := ratelimit.Limit{Rate: 1000, Period: time.Minute, Burst: 1000}
	p, lim, _, _ := single(t, config.Key{Name: "k", Key: "sk", Limits: config.Limits{Tokens: &config.Rate{Rate: 1000, Per: time.Minute, Burst: 1000}}}, fake.New(fake.Config{Tokens: 5}))
	ctx, _ := p.Authenticate(context.Background(), "sk")
	req := &core.Request{Model: "m", MaxTokens: 200, Messages: []core.Message{{Role: core.RoleUser, Text: "three little words"}}}
	if est := cost.Estimate(req, 64); est.InputTokens+est.OutputTokens <= 8 {
		t.Fatalf("estimate %+v should be above the real 8 tokens", est)
	}
	if _, err := p.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Admitted on the estimate (200+ tokens), reconciled to the real usage:
	// 3 input + 5 output.
	d, err := lim.Allow(context.Background(), "tok:k", tokens, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Remaining != 1000-8-1 {
		t.Errorf("remaining after reconciliation = %d, want %d", d.Remaining, 1000-8-1)
	}
	// A request that could never fit is a client error, not a wait.
	_, err = p.Complete(ctx, &core.Request{Model: "m", MaxTokens: 5000, Messages: req.Messages})
	if e := core.AsError(err); e.Kind != core.KindInvalidRequest {
		t.Errorf("oversized request: %v, want invalid request", err)
	}
}

func TestRefusalsRefundEarlierCharges(t *testing.T) {
	t.Parallel()
	reqs := ratelimit.Limit{Rate: 10, Period: time.Minute, Burst: 10}
	p, lim, _, _ := single(t, config.Key{
		Name: "k", Key: "sk",
		Limits: config.Limits{Requests: &config.Rate{Rate: 10, Per: time.Minute, Burst: 10}},
		Budget: &config.KeyBudget{USD: "0.000001", Period: "total"},
	}, fake.New(fake.Config{Tokens: 5}))
	ctx, _ := p.Authenticate(context.Background(), "sk")
	for range 3 {
		_, err := p.Complete(ctx, &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}})
		if core.AsError(err).Kind != core.KindBudgetExceeded {
			t.Fatalf("err = %v, want budget exceeded", err)
		}
	}
	// None of the refused requests used up the request limit.
	d, _ := lim.Allow(context.Background(), "req:k", reqs, 1)
	if d.Remaining != 9 {
		t.Errorf("request limit remaining = %d, want 9", d.Remaining)
	}
}

func TestFailuresAndAbandonedStreamsSettle(t *testing.T) {
	t.Parallel()
	b := budget.Budget{Limit: cost.Dollar, Period: budget.PeriodTotal}
	key := config.Key{Name: "k", Key: "sk", Budget: &config.KeyBudget{USD: "1", Period: "total"}, Limits: config.Limits{Tokens: &config.Rate{Rate: 1000, Per: time.Minute, Burst: 1000}}}
	tokens := ratelimit.Limit{Rate: 1000, Period: time.Minute, Burst: 1000}
	msgs := []core.Message{{Role: core.RoleUser, Text: "hello there"}}

	t.Run("upstream error costs nothing", func(t *testing.T) {
		t.Parallel()
		p, lim, led, met := single(t, key, scripted{err: core.Errorf(core.KindOverloaded, "busy")})
		ctx, _ := p.Authenticate(context.Background(), "sk")
		if _, err := p.Complete(ctx, &core.Request{Model: "m", Messages: msgs}); err == nil {
			t.Fatal("want an error")
		}
		bal, _ := led.Balance(context.Background(), "k", b)
		d, _ := lim.Allow(context.Background(), "tok:k", tokens, 1)
		if bal.Spent != 0 || bal.Reserved != 0 || d.Remaining != 999 {
			t.Errorf("after a failure: spent %v reserved %v, tokens remaining %d", bal.Spent, bal.Reserved, d.Remaining)
		}
		if n := sum(t, []*telemetry.Metrics{met}, "switchboard_requests_total", map[string]string{"outcome": "error"}); n != 1 {
			t.Errorf("error requests = %v", n)
		}
	})

	t.Run("abandoned stream is billed on an estimate", func(t *testing.T) {
		t.Parallel()
		p, _, led, met := single(t, key, scripted{stream: &endless{}})
		ctx, _ := p.Authenticate(context.Background(), "sk")
		s, err := p.Stream(ctx, &core.Request{Model: "m", Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		for range 31 { // start + 30 one-character deltas
			if _, err := s.Next(); err != nil {
				t.Fatal(err)
			}
		}
		_ = s.Close()
		bal, _ := led.Balance(context.Background(), "k", b)
		// The start event reported 1 input token; 30 characters of output
		// are estimated at 10 tokens.
		if want := cost.Nanos(1*3000 + 10*15000); bal.Spent != want || bal.Reserved != 0 {
			t.Errorf("spent %v reserved %v, want %v", bal.Spent, bal.Reserved, want)
		}
		if n := sum(t, []*telemetry.Metrics{met}, "switchboard_requests_total", map[string]string{"outcome": "abandoned"}); n != 1 {
			t.Errorf("abandoned requests = %v", n)
		}
	})
}

func TestUnpricedModelCountsTokensNotDollars(t *testing.T) {
	t.Parallel()
	met := telemetry.NewMetrics(false)
	p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk"}}), []Route{
		{Model: "m", ProviderName: "x", Upstream: "m", Provider: fake.New(fake.Config{Tokens: 5})},
	}, Policy{Metrics: met})
	ctx, _ := p.Authenticate(context.Background(), "sk")
	if _, err := p.Complete(ctx, &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "a b"}}}); err != nil {
		t.Fatal(err)
	}
	ms := []*telemetry.Metrics{met}
	if n := sum(t, ms, "switchboard_tokens_total", map[string]string{"type": "output"}); n != 5 {
		t.Errorf("output tokens = %v", n)
	}
	if n := sum(t, ms, "switchboard_unpriced_requests_total", nil); n != 1 {
		t.Errorf("unpriced requests = %v", n)
	}
	if n := sum(t, ms, "switchboard_cost_dollars_total", nil); n != 0 {
		t.Errorf("dollars = %v", n)
	}
}

func TestRedisDownDegradesToLocalLimits(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 3, config.Key{Name: "k", Key: "sk", Limits: config.Limits{Requests: &config.Rate{Rate: 30, Per: time.Hour, Burst: 30}}})
	f.redis.Close()
	// Each replica now admits a third of the limit on its own.
	got := kinds(f.fire(t, 90, false))
	if got[-1] != 30 || got[core.KindRateLimited] != 60 {
		t.Errorf("outcomes with Redis down %v, want 30 admitted (10 per replica)", got)
	}
	if n := sum(t, f.metrics, "switchboard_store_degraded", map[string]string{"component": "ratelimit"}); n != 3 {
		t.Errorf("degraded replicas = %v, want 3", n)
	}
}

// cancellingLedger is a client disconnecting while its reservation is
// in flight: Reserve cancels the request's context and fails with it.
type cancellingLedger struct {
	budget.Ledger
	cancel context.CancelFunc
}

func (c cancellingLedger) Reserve(ctx context.Context, _ string, _ budget.Budget, _ string, _ cost.Nanos) (budget.Reservation, error) {
	c.cancel()
	return budget.Reservation{}, ctx.Err()
}

// A client that disconnects during admission must not leave its rate
// limit charges behind, even though the refund runs after its context is
// gone.
func TestRefundsSurviveClientCancel(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	defer func() { _ = rdb.Close() }()
	lim := ratelimit.NewDegrading(ratelimit.NewFixedWindow(rdb, "sb:"), ratelimit.NewMemoryFixedWindow(time.Now), 1)
	lim.Timeout = time.Second
	reqs := ratelimit.Limit{Rate: 10, Period: time.Hour, Burst: 10}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := New(auth.NewKeys([]config.Key{{
		Name: "k", Key: "sk",
		Limits: config.Limits{Requests: &config.Rate{Rate: 10, Per: time.Hour, Burst: 10}},
		Budget: &config.KeyBudget{USD: "1", Period: "total"},
	}}), []Route{
		{Model: "m", ProviderName: "x", Upstream: "m", Provider: fake.New(fake.Config{Tokens: 5}), Price: price, Priced: true, OutputEstimate: 64},
	}, Policy{Limiter: lim, Ledger: cancellingLedger{cancel: cancel}})
	ctx, _ = p.Authenticate(ctx, "sk")
	if _, err := p.Complete(ctx, &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}); err == nil {
		t.Fatal("want an error")
	}
	d, err := lim.Allow(context.Background(), "req:k", reqs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Remaining != 9 {
		t.Errorf("request limit remaining = %d, want 9: the refund was lost", d.Remaining)
	}
}

// capturing records the request the provider was sent.
type capturing struct {
	core.Provider
	mu  sync.Mutex
	got []core.Request
}

func (c *capturing) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	c.mu.Lock()
	c.got = append(c.got, *req)
	c.mu.Unlock()
	return c.Provider.Complete(ctx, req)
}

// A request without max_tokens is reserved on the model's output estimate;
// that estimate is only a bound on the cost if the upstream is held to it.
func TestChargedRequestsAreCappedAtTheirEstimate(t *testing.T) {
	t.Parallel()
	msgs := []core.Message{{Role: core.RoleUser, Text: "hi"}}
	for _, tc := range []struct {
		name      string
		key       config.Key
		maxTokens int
		want      int
	}{
		{"budget, no max_tokens", config.Key{Name: "k", Key: "sk", Budget: &config.KeyBudget{USD: "1", Period: "total"}}, 0, 64},
		{"token limit, no max_tokens", config.Key{Name: "k", Key: "sk", Limits: config.Limits{Tokens: &config.Rate{Rate: 1000, Per: time.Minute, Burst: 1000}}}, 0, 64},
		{"budget, own max_tokens", config.Key{Name: "k", Key: "sk", Budget: &config.KeyBudget{USD: "1", Period: "total"}}, 10, 10},
		{"nothing charged", config.Key{Name: "k", Key: "sk"}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := &capturing{Provider: fake.New(fake.Config{Tokens: 5})}
			p, _, _, _ := single(t, tc.key, prov)
			ctx, _ := p.Authenticate(context.Background(), "sk")
			if _, err := p.Complete(ctx, &core.Request{Model: "m", MaxTokens: tc.maxTokens, Messages: msgs}); err != nil {
				t.Fatal(err)
			}
			if got := prov.got[0].MaxTokens; got != tc.want {
				t.Errorf("upstream max_tokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// A max_tokens large enough to overflow the cost arithmetic is the
// client's error, and charges nothing.
func TestHugeMaxTokensIsRefused(t *testing.T) {
	t.Parallel()
	reqs := ratelimit.Limit{Rate: 10, Period: time.Minute, Burst: 10}
	p, lim, led, met := single(t, config.Key{
		Name: "k", Key: "sk",
		Limits: config.Limits{Requests: &config.Rate{Rate: 10, Per: time.Minute, Burst: 10}},
		Budget: &config.KeyBudget{USD: "1", Period: "total"},
	}, fake.New(fake.Config{Tokens: 5}))
	ctx, _ := p.Authenticate(context.Background(), "sk")
	for _, n := range []int{MaxRequestTokens + 1, 922337203685478} {
		_, err := p.Complete(ctx, &core.Request{Model: "m", MaxTokens: n, Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}})
		if core.AsError(err).Kind != core.KindInvalidRequest {
			t.Errorf("max_tokens %d: %v, want invalid request", n, err)
		}
	}
	d, _ := lim.Allow(context.Background(), "req:k", reqs, 1)
	bal, _ := led.Balance(context.Background(), "k", budget.Budget{Limit: cost.Dollar, Period: budget.PeriodTotal})
	if d.Remaining != 9 || bal.Reserved != 0 {
		t.Errorf("request limit remaining %d, reserved %v: a refused request was charged", d.Remaining, bal.Reserved)
	}
	if n := sum(t, []*telemetry.Metrics{met}, "switchboard_store_errors_total", nil); n != 0 {
		t.Errorf("store errors = %v, want 0", n)
	}
}

// hangingUp is a client that disconnects while the upstream is working.
type hangingUp struct {
	core.Provider
	cancel context.CancelFunc
}

func (h hangingUp) Complete(ctx context.Context, _ *core.Request) (*core.Response, error) {
	h.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

// A non-streaming call the client abandons is charged its prompt
// estimate, and the charge lands even though the request's context is
// gone by the time it is settled.
func TestAbandonedCallIsChargedItsPrompt(t *testing.T) {
	t.Parallel()
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	defer func() { _ = rdb.Close() }()
	led := budget.NewRedis(rdb, "sb:", time.Minute, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk", Budget: &config.KeyBudget{USD: "1", Period: "total"}}}), []Route{
		{Model: "m", ProviderName: "x", Upstream: "m", Provider: hangingUp{cancel: cancel}, Price: price, Priced: true, OutputEstimate: 64},
	}, Policy{Ledger: led})
	ctx, _ = p.Authenticate(ctx, "sk")
	req := &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "a prompt the upstream has already read"}}}
	if _, err := p.Complete(ctx, req); err == nil {
		t.Fatal("want an error")
	}
	bal, err := led.Balance(context.Background(), "k", budget.Budget{Limit: cost.Dollar, Period: budget.PeriodTotal})
	if err != nil {
		t.Fatal(err)
	}
	want := price.Of(core.Usage{InputTokens: cost.EstimateInputTokens(req)})
	if bal.Spent != want || bal.Reserved != 0 {
		t.Errorf("spent %v reserved %v, want %v spent and nothing reserved", bal.Spent, bal.Reserved, want)
	}
}

// A budget refusal must undo the token-limit charge as well as the
// request-limit one.
func TestBudgetRefusalRefundsTokens(t *testing.T) {
	t.Parallel()
	tokens := ratelimit.Limit{Rate: 1000, Period: time.Minute, Burst: 1000}
	p, lim, _, _ := single(t, config.Key{
		Name: "k", Key: "sk",
		Limits: config.Limits{Tokens: &config.Rate{Rate: 1000, Per: time.Minute, Burst: 1000}},
		Budget: &config.KeyBudget{USD: "0.000001", Period: "total"},
	}, fake.New(fake.Config{Tokens: 5}))
	ctx, _ := p.Authenticate(context.Background(), "sk")
	_, err := p.Complete(ctx, &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}})
	if core.AsError(err).Kind != core.KindBudgetExceeded {
		t.Fatalf("err = %v, want budget exceeded", err)
	}
	d, _ := lim.Allow(context.Background(), "tok:k", tokens, 1)
	if d.Remaining != 999 {
		t.Errorf("token limit remaining = %d, want 999", d.Remaining)
	}
}

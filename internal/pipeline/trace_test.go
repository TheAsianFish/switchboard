package pipeline

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
	"github.com/TheAsianFish/switchboard/internal/telemetry/telemetrytest"
)

// spanSeen records the span a provider was called under, which is what a
// retry or hedge wrapper's attempt spans hang off.
type spanSeen struct {
	core.Provider
	sc chan trace.SpanContext
}

func (s spanSeen) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	s.sc <- trace.SpanContextFromContext(ctx)
	return s.Provider.Complete(ctx, req)
}

func (s spanSeen) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	s.sc <- trace.SpanContextFromContext(ctx)
	return s.Provider.Stream(ctx, req)
}

// tracedCall runs fn under a root span and returns the trace's spans.
func tracedCall(t *testing.T, exp *tracetest.InMemoryExporter, fn func(ctx context.Context)) tracetest.SpanStubs {
	t.Helper()
	ctx, root := telemetry.StartSpan(context.Background(), "root")
	fn(ctx)
	root.End()
	return telemetrytest.Spans(exp, root.SpanContext().TraceID().String())
}

func TestProviderGetsItsSpan(t *testing.T) {
	t.Parallel()
	exp := telemetrytest.InstallTracer()
	for _, stream := range []bool{false, true} {
		prov := spanSeen{Provider: fake.New(fake.Config{Tokens: 3}), sc: make(chan trace.SpanContext, 1)}
		p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk"}}), []Route{
			{Model: "m", ProviderName: "fake", Upstream: "up", Provider: prov},
		}, Policy{})
		spans := tracedCall(t, exp, func(ctx context.Context) {
			ctx, err := p.Authenticate(ctx, "sk")
			if err != nil {
				t.Fatal(err)
			}
			if !stream {
				if _, err := p.Complete(ctx, &core.Request{Model: "m"}); err != nil {
					t.Fatal(err)
				}
				return
			}
			s, err := p.Stream(ctx, &core.Request{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			for {
				if _, err := s.Next(); err != nil {
					break
				}
			}
			_ = s.Close()
		})
		ps, ok := telemetrytest.Find(spans, "provider")
		if !ok {
			t.Fatalf("stream=%v: no provider span in\n%s", stream, telemetrytest.Tree(spans))
		}
		if got := <-prov.sc; got.SpanID() != ps.SpanContext.SpanID() {
			t.Errorf("stream=%v: provider called under span %s, want the provider span %s", stream, got.SpanID(), ps.SpanContext.SpanID())
		}
		want := "root\n  auth\n  route\n  cache.lookup\n  provider\n  settle\n"
		if got := telemetrytest.Tree(spans); got != want {
			t.Errorf("stream=%v: tree\n%s\nwant\n%s", stream, got, want)
		}
	}
}

func TestProviderSpanOutcome(t *testing.T) {
	t.Parallel()
	exp := telemetrytest.InstallTracer()
	tests := []struct {
		name        string
		prov        core.Provider
		run         func(ctx context.Context, p *Pipeline)
		wantOutcome string
		wantError   bool
	}{
		{"upstream error", scripted{err: core.Errorf(core.KindUpstream, "upstream: overloaded")},
			func(ctx context.Context, p *Pipeline) { _, _ = p.Complete(ctx, &core.Request{Model: "m"}) }, "error", true},
		{"stream abandoned after one event", fake.New(fake.Config{Tokens: 5}),
			func(ctx context.Context, p *Pipeline) {
				s, err := p.Stream(ctx, &core.Request{Model: "m"})
				if err != nil {
					return
				}
				_, _ = s.Next()
				_ = s.Close()
			}, "abandoned", false},
		{"stream failing mid-way", cutShort{err: errors.New("connection reset")},
			func(ctx context.Context, p *Pipeline) {
				s, err := p.Stream(ctx, &core.Request{Model: "m"})
				if err != nil {
					return
				}
				for {
					if _, err := s.Next(); err != nil {
						break
					}
				}
				_ = s.Close()
			}, "error", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk"}}), []Route{
				{Model: "m", ProviderName: "x", Upstream: "m", Provider: tt.prov},
			}, Policy{})
			spans := tracedCall(t, exp, func(ctx context.Context) {
				ctx, _ = p.Authenticate(ctx, "sk")
				tt.run(ctx, p)
			})
			ps, ok := telemetrytest.Find(spans, "provider")
			if !ok {
				t.Fatalf("no provider span in\n%s", telemetrytest.Tree(spans))
			}
			outcome := ""
			for _, kv := range ps.Attributes {
				if kv.Key == "switchboard.outcome" {
					outcome = kv.Value.AsString()
				}
			}
			if outcome != tt.wantOutcome || (ps.Status.Code == codes.Error) != tt.wantError {
				t.Errorf("outcome %q status %v; want %q, error %v", outcome, ps.Status, tt.wantOutcome, tt.wantError)
			}
		})
	}
}

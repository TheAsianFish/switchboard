package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestOTLPTracesURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want, err string
	}{
		{in: "http://jaeger:4318", want: "http://jaeger:4318/v1/traces"},
		{in: "http://jaeger:4318/", want: "http://jaeger:4318/v1/traces"},
		{in: "https://collector.example/otlp/v1/traces", want: "https://collector.example/otlp/v1/traces"},
		{in: "jaeger:4318", err: "want an http(s) URL"},
		{in: "http://", err: "want an http(s) URL"},
	}
	for _, tt := range tests {
		got, err := otlpTracesURL(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("otlpTracesURL(%q) err = %v, want %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("otlpTracesURL(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestSetupTracingOff(t *testing.T) {
	t.Parallel()
	shutdown, err := SetupTracing(context.Background(), TraceConfig{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// With nothing installed, spans are no-ops and carry no trace id.
	ctx, span := StartSpan(context.Background(), "x")
	EndSpan(span, errors.New("boom"))
	if span.IsRecording() || TraceID(ctx) != "" {
		t.Errorf("tracing off: recording %v, trace id %q", span.IsRecording(), TraceID(ctx))
	}
}

func TestSetupTracingBadEndpoint(t *testing.T) {
	t.Parallel()
	if _, err := SetupTracing(context.Background(), TraceConfig{Endpoint: "jaeger:4318"}, slog.Default()); err == nil {
		t.Fatal("want an error for an endpoint without a scheme")
	}
}

func TestEndSpanRecordsError(t *testing.T) {
	t.Parallel()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, ok := tp.Tracer("t").Start(context.Background(), "ok")
	EndSpan(ok, nil)
	ctx, failed := tp.Tracer("t").Start(context.Background(), "failed")
	EndSpan(failed, errors.New("upstream refused"))
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans", len(spans))
	}
	if spans[0].Status.Code != codes.Unset {
		t.Errorf("ok span status = %v", spans[0].Status)
	}
	if spans[1].Status.Code != codes.Error || spans[1].Status.Description != "upstream refused" || len(spans[1].Events) != 1 {
		t.Errorf("failed span status = %v, events %d", spans[1].Status, len(spans[1].Events))
	}
	if got, want := TraceID(ctx), trace.SpanContextFromContext(ctx).TraceID().String(); got != want || len(got) != 32 {
		t.Errorf("TraceID = %q, want %q", got, want)
	}
}

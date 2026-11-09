package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// TracerName is the instrumentation scope of the gateway's own spans.
const TracerName = "github.com/TheAsianFish/switchboard"

// TraceConfig configures span export.
type TraceConfig struct {
	// Endpoint is the OTLP/HTTP collector's base URL, for example
	// http://jaeger:4318; /v1/traces is appended when it has no path.
	// Empty turns tracing off.
	Endpoint string
	// SampleRatio is the share of new traces kept, 0 to 1. A request that
	// arrives with a traceparent follows its caller's sampling decision.
	SampleRatio float64
	// ServiceName is the service.name resource attribute.
	ServiceName string
	// Version is the service.version resource attribute.
	Version string
}

// SetupTracing installs the global tracer provider and the W3C trace
// context propagator. With no endpoint it installs nothing: the global
// provider stays the no-op one, spans cost a context lookup, and incoming
// traceparent headers are ignored. The returned function flushes the spans
// still queued and stops the exporter; it is safe to call when tracing is
// off.
func SetupTracing(ctx context.Context, cfg TraceConfig, log *slog.Logger) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if cfg.Endpoint == "" {
		return noop, nil
	}
	endpoint, err := otlpTracesURL(cfg.Endpoint)
	if err != nil {
		return noop, err
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithTimeout(5*time.Second))
	if err != nil {
		return noop, fmt.Errorf("otlp exporter: %w", err)
	}
	// host.name tells the replicas apart; in compose it is the container's
	// hostname.
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(
			attribute.String("service.name", cfg.ServiceName),
			attribute.String("service.version", cfg.Version),
		),
	)
	if err != nil {
		return noop, fmt.Errorf("trace resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		// Batched: exporting never sits on a request's path. A span
		// dropped because the queue is full is lost, not waited for.
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(Sampler(cfg.SampleRatio)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	// Export failures (a collector that is down) go to the process log
	// instead of the OTel default of the standard library logger.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("tracing", slog.String("error", err.Error()))
	}))
	return tp.Shutdown, nil
}

// Sampler keeps ratio of new traces and follows the gateway's own
// decisions inside a trace. A caller's traceparent is continued (its trace
// id joins the caller's trace) but its sampled flag is not trusted: it
// arrives before authentication, and a flag forcing every request to be
// exported would let any caller flood the collector and push other traces
// out of the batch queue. Remote parents are sampled at random at ratio,
// not by trace id, because the caller chooses the trace id.
func Sampler(ratio float64) sdktrace.Sampler {
	random := randomSampler{ratio: ratio}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio),
		sdktrace.WithRemoteParentSampled(random),
		sdktrace.WithRemoteParentNotSampled(random))
}

type randomSampler struct{ ratio float64 }

func (s randomSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	d := sdktrace.Drop
	if s.ratio >= 1 || (s.ratio > 0 && rand.Float64() < s.ratio) {
		d = sdktrace.RecordAndSample
	}
	return sdktrace.SamplingResult{Decision: d, Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState()}
}

func (s randomSampler) Description() string {
	return fmt.Sprintf("RandomRemoteParent{%g}", s.ratio)
}

// otlpTracesURL turns a collector base URL into the traces endpoint, the
// way OTEL_EXPORTER_OTLP_ENDPOINT is read.
func otlpTracesURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("tracing endpoint %q: want an http(s) URL such as http://localhost:4318", endpoint)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	return u.String(), nil
}

// Tracer returns the gateway's tracer from the global provider, looked up
// on each call so that SetupTracing (or a test) may install the provider
// after a package's variables are initialised.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// StartSpan starts an internal span named name as a child of ctx's span.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// EndSpan ends span, recording err, when there is one, as the span's
// error status.
func EndSpan(span trace.Span, err error) {
	SpanError(span, err)
	span.End()
}

// SpanError marks span as failed with err. A classified error is recorded
// by its kind and client-safe message only: its cause can be an upstream's
// own error text, which may quote part of the gateway's credentials, and
// spans leave the process. Other errors (store failures) are recorded as
// they are.
func SpanError(span trace.Span, err error) {
	if err == nil || !span.IsRecording() {
		return
	}
	kind, msg := "", err.Error()
	var e *core.Error
	if errors.As(err, &e) {
		kind = e.Kind.String()
		msg = kind + ": " + e.Message
		span.SetAttributes(attribute.String("error.type", kind))
	}
	span.AddEvent("exception", trace.WithAttributes(
		attribute.String("exception.type", kind),
		attribute.String("exception.message", msg),
	))
	span.SetStatus(codes.Error, msg)
}

// TraceID returns the trace id of ctx's span, or "" when there is none:
// always so with tracing off. A trace that sampling dropped still has an
// id, so a client can quote it even when no backend kept the spans.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

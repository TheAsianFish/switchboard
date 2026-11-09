// Package telemetrytest records the gateway's spans in memory for tests.
package telemetrytest

import (
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	once     sync.Once
	exporter *tracetest.InMemoryExporter
)

// InstallTracer installs, once per test binary, a global tracer provider
// that samples everything and records spans synchronously in memory, and
// the W3C propagator. Tests running in parallel share it and tell their
// spans apart by trace id (Spans).
func InstallTracer() *tracetest.InMemoryExporter {
	once.Do(func() {
		exporter = tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
	return exporter
}

// Spans returns the ended spans of one trace, by its hex id.
func Spans(exp *tracetest.InMemoryExporter, traceID string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range exp.GetSpans() {
		if s.SpanContext.TraceID().String() == traceID {
			out = append(out, s)
		}
	}
	return out
}

// Tree renders spans as an indented tree of names, children in the order
// they started, for example
//
//	POST /v1/chat/completions
//	  auth
//	  route
func Tree(spans tracetest.SpanStubs) string {
	ids := map[string]bool{}
	children := map[string][]tracetest.SpanStub{}
	for _, s := range spans {
		ids[s.SpanContext.SpanID().String()] = true
	}
	for _, s := range spans {
		parent := s.Parent.SpanID().String()
		if !s.Parent.IsValid() || !ids[parent] {
			parent = ""
		}
		children[parent] = append(children[parent], s)
	}
	for _, c := range children {
		slices.SortStableFunc(c, func(a, b tracetest.SpanStub) int { return a.StartTime.Compare(b.StartTime) })
	}
	var b strings.Builder
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, s := range children[parent] {
			b.WriteString(strings.Repeat("  ", depth))
			b.WriteString(s.Name)
			b.WriteByte('\n')
			walk(s.SpanContext.SpanID().String(), depth+1)
		}
	}
	walk("", 0)
	return b.String()
}

// Find returns the first span named name, and whether there was one.
func Find(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

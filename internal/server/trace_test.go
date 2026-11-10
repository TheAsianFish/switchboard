package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/budget"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/ratelimit"
	"github.com/TheAsianFish/switchboard/internal/server"
	"github.com/TheAsianFish/switchboard/internal/telemetry/telemetrytest"
)

// traceRig is the gateway with every stage switched on: a request limit,
// a budget and the exact cache, over the in-process fake provider.
type traceRig struct {
	url   string
	cache *cache.Cache
	exp   *tracetest.InMemoryExporter
}

func newTraceRig(t *testing.T, requests *config.Rate) traceRig {
	t.Helper()
	exp := telemetrytest.InstallTracer()
	c := cache.New(cache.Options{
		KV:        cache.NewMemoryKV(100, time.Now),
		NewIndex:  func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 100, time.Now), nil },
		Embedder:  &embed.Fake{Dims: 64},
		Threshold: 0.9,
		TTL:       time.Hour,
	})
	key := config.Key{Name: "traced", Key: gatewayKey, Cache: "exact", Budget: &config.KeyBudget{USD: "1", Period: "total"},
		Limits: config.Limits{Requests: requests}}
	price := cost.Price{Base: cost.Rates{Input: 3000, Output: 15000}}
	gw := pipeline.New(auth.NewKeys([]config.Key{key}),
		[]pipeline.Route{{Model: "fake", ProviderName: "fake", Upstream: "fake-up", Provider: fake.New(fakeShape), Price: price, Priced: true, OutputEstimate: 64}},
		pipeline.Policy{Limiter: ratelimit.NewMemoryFixedWindow(time.Now), Ledger: budget.NewMemory(time.Minute, time.Now), Cache: c})
	front := httptest.NewServer(server.Handler(gw, slog.New(slog.DiscardHandler)))
	t.Cleanup(front.Close)
	return traceRig{url: front.URL, cache: c, exp: exp}
}

func (r traceRig) post(t *testing.T, body string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

// spans waits for the trace's root span, which ends after the client has
// read the last byte, and for want spans in all.
func (r traceRig) spans(t *testing.T, traceID string, want int) tracetest.SpanStubs {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := telemetrytest.Spans(r.exp, traceID)
		_, root := telemetrytest.Find(got, "POST /v1/chat/completions")
		if (root && len(got) >= want) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// body is a deterministic, so cacheable, chat completion request.
func body(stream bool) string {
	return fmt.Sprintf(`{"model":"fake","temperature":0,"max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"Name three primary colours."}]}`, stream)
}

func TestTraceContinuesTheCallersTrace(t *testing.T) {
	t.Parallel()
	r := newTraceRig(t, nil)
	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		parent  = "00f067aa0ba902b7"
	)
	resp, _ := r.post(t, body(false), http.Header{"Traceparent": {"00-" + traceID + "-" + parent + "-01"}})
	if got := resp.Header.Get(server.TraceIDHeader); got != traceID {
		t.Fatalf("%s = %q, want the caller's %q", server.TraceIDHeader, got, traceID)
	}
	spans := r.spans(t, traceID, 1)
	root, ok := telemetrytest.Find(spans, "POST /v1/chat/completions")
	if !ok || root.Parent.SpanID().String() != parent || !root.Parent.IsRemote() {
		t.Errorf("root span parent = %v, want the caller's span %s", root.Parent.SpanID(), parent)
	}
}

func TestHealthzIsNotTraced(t *testing.T) {
	t.Parallel()
	r := newTraceRig(t, nil)
	resp, err := http.Get(r.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get(server.TraceIDHeader); got != "" {
		t.Errorf("/healthz %s = %q, want none", server.TraceIDHeader, got)
	}
}

// Every log line of a traced request carries its trace id, so a log line
// leads to the trace and back.
func TestTraceIDOnEveryLogLine(t *testing.T) {
	t.Parallel()
	telemetrytest.InstallTracer()
	var logs bytes.Buffer
	gw := pipeline.New(auth.NewKeys([]config.Key{{Name: "k", Key: gatewayKey}}),
		[]pipeline.Route{{Model: "fake", ProviderName: "fake", Upstream: "fake", Provider: fake.New(fakeShape)}}, pipeline.Policy{})
	front := httptest.NewServer(server.Handler(gw, slog.New(slog.NewJSONHandler(&logs, nil))))
	r := traceRig{url: front.URL}
	resp, _ := r.post(t, body(true), nil)
	tid := resp.Header.Get(server.TraceIDHeader)
	front.Close() // waits for the handler, and so for its access log line
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if tid == "" || len(lines) < 2 {
		t.Fatalf("trace id %q, %d log lines", tid, len(lines))
	}
	for _, l := range lines {
		var line map[string]any
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatal(err)
		}
		if line["trace_id"] != tid {
			t.Errorf("log line %q has trace_id %v, want %s", line["msg"], line["trace_id"], tid)
		}
	}
}

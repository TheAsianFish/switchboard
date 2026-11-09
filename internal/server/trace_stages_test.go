package server_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/server"
	"github.com/TheAsianFish/switchboard/internal/telemetry/telemetrytest"
)

// The span tree of a request: the root span from the server, one child per
// pipeline stage that ran.
func TestTraceOfAStreamedCacheMissThenHit(t *testing.T) {
	t.Parallel()
	r := newTraceRig(t, &config.Rate{Rate: 100, Per: time.Minute, Burst: 100})

	resp, out := r.post(t, body(true), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "[DONE]") {
		t.Fatalf("status %d body %q", resp.StatusCode, out)
	}
	tid := resp.Header.Get(server.TraceIDHeader)
	if len(tid) != 32 {
		t.Fatalf("%s = %q, want a 32-hex trace id", server.TraceIDHeader, tid)
	}
	r.cache.Wait()
	spans := r.spans(t, tid, 8)
	want := `POST /v1/chat/completions
  auth
  route
  ratelimit
  budget.reserve
  cache.lookup
  provider
  settle
  cache.fill
`
	if got := telemetrytest.Tree(spans); got != want {
		t.Errorf("span tree:\n%s\nwant:\n%s", got, want)
	}
	lk, _ := telemetrytest.Find(spans, "cache.lookup")
	if got := attr(lk, "switchboard.cache.status"); got != "miss" {
		t.Errorf("cache.lookup status = %q, want miss", got)
	}
	prov, _ := telemetrytest.Find(spans, "provider")
	for k, v := range map[string]string{
		"switchboard.provider": "fake", "switchboard.upstream_model": "fake-up", "switchboard.stream": "true",
		"switchboard.outcome": "ok", "switchboard.output_tokens": "9",
	} {
		if got := attr(prov, k); got != v {
			t.Errorf("provider %s = %q, want %q", k, got, v)
		}
	}
	if attr(prov, "switchboard.ttft_ms") == "" {
		t.Error("provider span has no ttft_ms")
	}
	// The provider span lasts until the stream's stop event, past the
	// first token.
	if prov.EndTime.Sub(prov.StartTime) < 5*time.Millisecond {
		t.Errorf("provider span lasted %v, want the whole stream", prov.EndTime.Sub(prov.StartTime))
	}
	root, _ := telemetrytest.Find(spans, "POST /v1/chat/completions")
	if attr(root, "http.response.status_code") != "200" || attr(root, "http.route") != "/v1/chat/completions" {
		t.Errorf("root attributes = %v", root.Attributes)
	}

	// The same request again is answered from the cache: no provider span.
	resp, _ = r.post(t, body(false), nil)
	hitID := resp.Header.Get(server.TraceIDHeader)
	if resp.Header.Get(cache.StatusHeader) != "hit-exact" || hitID == "" || hitID == tid {
		t.Fatalf("second call: cache %q, trace id %q (first %q)", resp.Header.Get(cache.StatusHeader), hitID, tid)
	}
	spans = r.spans(t, hitID, 7)
	want = `POST /v1/chat/completions
  auth
  route
  ratelimit
  budget.reserve
  cache.lookup
  settle
`
	if got := telemetrytest.Tree(spans); got != want {
		t.Errorf("hit span tree:\n%s\nwant:\n%s", got, want)
	}
	lk, _ = telemetrytest.Find(spans, "cache.lookup")
	if got := attr(lk, "switchboard.cache.status"); got != "hit-exact" {
		t.Errorf("cache.lookup status = %q, want hit-exact", got)
	}
}

func TestTraceOfARateLimitedRequest(t *testing.T) {
	t.Parallel()
	r := newTraceRig(t, &config.Rate{Rate: 1, Per: time.Hour, Burst: 1})
	if resp, _ := r.post(t, body(false), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first call status %d", resp.StatusCode)
	}
	resp, _ := r.post(t, body(false), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call status %d, want 429", resp.StatusCode)
	}
	tid := resp.Header.Get(server.TraceIDHeader)
	spans := r.spans(t, tid, 4)
	want := `POST /v1/chat/completions
  auth
  route
  ratelimit
`
	if got := telemetrytest.Tree(spans); got != want {
		t.Errorf("span tree:\n%s\nwant:\n%s", got, want)
	}
	rl, _ := telemetrytest.Find(spans, "ratelimit")
	if rl.Status.Code != codes.Error || attr(rl, "switchboard.ratelimit.refused") != "requests" {
		t.Errorf("ratelimit span status %v, attributes %v", rl.Status, rl.Attributes)
	}
	// A refusal is the client's error: the root span records the status
	// and is not marked failed.
	root, _ := telemetrytest.Find(spans, "POST /v1/chat/completions")
	if root.Status.Code == codes.Error || attr(root, "http.response.status_code") != "429" {
		t.Errorf("root status %v, attributes %v", root.Status, root.Attributes)
	}
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func durations(ms ...int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, m := range ms {
		out[i] = time.Duration(m) * time.Millisecond
	}
	return out
}

func seq(n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = time.Duration(i+1) * time.Millisecond
	}
	return out
}

func TestPercentile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		sorted []time.Duration
		p      float64
		want   time.Duration
	}{
		{"empty", nil, 50, 0},
		{"single p50", durations(7), 50, 7 * time.Millisecond},
		{"single p99.9", durations(7), 99.9, 7 * time.Millisecond},
		{"ten p50", seq(10), 50, 5 * time.Millisecond},
		{"ten p90", seq(10), 90, 9 * time.Millisecond},
		{"ten p99", seq(10), 99, 10 * time.Millisecond},
		{"ten p100", seq(10), 100, 10 * time.Millisecond},
		{"ten p1", seq(10), 1, 1 * time.Millisecond},
		{"hundred p99", seq(100), 99, 99 * time.Millisecond},
		{"thousand p99.9", seq(1000), 99.9, 999 * time.Millisecond},
		{"thousand p99", seq(1000), 99, 990 * time.Millisecond},
		{"four p50", seq(4), 50, 2 * time.Millisecond},
		{"three p50", seq(3), 50, 2 * time.Millisecond},
		{"ten p94", seq(10), 94, 10 * time.Millisecond}, // rank 9.4 rounds up, not to nearest
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Percentile(tt.sorted, tt.p); got != tt.want {
				t.Errorf("Percentile(%d samples, %v) = %v, want %v", len(tt.sorted), tt.p, got, tt.want)
			}
		})
	}
}

func TestSummarise(t *testing.T) {
	t.Parallel()
	in := []time.Duration{3 * time.Millisecond, 1 * time.Millisecond, 2*time.Millisecond + 1234*time.Nanosecond, 10 * time.Millisecond}
	got := Summarise(in)
	want := Stats{Count: 4, Mean: 4.0, P50: 2.001, P90: 10, P95: 10, P99: 10, P999: 10, Max: 10}
	if got != want {
		t.Errorf("Summarise = %+v, want %+v", got, want)
	}
	if in[0] != 3*time.Millisecond {
		t.Error("Summarise reordered its input")
	}
	if (Summarise(nil) != Stats{}) {
		t.Error("Summarise(nil) is not zero")
	}
}

// sleepCtx waits for d or until the request is abandoned.
func sleepCtx(r *http.Request, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}

func okJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
}

// TestSummariseCountsSendDelay pins the coordinated-omission rule: a
// request the generator sent late is charged from its scheduled time, so a
// generator that falls behind cannot hide the queueing it caused.
func TestSummariseCountsSendDelay(t *testing.T) {
	t.Parallel()
	at := time.Unix(1000, 0)
	sent := at.Add(50 * time.Millisecond)
	rs := []scheduled{{at: at, sample: sample{
		outcome:    OutcomeOK,
		sent:       sent,
		firstToken: sent.Add(5 * time.Millisecond),
		done:       sent.Add(10 * time.Millisecond),
	}}}
	res := summarise(Config{Duration: time.Second}, rs, at, at.Add(time.Second))
	if res.Latency.P50 != 60 || res.TTFT.P50 != 55 || res.SendDelay.P50 != 50 {
		t.Errorf("latency %v, ttft %v, send delay %v ms; want 60, 55, 50", res.Latency.P50, res.TTFT.P50, res.SendDelay.P50)
	}
}

// TestSummariseKeepsSamples checks the per-request values are those of the
// measured successful requests only, in schedule order.
func TestSummariseKeepsSamples(t *testing.T) {
	t.Parallel()
	at := time.Unix(1000, 0)
	ok := func(start time.Time, ttft, total time.Duration) sample {
		return sample{outcome: OutcomeOK, sent: start, firstToken: start.Add(ttft), done: start.Add(total)}
	}
	rs := []scheduled{
		{at: at, warmup: true, sample: ok(at, time.Millisecond, 2*time.Millisecond)},
		{at: at, sample: ok(at, 3*time.Millisecond, 4*time.Millisecond)},
		{at: at, sample: sample{outcome: "http_500", sent: at, done: at.Add(time.Millisecond)}},
		{at: at, sample: ok(at, 1500*time.Microsecond, 7*time.Millisecond)},
	}
	for _, keep := range []bool{false, true} {
		res := summarise(Config{Duration: time.Second, KeepSamples: keep}, rs, at, at.Add(time.Second))
		if !keep {
			if res.Samples != nil {
				t.Errorf("samples recorded without KeepSamples: %+v", res.Samples)
			}
			continue
		}
		want := &Samples{LatencyMS: []float64{4, 7}, TTFTMS: []float64{3, 1.5}}
		if !reflect.DeepEqual(res.Samples, want) {
			t.Errorf("samples %+v, want %+v", res.Samples, want)
		}
	}
}

func TestRunOffersRateAgainstSlowServer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sleepCtx(r, 200*time.Millisecond) {
			okJSON(w)
		}
	}))
	defer srv.Close()

	res, err := Run(context.Background(), Config{URL: srv.URL, Model: "m", Rate: 50, Duration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 50 || res.Outcomes[OutcomeOK] != 50 {
		t.Fatalf("requests %d, outcomes %v, want 50 ok", res.Requests, res.Outcomes)
	}
	if res.OfferedRate != 50 {
		t.Errorf("offered %v/s, want 50", res.OfferedRate)
	}
	// A closed loop would keep one request in flight; open loop keeps
	// rate x service time = 10.
	if res.PeakInFlight < 9 {
		t.Errorf("peak in flight %d, want about 10", res.PeakInFlight)
	}
	if res.Latency.P50 < 200 || res.Latency.P50 > 300 {
		t.Errorf("latency p50 %v ms, want about 200", res.Latency.P50)
	}
	if res.TTFT != res.Latency {
		t.Errorf("non-stream TTFT %+v differs from latency %+v", res.TTFT, res.Latency)
	}
	if res.SendDelay.Count != 50 {
		t.Errorf("send delay count %d, want 50", res.SendDelay.Count)
	}
	// The whole run fits in duration + service time, not 50 x 200 ms.
	if res.WallSeconds > 2 {
		t.Errorf("wall %v s: the generator waited for replies", res.WallSeconds)
	}
}

// sseServer streams events with a delay before the first one carrying text
// and between later ones, and checks the request it got.
func sseServer(t *testing.T, format string, first, between time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("request body: %v", err)
		}
		if req["stream"] != true {
			t.Errorf("stream not set: %s", body)
		}
		if format == FormatAnthropic {
			if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") != "2023-06-01" {
				t.Errorf("anthropic request: %s %v", r.URL.Path, r.Header)
			}
			if req["max_tokens"] != float64(DefaultAnthropicMaxTokens) {
				t.Errorf("max_tokens = %v", req["max_tokens"])
			}
		} else {
			if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
				t.Errorf("openai request: %s %v", r.URL.Path, r.Header)
			}
			if so, _ := req["stream_options"].(map[string]any); so["include_usage"] != true {
				t.Errorf("stream_options missing: %s", body)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		emit := func(name, data string) {
			if name != "" {
				fmt.Fprintf(w, "event: %s\n", name)
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}
		if format == FormatAnthropic {
			emit("message_start", `{"type":"message_start","message":{"id":"m"}}`)
			emit("content_block_start", `{"type":"content_block_start","index":0}`)
			if !sleepCtx(r, first) {
				return
			}
			emit("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"a"}}`)
			if !sleepCtx(r, between) {
				return
			}
			emit("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":" b"}}`)
			emit("content_block_stop", `{"type":"content_block_stop","index":0}`)
			emit("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
			emit("message_stop", `{"type":"message_stop"}`)
			return
		}
		emit("", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`)
		if !sleepCtx(r, first) {
			return
		}
		emit("", `{"choices":[{"delta":{"content":"a"}}]}`)
		if !sleepCtx(r, between) {
			return
		}
		emit("", `{"choices":[{"delta":{"content":" b"}}]}`)
		emit("", `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
		emit("", `[DONE]`)
	}))
}

func TestRunStreamTTFT(t *testing.T) {
	t.Parallel()
	for _, format := range []string{FormatOpenAI, FormatAnthropic} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			srv := sseServer(t, format, 40*time.Millisecond, 120*time.Millisecond)
			defer srv.Close()
			res, err := Run(context.Background(), Config{URL: srv.URL, Format: format, APIKey: "k", Model: "m", Stream: true, Rate: 20, Duration: 250 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcomes[OutcomeOK] != 5 || res.TTFT.Count != 5 {
				t.Fatalf("outcomes %v, ttft count %d, want 5 ok", res.Outcomes, res.TTFT.Count)
			}
			if res.TTFT.P50 < 40 || res.TTFT.P50 > 120 {
				t.Errorf("ttft p50 %v ms, want about 40", res.TTFT.P50)
			}
			if res.Latency.P50 < 160 {
				t.Errorf("total p50 %v ms, want at least 160", res.Latency.P50)
			}
			if res.Latency.P50-res.TTFT.P50 < 100 {
				t.Errorf("total %v - ttft %v < 100 ms between deltas", res.Latency.P50, res.TTFT.P50)
			}
		})
	}
}

func TestRunExcludesWarmup(t *testing.T) {
	t.Parallel()
	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		okJSON(w)
	}))
	defer srv.Close()
	res, err := Run(context.Background(), Config{URL: srv.URL, Model: "m", Rate: 20, Warmup: 500 * time.Millisecond, Duration: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if got.Load() != 20 {
		t.Errorf("server saw %d requests, want 20 (warm-up is sent)", got.Load())
	}
	if res.WarmupRequests != 10 || res.Requests != 10 || res.Outcomes[OutcomeOK] != 10 || res.Latency.Count != 10 {
		t.Errorf("warmup %d, requests %d, outcomes %v, latency count %d; want 10 each", res.WarmupRequests, res.Requests, res.Outcomes, res.Latency.Count)
	}
}

func TestRunDropsBeyondMaxInFlight(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sleepCtx(r, 400*time.Millisecond) {
			okJSON(w)
		}
	}))
	defer srv.Close()
	// 10 requests 20 ms apart, all due while the first three are out.
	res, err := Run(context.Background(), Config{URL: srv.URL, Model: "m", Rate: 50, Duration: 200 * time.Millisecond, MaxInFlight: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{OutcomeOK: 3, OutcomeDropped: 7}
	if fmt.Sprint(res.Outcomes) != fmt.Sprint(want) {
		t.Errorf("outcomes %v, want %v", res.Outcomes, want)
	}
	if res.PeakInFlight != 3 || res.SendDelay.Count != 3 || res.Errors() != 7 {
		t.Errorf("peak %d, send delay count %d, errors %d", res.PeakInFlight, res.SendDelay.Count, res.Errors())
	}
}

func TestRunClassifiesOutcomes(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	tests := []struct {
		name    string
		stream  bool
		timeout time.Duration
		handler http.HandlerFunc
		url     string
		want    string
	}{
		{name: "500", handler: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		}, want: "http_500"},
		{name: "429 stream", stream: true, handler: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		}, want: "http_429"},
		{name: "stream ends without DONE", stream: true, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		}, want: OutcomeMalformedStream},
		{name: "stream cut mid-body", stream: true, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		}, want: OutcomeMalformedStream},
		{name: "stream bad json", stream: true, handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "data: {nope\n\ndata: [DONE]\n\n")
		}, want: OutcomeMalformedStream},
		{name: "stream error chunk", stream: true, handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"x\"}}\n\n")
		}, want: OutcomeStreamError},
		{name: "body not json", handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "<html>")
		}, want: OutcomeMalformedBody},
		{name: "timeout", timeout: 50 * time.Millisecond, handler: func(w http.ResponseWriter, r *http.Request) {
			if sleepCtx(r, time.Second) {
				okJSON(w)
			}
		}, want: OutcomeTimeout},
		{name: "stream timeout", stream: true, timeout: 50 * time.Millisecond, handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
			w.(http.Flusher).Flush()
			sleepCtx(r, time.Second)
		}, want: OutcomeTimeout},
		{name: "connection refused", url: closedURL, want: OutcomeTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url := tt.url
			if tt.handler != nil {
				srv := httptest.NewServer(tt.handler)
				defer srv.Close()
				url = srv.URL
			}
			res, err := Run(context.Background(), Config{URL: url, Model: "m", Stream: tt.stream, Rate: 10, Duration: 100 * time.Millisecond, Timeout: tt.timeout})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int{tt.want: 1}
			if fmt.Sprint(res.Outcomes) != fmt.Sprint(want) {
				t.Errorf("outcomes %v, want %v", res.Outcomes, want)
			}
			if res.Latency.Count != 0 {
				t.Errorf("failed request counted in latency: %+v", res.Latency)
			}
		})
	}
}

func TestRunInterrupted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { okJSON(w) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	res, err := Run(ctx, Config{URL: srv.URL, Model: "m", Rate: 20, Duration: 10 * time.Second})
	if err == nil || res == nil {
		t.Fatalf("Run = %v, %v; want a partial result and an error", res, err)
	}
	if res.Requests < 1 || res.Requests > 10 {
		t.Errorf("requests %d after 150 ms at 20/s", res.Requests)
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{Rate: 1, Duration: time.Second},
		{URL: "http://x", Duration: time.Second},
		{URL: "http://x", Rate: 1},
		{URL: "http://x", Rate: 1, Duration: time.Second, Format: "grpc"},
	} {
		if _, err := Run(context.Background(), cfg); err == nil {
			t.Errorf("Run(%+v) accepted a bad config", cfg)
		}
	}
}

func TestResultOmitsAPIKey(t *testing.T) {
	t.Parallel()
	const key = "sk-very-secret-bench-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { okJSON(w) }))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "r.json")
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"--url", srv.URL, "--key", key, "--rate", "20", "--duration", "100ms", "--warmup", "0s", "--label", "t", "--out", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Main = %d, stderr %s", code, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.Config.Label != "t" || res.Outcomes[OutcomeOK] != 2 {
		t.Errorf("result %+v", res)
	}
	for name, s := range map[string]string{"result": string(b), "stdout": stdout.String(), "stderr": stderr.String()} {
		if strings.Contains(s, key) {
			t.Errorf("API key appears in %s", name)
		}
	}
	if !strings.HasPrefix(stderr.String(), "t: 2 requests") {
		t.Errorf("summary %q", stderr.String())
	}
}

func TestMainUsage(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"--rate", "-1", "--url", "http://x"}, &stdout, &stderr); code != 2 {
		t.Errorf("bad rate: exit %d", code)
	}
	if code := Main(context.Background(), []string{"--nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("bad flag: exit %d", code)
	}
}

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/TheAsianFish/switchboard/internal/core"
)

const secretKey = "sk-ant-secret-0123456789"

func testRequest() *core.Request {
	return &core.Request{Model: "qwen2.5:0.5b", MaxTokens: 50, Messages: []core.Message{{Role: core.RoleUser, Text: "Name the three primary colours."}}}
}

// fixtureServer replays a recorded response with its recorded status.
func fixtureServer(t *testing.T, name string) *httptest.Server {
	t.Helper()
	dir := filepath.Join("..", "testdata", "anthropic")
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	statusText, err := os.ReadFile(filepath.Join(dir, name+".status"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(statusText)))
	if err != nil {
		t.Fatal(err)
	}
	ct := "application/json"
	if strings.HasSuffix(name, ".sse") {
		ct = "text/event-stream"
	}
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != secretKey {
			http.Error(w, "unexpected request "+r.URL.Path, http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newProvider(t *testing.T, srv *httptest.Server, timeout time.Duration) *Provider {
	t.Helper()
	p, err := New(Config{BaseURL: srv.URL, APIKey: secretKey, HTTPClient: srv.Client(), Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func collect(t *testing.T, s core.Stream) ([]core.Event, error) {
	t.Helper()
	defer func() { _ = s.Close() }()
	var evs []core.Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return evs, nil
		}
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
}

func TestFixtureComplete(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-messages.json"), 0)
	got, err := p.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := &core.Response{
		ID: "msg_a550a08212a5b5c712d83173", Model: "qwen2.5:0.5b", Text: "Red, Yellow, Blue", StopReason: core.StopEndTurn,
		Usage: core.Usage{InputTokens: 1, CacheReadTokens: 23, OutputTokens: 6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestFixtureStream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want []core.Event
	}{
		{
			name: "ollama-messages-stream.sse",
			want: []core.Event{
				{Kind: core.EventStart, ID: "msg_76b22ee21413a054e8d86e7a", Model: "qwen2.5:0.5b", Usage: core.Usage{InputTokens: 12}},
				{Kind: core.EventDelta, Text: "Red"},
				{Kind: core.EventDelta, Text: ","},
				{Kind: core.EventDelta, Text: " Yellow"},
				{Kind: core.EventDelta, Text: ","},
				{Kind: core.EventDelta, Text: " Blue"},
				{Kind: core.EventStop, StopReason: core.StopEndTurn, Usage: core.Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
			},
		},
		{
			name: "ollama-messages-stream-max-tokens.sse",
			want: []core.Event{
				{Kind: core.EventStart, ID: "msg_4c18d777108e4a1923653530", Model: "qwen2.5:0.5b", Usage: core.Usage{InputTokens: 9}},
				{Kind: core.EventDelta, Text: "Sure"},
				{Kind: core.EventDelta, Text: ","},
				{Kind: core.EventDelta, Text: " here"},
				{Kind: core.EventDelta, Text: " are"},
				{Kind: core.EventDelta, Text: " the"},
				{Kind: core.EventStop, StopReason: core.StopMaxTokens, Usage: core.Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 5}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t, fixtureServer(t, tt.name), 0)
			s, err := p.Stream(context.Background(), testRequest())
			if err != nil {
				t.Fatal(err)
			}
			got, err := collect(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("events\n got %+v\nwant %+v", got, tt.want)
			}
			if _, err := s.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("Next after the stop event: %v, want io.EOF", err)
			}
		})
	}
}

func TestFixtureUnknownModel(t *testing.T) {
	t.Parallel()
	srv := fixtureServer(t, "ollama-messages-unknown-model.json")
	p := newProvider(t, srv, 0)
	_, cerr := p.Complete(context.Background(), testRequest())
	_, serr := p.Stream(context.Background(), testRequest())
	for _, err := range []error{cerr, serr} {
		var e *core.Error
		if !errors.As(err, &e) || e.Kind != core.KindNotFound || e.Message != "upstream: model 'no-such-model' not found" {
			t.Errorf("err %v, want not_found with the API's message", err)
		}
	}
}

func TestStreamEndedEarly(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("..", "testdata", "anthropic", "ollama-messages-stream.sse"))
	if err != nil {
		t.Fatal(err)
	}
	cut := string(body[:strings.Index(string(body), "event: message_delta")])
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cut)
	})
	s, err := newProvider(t, srv, 0).Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, s)
	var e *core.Error
	if !errors.As(err, &e) || e.Kind != core.KindUpstream || e.Message != "upstream stream ended early" {
		t.Fatalf("err %v, want upstream stream ended early", err)
	}
	if len(evs) != 6 {
		t.Errorf("%d events before the cut, want 6", len(evs))
	}
	if _, again := s.Next(); !errors.Is(again, err) {
		t.Errorf("error is not sticky: %v", again)
	}
}

func TestStreamErrorEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		errType string
		kind    core.ErrorKind
	}{
		{"overloaded_error", core.KindOverloaded},
		{"rate_limit_error", core.KindRateLimited},
		{"api_error", core.KindUpstream},
		{"invalid_request_error", core.KindInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.errType, func(t *testing.T) {
			t.Parallel()
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n"+
					"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\""+tt.errType+"\",\"message\":\"Overloaded\"}}\n\n")
			})
			s, err := newProvider(t, srv, 0).Stream(context.Background(), testRequest())
			if err != nil {
				t.Fatal(err)
			}
			evs, err := collect(t, s)
			var e *core.Error
			if !errors.As(err, &e) || e.Kind != tt.kind || e.Message != "upstream: Overloaded" {
				t.Fatalf("err %v, want %s", err, tt.kind)
			}
			if len(evs) != 2 || evs[1].Text != "Hi" {
				t.Errorf("events %+v", evs)
			}
		})
	}
}

func TestHTTPErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		header     string
		body       string
		kind       core.ErrorKind
		message    string
		retryAfter time.Duration
	}{
		{"rate limited", 429, "7", `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`, core.KindRateLimited, "upstream: Number of requests has exceeded your rate limit", 7 * time.Second},
		{"overloaded 529", 529, "", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, core.KindOverloaded, "upstream: Overloaded", 0},
		{"auth", 401, "", `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, core.KindAuthentication, "upstream: invalid x-api-key", 0},
		{"bad request", 400, "", `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`, core.KindInvalidRequest, "upstream: max_tokens: too large", 0},
		{"server error, plain body", 500, "", `oops`, core.KindUpstream, "upstream: Internal Server Error", 0},
		{"bad gateway, no body", 502, "", ``, core.KindUpstream, "upstream: Bad Gateway", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			p := newProvider(t, srv, 0)
			_, cerr := p.Complete(context.Background(), testRequest())
			_, serr := p.Stream(context.Background(), testRequest())
			for i, err := range []error{cerr, serr} {
				var e *core.Error
				if !errors.As(err, &e) || e.Kind != tt.kind || e.Message != tt.message || e.RetryAfter != tt.retryAfter {
					t.Errorf("call %d: err %#v, want %s %q retry %s", i, e, tt.kind, tt.message, tt.retryAfter)
				}
				if strings.Contains(err.Error(), secretKey) {
					t.Errorf("call %d: error leaks the API key: %v", i, err)
				}
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{"0.5", 500 * time.Millisecond},
		{"-1", 0},
		{"soon", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := retryAfter(tt.in, now); got != tt.want {
			t.Errorf("retryAfter(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hanging up.
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("X-Test-Mode") == "stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p := newProvider(t, srv, 100*time.Millisecond)

	_, err := p.Complete(context.Background(), testRequest())
	assertKind(t, "complete", err, core.KindTimeout)

	// A stream whose first event arrived still runs into the deadline.
	sp, err := New(Config{BaseURL: srv.URL, HTTPClient: &http.Client{Transport: headerTransport{srv.Client().Transport}}, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sp.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, s)
	if len(evs) != 1 || evs[0].Kind != core.EventStart {
		t.Errorf("events %+v", evs)
	}
	assertKind(t, "stream", err, core.KindTimeout)
}

// headerTransport marks requests so the test server streams.
type headerTransport struct{ rt http.RoundTripper }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Mode", "stream")
	return h.rt.RoundTrip(r)
}

func assertKind(t *testing.T, what string, err error, kind core.ErrorKind) {
	t.Helper()
	var e *core.Error
	if !errors.As(err, &e) || e.Kind != kind {
		t.Errorf("%s: err %v, want kind %s", what, err, kind)
	}
}

func TestConnectionRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	p, err := New(Config{BaseURL: url, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), testRequest())
	assertKind(t, "complete", err, core.KindUpstream)
	_, err = p.Stream(context.Background(), testRequest())
	assertKind(t, "stream", err, core.KindUpstream)
}

func TestCloseBeforeDrain(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-messages-stream.sse"), 0)
	s, err := p.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := s.Next(); err != nil || ev.Kind != core.EventStart {
		t.Fatalf("first event %+v, %v", ev, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if ev, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next after Close: %+v, %v; want io.EOF", ev, err)
	}
}

func TestRequestTranslation(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"t","name":"n","input":{}},{"type":"text","text":"b"}],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`)
	})
	p, err := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), DefaultMaxTokens: 77})
	if err != nil {
		t.Fatal(err)
	}
	req := &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "q"}}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(77) {
		t.Errorf("max_tokens %v, want the configured default 77", got["max_tokens"])
	}
	for _, k := range []string{"system", "temperature", "top_p", "stop_sequences", "stream", "tools", "tool_choice"} {
		if _, ok := got[k]; ok {
			t.Errorf("unset field %q was sent", k)
		}
	}
	want := &core.Response{ID: "msg_1", Model: "m", Text: "ab", ToolCalls: []core.ToolCall{{ID: "t", Name: "n", Arguments: json.RawMessage(`{}`)}}, StopReason: core.StopContentFilter, Usage: core.Usage{InputTokens: 1, OutputTokens: 2, CacheWriteTokens: 3, CacheReadTokens: 4}}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("got %+v\nwant %+v", resp, want)
	}
}

func TestLargeMaxTokensNonStreaming(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-messages.json"), 0)
	req := testRequest()
	req.MaxTokens = 64000 // the SDK refuses this without an explicit timeout
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"defaults", Config{}, true},
		{"ollama", Config{BaseURL: "http://localhost:11434"}, true},
		{"relative url", Config{BaseURL: "localhost:11434"}, false},
		{"ftp url", Config{BaseURL: "ftp://host"}, false},
		{"negative timeout", Config{Timeout: -time.Second}, false},
		{"negative max tokens", Config{DefaultMaxTokens: -1}, false},
	}
	for _, tt := range tests {
		p, err := New(tt.cfg)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err %v", tt.name, err)
		}
		if tt.ok && (p.timeout != defaultTimeout || p.maxTokens != defaultMaxTokens) {
			t.Errorf("%s: defaults not applied: %s %d", tt.name, p.timeout, p.maxTokens)
		}
	}
}

func TestStopReason(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]core.StopReason{
		"end_turn": core.StopEndTurn, "max_tokens": core.StopMaxTokens, "stop_sequence": core.StopSequence,
		"model_context_window_exceeded": core.StopMaxTokens,
		"refusal":                       core.StopContentFilter, "tool_use": core.StopToolUse, "pause_turn": core.StopEndTurn, "": core.StopEndTurn, "unknown": core.StopEndTurn,
	} {
		if got := stopReason(sdk.StopReason(in)); got != want {
			t.Errorf("stopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

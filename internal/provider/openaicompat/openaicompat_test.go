package openaicompat

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

	"github.com/TheAsianFish/switchboard/internal/core"
)

const fixtures = "../testdata/openai"

// fixtureServer replays a recorded body with its recorded status.
func fixtureServer(t *testing.T, name string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	rawStatus, err := os.ReadFile(filepath.Join(fixtures, name+".status"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(rawStatus)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if strings.HasSuffix(name, ".sse") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProvider(t *testing.T, url string, timeout time.Duration) *Provider {
	t.Helper()
	p, err := New(Config{BaseURL: url + "/v1", Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func request(stream bool) *core.Request {
	return &core.Request{Model: "qwen2.5:0.5b", Messages: []core.Message{{Role: core.RoleUser, Text: "Name the primary colours"}}, Stream: stream}
}

func TestNewValidatesBaseURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"", "localhost:11434", "ftp://host/v1", "http://", "http://h/v1?x=1", "://bad"} {
		if _, err := New(Config{BaseURL: u}); err == nil {
			t.Errorf("New(%q) succeeded", u)
		}
	}
	p, err := New(Config{BaseURL: "http://localhost:11434/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	if p.endpoint != "http://localhost:11434/v1/chat/completions" || p.timeout != DefaultTimeout {
		t.Errorf("endpoint %q timeout %v", p.endpoint, p.timeout)
	}
	tr, ok := p.client.Transport.(*http.Transport)
	if !ok || tr.MaxIdleConnsPerHost != 256 || tr.DisableKeepAlives {
		t.Errorf("transport %+v", p.client.Transport)
	}
}

func TestFixtureComplete(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-chat.json").URL, 0)
	got, err := p.Complete(context.Background(), request(false))
	if err != nil {
		t.Fatal(err)
	}
	want := &core.Response{
		ID:         "chatcmpl-599",
		Model:      "qwen2.5:0.5b",
		Text:       "Red, Yellow, Blue",
		StopReason: core.StopEndTurn,
		Usage:      core.Usage{InputTokens: 21, CacheReadTokens: 3, OutputTokens: 6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestFixtureStream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fixture string
		id      string
		deltas  []string
		stop    core.StopReason
		usage   core.Usage
	}{
		{"ollama-chat-stream.sse", "chatcmpl-719", []string{"Red", ",", " Yellow", ",", " Blue"}, core.StopEndTurn, core.Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
		{"ollama-chat-stream-length.sse", "chatcmpl-917", []string{"Sure", ",", " here", " are", " the"}, core.StopMaxTokens, core.Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t, fixtureServer(t, tt.fixture).URL, 0)
			s, err := p.Stream(context.Background(), request(true))
			if err != nil {
				t.Fatal(err)
			}
			got := drain(t, s)
			want := []core.Event{{Kind: core.EventStart, ID: tt.id, Model: "qwen2.5:0.5b"}}
			for _, d := range tt.deltas {
				want = append(want, core.Event{Kind: core.EventDelta, Text: d})
			}
			want = append(want, core.Event{Kind: core.EventStop, StopReason: tt.stop, Usage: tt.usage})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

// drain reads s to io.EOF, failing on any other error.
func drain(t *testing.T, s core.Stream) []core.Event {
	t.Helper()
	defer func() { _ = s.Close() }()
	var evs []core.Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			// A drained stream keeps returning io.EOF.
			if _, err := s.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("second EOF: %v", err)
			}
			return evs
		}
		if err != nil {
			t.Fatalf("after %d events: %v", len(evs), err)
		}
		evs = append(evs, ev)
	}
}

func TestFixtureUnknownModel(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-chat-unknown-model.json").URL, 0)
	_, cErr := p.Complete(context.Background(), request(false))
	_, sErr := p.Stream(context.Background(), request(true))
	for _, err := range []error{cErr, sErr} {
		var e *core.Error
		if !errors.As(err, &e) || e.Kind != core.KindNotFound {
			t.Fatalf("err = %v, want not found", err)
		}
		if e.Message != "upstream: model 'no-such-model' not found" {
			t.Errorf("message %q", e.Message)
		}
	}
}

// sseServer serves a hand-built stream to exercise failure paths.
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	chunkStart  = `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}` + "\n\n"
	chunkFinish = `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
)

func TestStreamFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		kind    core.ErrorKind
		message string
		events  int // events before the error
	}{
		{"cut before finish", chunkStart, core.KindUpstream, "upstream stream ended early", 2},
		{"done before finish", chunkStart + "data: [DONE]\n\n", core.KindUpstream, "upstream stream ended early", 2},
		{"empty body", "", core.KindUpstream, "upstream stream ended early", 0},
		{"error chunk", chunkStart + `data: {"error":{"message":"model crashed"}}` + "\n\n", core.KindUpstream, "upstream: model crashed", 2},
		{"invalid chunk", chunkStart + "data: {nope\n\n", core.KindUpstream, "upstream: invalid stream chunk", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t, sseServer(t, tt.body).URL, 0)
			s, err := p.Stream(context.Background(), request(true))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			n := 0
			for {
				_, err = s.Next()
				if err != nil {
					break
				}
				n++
			}
			var e *core.Error
			if !errors.As(err, &e) || e.Kind != tt.kind || e.Message != tt.message {
				t.Fatalf("err = %v, want %s %q", err, tt.kind, tt.message)
			}
			if n != tt.events {
				t.Errorf("%d events before the error, want %d", n, tt.events)
			}
			if _, again := s.Next(); !errors.Is(again, err) {
				t.Errorf("error is not sticky: %v", again)
			}
		})
	}
}

func TestStreamCleanEOFAfterFinish(t *testing.T) {
	t.Parallel()
	p := newProvider(t, sseServer(t, chunkStart+chunkFinish).URL, 0)
	s, err := p.Stream(context.Background(), request(true))
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, s)
	if len(evs) != 3 || evs[2].Kind != core.EventStop || evs[2].StopReason != core.StopEndTurn {
		t.Errorf("events %+v", evs)
	}
}

func TestStatusErrors(t *testing.T) {
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
		{"rate limited", 429, "7", `{"error":{"message":"too many requests","type":"rate_limit"}}`, core.KindRateLimited, "upstream: too many requests", 7 * time.Second},
		{"overloaded", 503, "2", `not json`, core.KindOverloaded, "upstream: Service Unavailable", 2 * time.Second},
		{"string error", 400, "", `{"error":"bad stop"}`, core.KindInvalidRequest, "upstream: bad stop", 0},
		{"vllm style", 400, "", `{"object":"error","message":"max_tokens too large"}`, core.KindInvalidRequest, "upstream: max_tokens too large", 0},
		{"bad key", 401, "", `{"error":{"message":"bad key"}}`, core.KindAuthentication, "upstream: bad key", 0},
		{"server error", 500, "soon", ``, core.KindUpstream, "upstream: Internal Server Error", 0},
		{"gateway timeout", 504, "", ``, core.KindTimeout, "upstream: Gateway Timeout", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			p := newProvider(t, srv.URL, 0)
			_, cErr := p.Complete(context.Background(), request(false))
			_, sErr := p.Stream(context.Background(), request(true))
			for _, err := range []error{cErr, sErr} {
				var e *core.Error
				if !errors.As(err, &e) {
					t.Fatalf("err = %v", err)
				}
				if e.Kind != tt.kind || e.Message != tt.message || e.RetryAfter != tt.retryAfter {
					t.Errorf("got %s %q %v, want %s %q %v", e.Kind, e.Message, e.RetryAfter, tt.kind, tt.message, tt.retryAfter)
				}
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			// Send the headers and one chunk, then stall mid-stream.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chunkStart)
			w.(http.Flusher).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	p := newProvider(t, srv.URL, 100*time.Millisecond)

	_, err := p.Complete(context.Background(), request(false))
	var e *core.Error
	if !errors.As(err, &e) || e.Kind != core.KindTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("complete err = %v, want timeout", err)
	}

	s, err := p.Stream(context.Background(), request(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for {
		_, err = s.Next()
		if err != nil {
			break
		}
	}
	if !errors.As(err, &e) || e.Kind != core.KindTimeout {
		t.Errorf("stream err = %v, want timeout", err)
	}
}

func TestTransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	p := newProvider(t, url, 0)
	_, err := p.Complete(context.Background(), request(false))
	var e *core.Error
	if !errors.As(err, &e) || e.Kind != core.KindUpstream || e.Err == nil {
		t.Errorf("err = %v, want upstream with a cause", err)
	}
}

func TestCloseBeforeDrain(t *testing.T) {
	t.Parallel()
	gone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunkStart)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL, 0)
	s, err := p.Stream(context.Background(), request(true))
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := s.Next(); err != nil || ev.Kind != core.EventStart {
		t.Fatalf("first event %+v %v", ev, err)
	}
	_ = s.Close()
	_ = s.Close()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request not cancelled by Close")
	}
}

func TestRequestBody(t *testing.T) {
	t.Parallel()
	got := make(chan map[string]any, 1)
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		got <- m
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunkStart+chunkFinish+"data: [DONE]\n\n")
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL + "/v1", APIKey: "upstream-key"})
	if err != nil {
		t.Fatal(err)
	}
	temp := 0.0
	req := &core.Request{
		Model: "m", System: "sys", MaxTokens: 7, Temperature: &temp, Stop: []string{"x"},
		Messages: []core.Message{{Role: core.RoleUser, Text: "q"}, {Role: core.RoleAssistant, Text: "a"}},
	}
	s, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	want := `{"max_tokens":7,"messages":[{"content":"sys","role":"system"},{"content":"q","role":"user"},{"content":"a","role":"assistant"}],"model":"m","stop":["x"],"stream":true,"stream_options":{"include_usage":true},"temperature":0}`
	b, _ := json.Marshal(<-got)
	if string(b) != want {
		t.Errorf("body %s\nwant %s", b, want)
	}
	if auth != "Bearer upstream-key" {
		t.Errorf("auth %q", auth)
	}
}

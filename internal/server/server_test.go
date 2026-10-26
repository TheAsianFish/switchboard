package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

func newHandler(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	gw := pipeline.Open{Provider: fake.New(fake.Config{Tokens: 3}), Names: []string{"fake"}}
	return Handler(gw, log), &buf
}

func TestRequestID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"fresh id when none is sent", "", false},
		{"caller's id kept", "abc-123_x.y", true},
		{"unsafe id replaced", "bad id\nwith newline", false},
		{"overlong id replaced", strings.Repeat("a", 129), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, logs := newHandler(t)
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tt.incoming != "" {
				req.Header.Set("X-Request-Id", tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			id := rec.Header().Get("X-Request-Id")
			if rec.Header().Get("Request-Id") != id {
				t.Errorf("request-id %q differs from x-request-id %q", rec.Header().Get("Request-Id"), id)
			}
			if tt.keep && id != tt.incoming {
				t.Errorf("id = %q, want %q", id, tt.incoming)
			}
			if !tt.keep && !strings.HasPrefix(id, "req_") {
				t.Errorf("id = %q, want a generated one", id)
			}
			var line map[string]any
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
				t.Fatalf("access log is not one JSON line: %v\n%s", err, logs)
			}
			if line["request_id"] != id || line["path"] != "/healthz" || line["status"] != float64(200) {
				t.Errorf("access log = %v", line)
			}
		})
	}
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		method     string
		path       string
		header     map[string]string
		wantStatus int
		wantBody   string
	}{
		{"health", "GET", "/healthz", nil, 200, "ok"},
		{"unknown path", "GET", "/v2/nothing", nil, 404, `no route for GET /v2/nothing`},
		{"wrong method", "GET", "/v1/messages", nil, 405, ""},
		{"openai models", "GET", "/v1/models", map[string]string{"Authorization": "Bearer x"}, 200, `"object":"list"`},
		{"anthropic models", "GET", "/v1/models", map[string]string{"x-api-key": "x", "anthropic-version": "2023-06-01"}, 200, `"has_more":false`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newHandler(t)
			req := httptest.NewRequest(tt.method, tt.path, nil)
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body %q lacks %q", rec.Body, tt.wantBody)
			}
		})
	}
}

func TestPanicRecovered(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := withRequestContext(slog.New(slog.NewJSONHandler(&buf, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal error") {
		t.Errorf("got %d %q", rec.Code, rec.Body)
	}
	if !strings.Contains(buf.String(), `"panic":"boom"`) {
		t.Errorf("panic not logged: %s", buf.String())
	}
}

// TestStreamsAreFlushed checks that a streamed token reaches the client
// before the stream ends, through the access-log wrapper.
func TestStreamsAreFlushed(t *testing.T) {
	t.Parallel()
	gw := pipeline.Open{Provider: fake.New(fake.Config{Tokens: 3, TokenInterval: time.Hour}), Names: []string{"fake"}}
	srv := httptest.NewServer(Handler(gw, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := `{"model":"fake","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The first token is sent immediately and the second an hour later, so
	// reading the first content chunk only succeeds if it was flushed.
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for !bytes.Contains(buf, []byte(`"content":"`)) || bytes.Count(buf, []byte("data: ")) < 2 {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			t.Fatalf("read: %v after %q", err, buf)
		}
	}
}

func TestRunShutsDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan net.Addr, 1)
	done := make(chan error, 1)
	h, _ := newHandler(t)
	go func() {
		done <- Run(ctx, "127.0.0.1:0", h, slog.New(slog.DiscardHandler), func(a net.Addr) { addr <- a })
	}()
	a := <-addr
	resp, err := http.Get("http://" + a.String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

var _ core.Gateway = pipeline.Open{}

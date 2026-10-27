package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

const testKey = "sk-test"

// testGateway accepts testKey and serves from a provider.
type testGateway struct {
	p      core.Provider
	models []string
}

func (g *testGateway) Authenticate(ctx context.Context, key string) (context.Context, error) {
	if key != testKey {
		return nil, core.Errorf(core.KindAuthentication, "invalid API key")
	}
	return ctx, nil
}

func (g *testGateway) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	return g.p.Complete(ctx, req)
}

func (g *testGateway) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	return g.p.Stream(ctx, req)
}

func (g *testGateway) Models(context.Context) []string { return g.models }

// funcProvider adapts functions to core.Provider.
type funcProvider struct {
	complete func(ctx context.Context, req *core.Request) (*core.Response, error)
	stream   func(ctx context.Context, req *core.Request) (core.Stream, error)
}

func (f funcProvider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	return f.complete(ctx, req)
}

func (f funcProvider) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	return f.stream(ctx, req)
}

// scriptStream yields events, then err (io.EOF when nil), and records Close.
type scriptStream struct {
	events []core.Event
	err    error
	// block makes Next wait for ctx once the events are used up.
	block  context.Context
	once   sync.Once
	closed chan struct{}
}

func newScriptStream(err error, events ...core.Event) *scriptStream {
	return &scriptStream{events: events, err: err, closed: make(chan struct{})}
}

func (s *scriptStream) Next() (core.Event, error) {
	if len(s.events) > 0 {
		ev := s.events[0]
		s.events = s.events[1:]
		return ev, nil
	}
	if s.block != nil {
		<-s.block.Done()
		return core.Event{}, core.Wrap(core.KindTimeout, s.block.Err(), "request cancelled")
	}
	if s.err != nil {
		return core.Event{}, s.err
	}
	return core.Event{}, io.EOF
}

func (s *scriptStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *scriptStream) wasClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func streamGateway(s core.Stream) *testGateway {
	return &testGateway{p: funcProvider{stream: func(context.Context, *core.Request) (core.Stream, error) { return s, nil }}}
}

func post(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errorOf(t *testing.T, body []byte) errorObject {
	t.Helper()
	var e struct {
		Error *errorObject `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error == nil {
		t.Fatalf("not an OpenAI error body: %s", body)
	}
	return *e.Error
}

const simpleBody = `{"model":"m","messages":[{"role":"user","content":"hello world"}]}`

func TestAuth(t *testing.T) {
	t.Parallel()
	gw := &testGateway{p: fake.New(fake.Config{Tokens: 3}), models: []string{"a"}}
	tests := []struct {
		name   string
		header map[string]string
		status int
	}{
		{"missing", nil, 401},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, 401},
		{"wrong key", map[string]string{"Authorization": "Bearer nope"}, 401},
		{"bearer", map[string]string{"Authorization": "Bearer " + testKey}, 200},
		{"lowercase scheme", map[string]string{"Authorization": "bearer " + testKey}, 200},
		{"x-api-key", map[string]string{"x-api-key": testKey}, 200},
		{"basic falls back to x-api-key", map[string]string{"Authorization": "Basic Zm9v", "x-api-key": testKey}, 200},
	}
	for _, tt := range tests {
		for _, path := range []string{"chat", "models"} {
			t.Run(tt.name+"/"+path, func(t *testing.T) {
				t.Parallel()
				var r *http.Request
				var h http.Handler
				if path == "chat" {
					r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(simpleBody))
					h = ChatCompletions(gw)
				} else {
					r = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
					h = ListModels(gw)
				}
				for k, v := range tt.header {
					r.Header.Set(k, v)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tt.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body)
				}
				if tt.status == 401 {
					e := errorOf(t, w.Body.Bytes())
					if e.Type != "invalid_request_error" || e.Code == nil || *e.Code != "invalid_api_key" || e.Message == "" {
						t.Errorf("error = %+v", e)
					}
				}
			})
		}
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()
	gw := &testGateway{models: []string{"small", "large"}}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	ListModels(gw).ServeHTTP(w, r)
	want := `{"object":"list","data":[{"id":"small","object":"model","created":0,"owned_by":"switchboard"},{"id":"large","object":"model","created":0,"owned_by":"switchboard"}]}` + "\n"
	if w.Code != 200 || w.Body.String() != want {
		t.Errorf("got %d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
}

func TestRequestErrors(t *testing.T) {
	t.Parallel()
	gw := &testGateway{p: fake.New(fake.Config{Tokens: 3})}
	h := ChatCompletions(gw)

	w := post(t, h, testKey, `{"model":`)
	if w.Code != 400 || errorOf(t, w.Body.Bytes()).Type != "invalid_request_error" {
		t.Errorf("malformed: %d %s", w.Code, w.Body)
	}
	w = post(t, h, testKey, `{"model":"m","tools":[{}],"messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 400 || !strings.Contains(errorOf(t, w.Body.Bytes()).Message, "tool use is not supported yet") {
		t.Errorf("tools: %d %s", w.Code, w.Body)
	}
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", maxBodyBytes) + `"}]}`
	w = post(t, h, testKey, big)
	if w.Code != 413 || errorOf(t, w.Body.Bytes()).Type != "invalid_request_error" {
		t.Errorf("too large: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestErrorKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err        error
		status     int
		typ        string
		code       string
		message    string
		retryAfter string
	}{
		{core.Errorf(core.KindInvalidRequest, "bad"), 400, "invalid_request_error", "", "bad", ""},
		{core.Errorf(core.KindAuthentication, "invalid API key"), 401, "invalid_request_error", "invalid_api_key", "invalid API key", ""},
		{core.Errorf(core.KindPermission, "model not allowed for this key"), 403, "permission_error", "", "model not allowed for this key", ""},
		{core.Errorf(core.KindNotFound, "model \"x\" does not exist"), 404, "invalid_request_error", "model_not_found", "model \"x\" does not exist", ""},
		{core.Errorf(core.KindNotFound, "no such thing"), 404, "invalid_request_error", "", "no such thing", ""},
		{&core.Error{Kind: core.KindRateLimited, Message: "slow down", RetryAfter: 1500 * time.Millisecond}, 429, "rate_limit_error", "rate_limit_exceeded", "slow down", "2"},
		{&core.Error{Kind: core.KindOverloaded, Message: "busy", RetryAfter: 3 * time.Second}, 503, "server_error", "overloaded", "busy", "3"},
		{&core.Error{Kind: core.KindBudgetExceeded, Message: "budget spent"}, 429, "insufficient_quota", "insufficient_quota", "budget spent", ""},
		{core.Errorf(core.KindTimeout, "upstream timed out"), 504, "server_error", "timeout", "upstream timed out", ""},
		{core.Errorf(core.KindUpstream, "upstream: boom"), 502, "server_error", "upstream_error", "upstream: boom", ""},
		{core.Wrap(core.KindInternal, io.ErrUnexpectedEOF, "secret detail"), 500, "server_error", "", "internal error", ""},
		{io.ErrClosedPipe, 500, "server_error", "", "internal error", ""},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			name := core.AsError(tt.err).Kind.String()
			if stream {
				name += "/stream"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				p := funcProvider{
					complete: func(context.Context, *core.Request) (*core.Response, error) { return nil, tt.err },
					stream:   func(context.Context, *core.Request) (core.Stream, error) { return nil, tt.err },
				}
				body := simpleBody
				if stream {
					body = `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`
				}
				w := post(t, ChatCompletions(&testGateway{p: p}), testKey, body)
				if w.Code != tt.status {
					t.Fatalf("status = %d, want %d", w.Code, tt.status)
				}
				e := errorOf(t, w.Body.Bytes())
				code := ""
				if e.Code != nil {
					code = *e.Code
				}
				if e.Type != tt.typ || code != tt.code || e.Message != tt.message {
					t.Errorf("error = %+v code %q", e, code)
				}
				if got := w.Header().Get("Retry-After"); got != tt.retryAfter {
					t.Errorf("Retry-After = %q, want %q", got, tt.retryAfter)
				}
			})
		}
	}
}

func TestCompleteBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		resp   core.Response
		id     string
		finish string
	}{
		{"end turn", core.Response{ID: "abc", Model: "served", Text: "hi \"there\"\n", StopReason: core.StopEndTurn, Usage: core.Usage{InputTokens: 4, CacheReadTokens: 3, CacheWriteTokens: 1, OutputTokens: 2}}, "chatcmpl-abc", "stop"},
		{"prefixed id, length", core.Response{ID: "chatcmpl-9", Model: "served", Text: "x", StopReason: core.StopMaxTokens, Usage: core.Usage{InputTokens: 8, OutputTokens: 2}}, "chatcmpl-9", "length"},
		{"stop sequence", core.Response{ID: "q", Model: "served", StopReason: core.StopSequence}, "chatcmpl-q", "stop"},
		{"content filter", core.Response{ID: "q", Model: "served", StopReason: core.StopContentFilter}, "chatcmpl-q", "content_filter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := funcProvider{complete: func(context.Context, *core.Request) (*core.Response, error) { r := tt.resp; return &r, nil }}
			before := time.Now().Unix()
			w := post(t, ChatCompletions(&testGateway{p: p}), testKey, simpleBody)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			created := int64(got["created"].(float64))
			if created < before || created > time.Now().Unix() {
				t.Errorf("created = %d", created)
			}
			delete(got, "created")
			u := tt.resp.Usage
			want := map[string]any{
				"id":     tt.id,
				"object": "chat.completion",
				"model":  "served",
				"choices": []any{map[string]any{
					"index":         0.0,
					"message":       map[string]any{"role": "assistant", "content": tt.resp.Text, "refusal": nil},
					"logprobs":      nil,
					"finish_reason": tt.finish,
				}},
				"usage": map[string]any{
					"prompt_tokens":         float64(u.PromptTokens()),
					"completion_tokens":     float64(u.OutputTokens),
					"total_tokens":          float64(u.PromptTokens() + u.OutputTokens),
					"prompt_tokens_details": map[string]any{"cached_tokens": float64(u.CacheReadTokens)},
				},
			}
			if !jsonEqual(got, want) {
				t.Errorf("got  %s\nwant %s", mustJSON(got), mustJSON(want))
			}
		})
	}
}

func TestCompleteGeneratesID(t *testing.T) {
	t.Parallel()
	p := funcProvider{complete: func(context.Context, *core.Request) (*core.Response, error) { return &core.Response{Model: "m"}, nil }}
	w := post(t, ChatCompletions(&testGateway{p: p}), testKey, simpleBody)
	var got completion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ID, "chatcmpl-") || len(got.ID) <= len("chatcmpl-") {
		t.Errorf("id = %q", got.ID)
	}
}

func jsonEqual(a, b any) bool { return mustJSON(a) == mustJSON(b) }

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// sseData returns the data payloads of an SSE body.
func sseData(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	s := bufio.NewScanner(strings.NewReader(body))
	for s.Scan() {
		if d, ok := strings.CutPrefix(s.Text(), "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestStreamRender(t *testing.T) {
	t.Parallel()
	events := []core.Event{
		{Kind: core.EventStart, ID: "s1", Model: "served", Usage: core.Usage{InputTokens: 5, CacheReadTokens: 2}},
		{Kind: core.EventDelta, Text: "Hel"},
		{Kind: core.EventDelta, Text: ""},
		{Kind: core.EventDelta, Text: "lo\n"},
		{Kind: core.EventStop, StopReason: core.StopMaxTokens, Usage: core.Usage{OutputTokens: 2}},
	}
	head := `{"id":"chatcmpl-s1","object":"chat.completion.chunk","created":0,"model":"served","choices":[`
	want := []string{
		head + `{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}`,
		head + `{"index":0,"delta":{"content":"Hel"},"logprobs":null,"finish_reason":null}]}`,
		head + `{"index":0,"delta":{"content":"lo\n"},"logprobs":null,"finish_reason":null}]}`,
		head + `{"index":0,"delta":{},"logprobs":null,"finish_reason":"length"}]}`,
	}
	usage := head + `],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9,"prompt_tokens_details":{"cached_tokens":2}}}`
	for _, includeUsage := range []bool{false, true} {
		name := "without usage"
		if includeUsage {
			name = "with usage"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newScriptStream(nil, events...)
			body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`
			if includeUsage {
				body = `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`
			}
			w := post(t, ChatCompletions(streamGateway(s)), testKey, body)
			if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
			}
			got := sseData(t, w.Body.String())
			exp := append([]string(nil), want...)
			if includeUsage {
				exp = append(exp, usage)
			}
			exp = append(exp, "[DONE]")
			if len(got) != len(exp) {
				t.Fatalf("got %d events:\n%s", len(got), strings.Join(got, "\n"))
			}
			for i := range exp {
				if g := zeroCreated(t, got[i]); g != exp[i] {
					t.Errorf("event %d:\ngot  %s\nwant %s", i, g, exp[i])
				}
			}
			if !s.wasClosed() {
				t.Error("stream not closed")
			}
		})
	}
}

// zeroCreated replaces the created timestamp so chunks compare exactly.
func zeroCreated(t *testing.T, data string) string {
	t.Helper()
	if data == "[DONE]" {
		return data
	}
	var c chunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		t.Fatalf("chunk %s: %v", data, err)
	}
	if c.Created == 0 {
		t.Errorf("created missing in %s", data)
	}
	i := strings.Index(data, `"created":`)
	j := strings.Index(data[i:], ",")
	return data[:i] + `"created":0` + data[i+j:]
}

func TestStreamErrorBeforeFirstEvent(t *testing.T) {
	t.Parallel()
	start := []core.Event{{Kind: core.EventStart, ID: "x", Model: "m"}}
	tests := []struct {
		name   string
		events []core.Event
		err    error
		status int
	}{
		{"rate limited", nil, &core.Error{Kind: core.KindRateLimited, Message: "slow down", RetryAfter: time.Second}, 429},
		{"not found", nil, core.Errorf(core.KindNotFound, "upstream: model 'x' not found"), 404},
		{"empty stream", nil, nil, 502},
		// Anthropic sends message_start before generating and can still
		// fail with overloaded_error: that is before the first token too.
		{"overloaded after start", start, core.Errorf(core.KindOverloaded, "busy"), 503},
		{"ended after start", start, nil, 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScriptStream(tt.err, tt.events...)
			w := post(t, ChatCompletions(streamGateway(s)), testKey, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)
			if w.Code != tt.status {
				t.Fatalf("status %d, want %d", w.Code, tt.status)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content type %q", ct)
			}
			errorOf(t, w.Body.Bytes())
			if !s.wasClosed() {
				t.Error("stream not closed")
			}
		})
	}
}

func TestStreamMidError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		typ  string
		msg  string
	}{
		{"upstream", core.Errorf(core.KindUpstream, "upstream: connection reset"), "server_error", "upstream: connection reset"},
		{"internal", core.Wrap(core.KindInternal, io.ErrUnexpectedEOF, "private"), "server_error", "internal error"},
		{"cut short", nil, "server_error", "upstream stream ended early"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScriptStream(tt.err, core.Event{Kind: core.EventStart, ID: "x", Model: "m"}, core.Event{Kind: core.EventDelta, Text: "partial"})
			w := post(t, ChatCompletions(streamGateway(s)), testKey, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)
			if w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
			got := sseData(t, w.Body.String())
			if len(got) != 3 {
				t.Fatalf("events: %v", got)
			}
			e := errorOf(t, []byte(got[2]))
			if e.Type != tt.typ || e.Message != tt.msg {
				t.Errorf("error = %+v", e)
			}
			if strings.Contains(w.Body.String(), "[DONE]") {
				t.Error("[DONE] after an error")
			}
			if !s.wasClosed() {
				t.Error("stream not closed")
			}
		})
	}
}

func TestStreamClientGoneClosesStream(t *testing.T) {
	t.Parallel()
	var s *scriptStream
	ready := make(chan struct{})
	p := funcProvider{stream: func(ctx context.Context, _ *core.Request) (core.Stream, error) {
		s = newScriptStream(nil, core.Event{Kind: core.EventStart, ID: "x", Model: "m"}, core.Event{Kind: core.EventDelta, Text: "first"})
		s.block = ctx
		close(ready)
		return s, nil
	}}
	srv := httptest.NewServer(ChatCompletions(&testGateway{p: p}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "first") {
			break
		}
	}
	cancel()
	<-ready
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("provider stream was not closed after the client went away")
	}
}

func TestStreamFake(t *testing.T) {
	t.Parallel()
	p := fake.New(fake.Config{Tokens: 5})
	w := post(t, ChatCompletions(&testGateway{p: p}), testKey, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"one two"}]}`)
	want, err := p.Complete(context.Background(), &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "one two"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var u *usage
	for _, d := range sseData(t, w.Body.String()) {
		if d == "[DONE]" {
			continue
		}
		var c chunk
		if err := json.Unmarshal([]byte(d), &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Choices) > 0 && c.Choices[0].Delta.Content != nil {
			text.WriteString(*c.Choices[0].Delta.Content)
		}
		if c.Usage != nil {
			u = c.Usage
		}
	}
	if text.String() != want.Text {
		t.Errorf("text %q, want %q", text.String(), want.Text)
	}
	if u == nil || u.PromptTokens != 2 || u.CompletionTokens != 5 || u.TotalTokens != 7 {
		t.Errorf("usage %+v", u)
	}
}

package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

func ptr[T any](v T) *T { return &v }

func TestParseRequest(t *testing.T) {
	t.Parallel()
	user := func(s string) core.Message { return core.Message{Role: core.RoleUser, Text: s} }
	tests := []struct {
		name string
		body string
		want core.Request
	}{
		{
			name: "string content",
			body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", MaxTokens: 5, Messages: []core.Message{user("hi")}},
		},
		{
			name: "text blocks joined, cache_control ignored",
			body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]}]}`,
			want: core.Request{Model: "m", MaxTokens: 5, Messages: []core.Message{user("ab")}},
		},
		{
			name: "system string and conversation",
			body: `{"model":"m","max_tokens":5,"system":"be brief","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"q2"}]}`,
			want: core.Request{Model: "m", MaxTokens: 5, System: "be brief", Messages: []core.Message{user("q"), {Role: core.RoleAssistant, Text: "a"}, user("q2")}},
		},
		{
			name: "system blocks joined with blank line",
			body: `{"model":"m","max_tokens":5,"system":[{"type":"text","text":"one"},{"type":"text","text":"two","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"q"}]}`,
			want: core.Request{Model: "m", MaxTokens: 5, System: "one\n\ntwo", Messages: []core.Message{user("q")}},
		},
		{
			name: "sampling, stop and stream",
			body: `{"model":"m","max_tokens":7,"temperature":0.5,"top_p":0.9,"stop_sequences":["\n\n","END"],"stream":true,"messages":[{"role":"user","content":"q"}]}`,
			want: core.Request{Model: "m", MaxTokens: 7, Temperature: ptr(0.5), TopP: ptr(0.9), Stop: []string{"\n\n", "END"}, Stream: true, Messages: []core.Message{user("q")}},
		},
		{
			name: "ignored fields",
			body: `{"model":"m","max_tokens":5,"top_k":40,"metadata":{"user_id":"u"},"service_tier":"auto","tools":[],"thinking":{"type":"disabled"},"unknown":1,"messages":[{"role":"user","content":"q"}]}`,
			want: core.Request{Model: "m", MaxTokens: 5, Messages: []core.Message{user("q")}},
		},
		{
			name: "null system and empty stop",
			body: `{"model":"m","max_tokens":5,"system":null,"stop_sequences":[],"messages":[{"role":"user","content":"q"}]}`,
			want: core.Request{Model: "m", MaxTokens: 5, Messages: []core.Message{user("q")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRequest(strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("got %+v\nwant %+v", *got, tt.want)
			}
		})
	}
}

func TestParseRequestInvalid(t *testing.T) {
	t.Parallel()
	const msgs = `"messages":[{"role":"user","content":"q"}]`
	tests := []struct {
		name string
		body string
		want string
	}{
		{"malformed json", `{"model":`, "not valid JSON"},
		{"not an object", `[]`, "not valid JSON"},
		{"missing model", `{"max_tokens":5,` + msgs + `}`, "model: field required"},
		{"missing max_tokens", `{"model":"m",` + msgs + `}`, "max_tokens: field required"},
		{"zero max_tokens", `{"model":"m","max_tokens":0,` + msgs + `}`, "max_tokens: must be greater than 0"},
		{"negative max_tokens", `{"model":"m","max_tokens":-1,` + msgs + `}`, "max_tokens: must be greater than 0"},
		{"fractional max_tokens", `{"model":"m","max_tokens":1.5,` + msgs + `}`, "max_tokens: invalid type"},
		{"string temperature", `{"model":"m","max_tokens":5,"temperature":"hot",` + msgs + `}`, "temperature: invalid type"},
		{"no messages", `{"model":"m","max_tokens":5,"messages":[]}`, "at least one message"},
		{"missing messages", `{"model":"m","max_tokens":5}`, "at least one message"},
		{"bad role", `{"model":"m","max_tokens":5,"messages":[{"role":"system","content":"q"}]}`, `messages.0.role: must be "user" or "assistant"`},
		{"missing content", `{"model":"m","max_tokens":5,"messages":[{"role":"user"}]}`, "messages.0.content: field required"},
		{"numeric content", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":3}]}`, "messages.0.content: must be a string or an array"},
		{"image block", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`, "messages.0.content.0: only text content is supported"},
		{"tool_result block", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"q"},{"role":"user","content":[{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"t"}]}]}`, "messages.1.content.1: only text content is supported"},
		{"text block without text", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text"}]}]}`, "messages.0.content.0.text: field required"},
		{"image in system", `{"model":"m","max_tokens":5,"system":[{"type":"image"}],` + msgs + `}`, "system.0: only text content is supported"},
		{"numeric system", `{"model":"m","max_tokens":5,"system":1,` + msgs + `}`, "system: must be a string or an array"},
		{"tools", `{"model":"m","max_tokens":5,"tools":[{"name":"t","input_schema":{}}],` + msgs + `}`, "tool use is not supported yet"},
		{"tool_choice", `{"model":"m","max_tokens":5,"tool_choice":{"type":"auto"},` + msgs + `}`, "tool use is not supported yet"},
		{"thinking", `{"model":"m","max_tokens":5,"thinking":{"type":"enabled","budget_tokens":1024},` + msgs + `}`, "extended thinking is not supported yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &stubProvider{resp: &core.Response{}}
			w := post(t, &testGateway{p: p}, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body %s", w.Code, w.Body)
			}
			e := decodeError(t, w.Body.Bytes())
			if e.Type != "error" || e.Error.Type != "invalid_request_error" || !strings.Contains(e.Error.Message, tt.want) {
				t.Errorf("error %+v, want invalid_request_error containing %q", e, tt.want)
			}
			if p.last != nil {
				t.Error("provider was called for an invalid request")
			}
		})
	}
}

func decodeError(t *testing.T, body []byte) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return e
}

func TestBodyTooLarge(t *testing.T) {
	t.Parallel()
	body := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"` + strings.Repeat("a", MaxBodyBytes) + `"}]}`
	w := post(t, &testGateway{p: &stubProvider{}}, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", w.Code)
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Type != "request_too_large" {
		t.Errorf("type %q, want request_too_large", e.Error.Type)
	}
}

func TestAuth(t *testing.T) {
	t.Parallel()
	const body = `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"q"}]}`
	tests := []struct {
		name    string
		headers map[string]string
		status  int
		message string
	}{
		{"missing key", nil, http.StatusUnauthorized, "missing API key"},
		{"wrong key", map[string]string{"x-api-key": "nope"}, http.StatusUnauthorized, "invalid x-api-key"},
		{"x-api-key", map[string]string{"x-api-key": testKey}, http.StatusOK, ""},
		{"bearer fallback", map[string]string{"Authorization": "Bearer " + testKey}, http.StatusOK, ""},
		{"basic is not a key", map[string]string{"Authorization": "Basic " + testKey}, http.StatusUnauthorized, "missing API key"},
		{"no anthropic-version needed", map[string]string{"x-api-key": testKey, "anthropic-version": ""}, http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gw := &testGateway{p: &stubProvider{resp: &core.Response{ID: "x", Model: "m", Text: "ok"}}}
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			Messages(gw).ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status %d, want %d; body %s", w.Code, tt.status, w.Body)
			}
			if tt.status == http.StatusOK {
				return
			}
			e := decodeError(t, w.Body.Bytes())
			if e.Type != "error" || e.Error.Type != "authentication_error" || !strings.Contains(e.Error.Message, tt.message) {
				t.Errorf("error %+v", e)
			}

			r = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			w = httptest.NewRecorder()
			ListModels(gw).ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Errorf("models: status %d, want %d", w.Code, tt.status)
			}
		})
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind       core.ErrorKind
		status     int
		typ        string
		message    string
		retryAfter time.Duration
		header     string
	}{
		{core.KindInvalidRequest, 400, "invalid_request_error", "boom", 0, ""},
		{core.KindAuthentication, 401, "authentication_error", "boom", 0, ""},
		{core.KindPermission, 403, "permission_error", "boom", 0, ""},
		{core.KindNotFound, 404, "not_found_error", "boom", 0, ""},
		{core.KindRateLimited, 429, "rate_limit_error", "boom", 1500 * time.Millisecond, "2"},
		{core.KindOverloaded, 529, "overloaded_error", "boom", 30 * time.Second, "30"},
		{core.KindTimeout, 504, "timeout_error", "boom", 0, ""},
		{core.KindUpstream, 502, "api_error", "boom", 0, ""},
		{core.KindInternal, 500, "api_error", internalMessage, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.kind.String(), func(t *testing.T) {
			t.Parallel()
			cause := &core.Error{Kind: tt.kind, Message: "boom", RetryAfter: tt.retryAfter, Err: errSecret}
			for _, stream := range []bool{false, true} {
				p := &stubProvider{err: cause}
				body := fmt.Sprintf(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"q"}],"stream":%t}`, stream)
				w := post(t, &testGateway{p: p}, body)
				if w.Code != tt.status {
					t.Fatalf("stream=%v: status %d, want %d", stream, w.Code, tt.status)
				}
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("content type %q", ct)
				}
				e := decodeError(t, w.Body.Bytes())
				if e.Type != "error" || e.Error.Type != tt.typ || e.Error.Message != tt.message {
					t.Errorf("stream=%v: error %+v, want %s %q", stream, e, tt.typ, tt.message)
				}
				if got := w.Header().Get("Retry-After"); got != tt.header {
					t.Errorf("Retry-After %q, want %q", got, tt.header)
				}
				if strings.Contains(w.Body.String(), errSecret.Error()) {
					t.Error("cause leaked to the client")
				}
			}
		})
	}
}

var errSecret = &secretError{}

type secretError struct{}

func (*secretError) Error() string { return "secret cause text" }

func TestUnclassifiedErrorIsInternal(t *testing.T) {
	t.Parallel()
	w := post(t, &testGateway{p: &stubProvider{err: errSecret}}, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"q"}]}`)
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Errorf("status %d body %s", w.Code, w.Body)
	}
}

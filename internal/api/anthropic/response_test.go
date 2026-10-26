package anthropic

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

const streamBody = `{"model":"m","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"q"}]}`

func TestMessageResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		resp core.Response
		want string
	}{
		{
			name: "provider id gets prefix",
			resp: core.Response{ID: "fake-1", Model: "qwen2.5:0.5b", Text: "Red <&> Blue", StopReason: core.StopEndTurn, Usage: core.Usage{InputTokens: 1, OutputTokens: 6, CacheReadTokens: 23, CacheWriteTokens: 2}},
			want: `{"id":"msg_fake-1","type":"message","role":"assistant","model":"qwen2.5:0.5b","content":[{"type":"text","text":"Red <&> Blue"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":6,"cache_creation_input_tokens":2,"cache_read_input_tokens":23}}`,
		},
		{
			name: "msg id kept, max tokens",
			resp: core.Response{ID: "msg_abc", Model: "m", Text: "x", StopReason: core.StopMaxTokens},
			want: `{"id":"msg_abc","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
		},
		{
			name: "stop sequence",
			resp: core.Response{ID: "msg_s", Model: "m", StopReason: core.StopSequence},
			want: `{"id":"msg_s","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":""}],"stop_reason":"stop_sequence","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
		},
		{
			name: "content filter is a refusal",
			resp: core.Response{ID: "msg_r", Model: "m", StopReason: core.StopContentFilter},
			want: `{"id":"msg_r","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":""}],"stop_reason":"refusal","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp := tt.resp
			w := post(t, &testGateway{p: &stubProvider{resp: &resp}}, `{"model":"alias","max_tokens":5,"messages":[{"role":"user","content":"q"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if got := w.Body.String(); got != tt.want {
				t.Errorf("body\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestGeneratedID(t *testing.T) {
	t.Parallel()
	id := messageID("")
	if !strings.HasPrefix(id, "msg_") || len(id) != 28 || id == messageID("") {
		t.Errorf("generated id %q", id)
	}
}

func TestStreamEvents(t *testing.T) {
	t.Parallel()
	events := []core.Event{
		{Kind: core.EventStart, ID: "abc", Model: "qwen2.5:0.5b", Usage: core.Usage{InputTokens: 12, CacheReadTokens: 3, CacheWriteTokens: 4}},
		{Kind: core.EventDelta, Text: "Red"},
		{Kind: core.EventDelta, Text: ""},
		{Kind: core.EventDelta, Text: ",\n\"Blue\""},
		{Kind: core.EventStop, StopReason: core.StopMaxTokens, Usage: core.Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 2}},
	}
	p := &stubProvider{newStream: func(context.Context) core.Stream { return &core.SliceStream{Events: events} }}
	w := post(t, &testGateway{p: p}, streamBody)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}
	want := `event: message_start
data: {"type":"message_start","message":{"id":"msg_abc","type":"message","role":"assistant","model":"qwen2.5:0.5b","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":0,"cache_creation_input_tokens":4,"cache_read_input_tokens":3}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Red"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":",\n\"Blue\""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"input_tokens":13,"cache_creation_input_tokens":0,"cache_read_input_tokens":24,"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	if got := w.Body.String(); got != want {
		t.Errorf("stream\n got %s\nwant %s", got, want)
	}
}

func TestStreamUsageOmittedWhenUnknown(t *testing.T) {
	t.Parallel()
	events := []core.Event{
		{Kind: core.EventStart, ID: "a", Model: "m"},
		{Kind: core.EventStop, StopReason: core.StopContentFilter, Usage: core.Usage{OutputTokens: 1}},
	}
	p := &stubProvider{newStream: func(context.Context) core.Stream { return &core.SliceStream{Events: events} }}
	w := post(t, &testGateway{p: p}, streamBody)
	want := `data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":1}}`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("stream %s\nwant line %s", w.Body, want)
	}
}

func TestStreamErrorBeforeFirstEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		p      *stubProvider
		status int
		typ    string
	}{
		{
			name:   "stream call fails",
			p:      &stubProvider{err: core.Errorf(core.KindRateLimited, "slow down")},
			status: 429, typ: "rate_limit_error",
		},
		{
			name: "first event fails",
			p: &stubProvider{newStream: func(context.Context) core.Stream {
				return &errStream{err: core.Errorf(core.KindOverloaded, "busy")}
			}},
			status: 529, typ: "overloaded_error",
		},
		{
			name:   "empty stream",
			p:      &stubProvider{newStream: func(context.Context) core.Stream { return &core.SliceStream{} }},
			status: 502, typ: "api_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := post(t, &testGateway{p: tt.p}, streamBody)
			if w.Code != tt.status {
				t.Fatalf("status %d, want %d", w.Code, tt.status)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content type %q", ct)
			}
			if e := decodeError(t, w.Body.Bytes()); e.Error.Type != tt.typ {
				t.Errorf("type %q, want %q", e.Error.Type, tt.typ)
			}
		})
	}
}

func TestStreamMidStreamError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"overloaded", core.Errorf(core.KindOverloaded, "upstream: Overloaded"), `{"type":"error","error":{"type":"overloaded_error","message":"upstream: Overloaded"}}`},
		{"internal", errSecret, `{"type":"error","error":{"type":"api_error","message":"internal server error"}}`},
		{"ended early", nil, `{"type":"error","error":{"type":"api_error","message":"upstream stream ended early"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			evs := []core.Event{{Kind: core.EventStart, ID: "msg_1", Model: "m"}, {Kind: core.EventDelta, Text: "partial"}}
			p := &stubProvider{newStream: func(context.Context) core.Stream {
				return &errStream{SliceStream: core.SliceStream{Events: evs}, err: tt.err}
			}}
			w := post(t, &testGateway{p: p}, streamBody)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			body := w.Body.String()
			wantTail := "event: error\ndata: " + tt.want + "\n\n"
			if !strings.HasSuffix(body, wantTail) {
				t.Errorf("stream does not end with the error event:\n%s", body)
			}
			if strings.Contains(body, "message_stop") || !strings.Contains(body, `"text":"partial"`) {
				t.Errorf("unexpected stream:\n%s", body)
			}
		})
	}
}

// blockingStream sends a start event, then blocks until its context ends.
type blockingStream struct {
	ctx    context.Context
	sent   bool
	closed chan struct{}
}

func (s *blockingStream) Next() (core.Event, error) {
	if !s.sent {
		s.sent = true
		return core.Event{Kind: core.EventStart, ID: "msg_b", Model: "m"}, nil
	}
	<-s.ctx.Done()
	return core.Event{}, core.Wrap(core.KindTimeout, s.ctx.Err(), "request cancelled")
}

func (s *blockingStream) Close() error {
	close(s.closed)
	return nil
}

func TestStreamClientGoneClosesStream(t *testing.T) {
	t.Parallel()
	closed := make(chan struct{})
	p := &stubProvider{newStream: func(ctx context.Context) core.Stream {
		return &blockingStream{ctx: ctx, closed: closed}
	}}
	srv := httptest.NewServer(Messages(&testGateway{p: p}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(streamBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "event: message_start\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("provider stream was not closed after the client went away")
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		models []string
		want   string
	}{
		{"two", []string{"claude", "qwen"}, `{"data":[{"type":"model","id":"claude","display_name":"claude","created_at":"1970-01-01T00:00:00Z"},{"type":"model","id":"qwen","display_name":"qwen","created_at":"1970-01-01T00:00:00Z"}],"has_more":false,"first_id":"claude","last_id":"qwen"}`},
		{"none", nil, `{"data":[],"has_more":false,"first_id":null,"last_id":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("x-api-key", testKey)
			w := httptest.NewRecorder()
			ListModels(&testGateway{models: tt.models}).ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != tt.want {
				t.Errorf("status %d body\n got %s\nwant %s", w.Code, w.Body, tt.want)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	w := httptest.NewRecorder()
	Messages(&testGateway{}).ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Errorf("status %d allow %q", w.Code, w.Header().Get("Allow"))
	}
}

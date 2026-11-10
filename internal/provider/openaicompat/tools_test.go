package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// toolRequest is the request the tool fixtures were recorded with.
func toolRequest(stream bool) *core.Request {
	return &core.Request{
		Model:    "qwen2.5:0.5b",
		Stream:   stream,
		Messages: []core.Message{{Role: core.RoleUser, Text: "What is the weather in Paris?"}},
		Tools: []core.Tool{{
			Name: "get_weather", Description: "Get the current weather for a city.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"City name"}},"required":["city"]}`),
		}},
	}
}

func TestFixtureToolCall(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-chat-tools.json").URL, 0)
	got, err := p.Complete(context.Background(), toolRequest(false))
	if err != nil {
		t.Fatal(err)
	}
	want := &core.Response{
		ID:         "chatcmpl-269",
		Model:      "qwen2.5:0.5b",
		ToolCalls:  []core.ToolCall{{ID: "call_crjksjoj", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}},
		StopReason: core.StopToolUse,
		Usage:      core.Usage{InputTokens: 1, CacheReadTokens: 157, OutputTokens: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestFixtureToolCallStream(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-chat-tools-stream.sse").URL, 0)
	s, err := p.Stream(context.Background(), toolRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Event{
		{Kind: core.EventStart, ID: "chatcmpl-580", Model: "qwen2.5:0.5b"},
		{Kind: core.EventToolCallStart, ToolIndex: 0, ToolCallID: "call_j5v3c55r", ToolName: "get_weather"},
		{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: `{"city":"Paris"}`},
		{Kind: core.EventStop, StopReason: core.StopToolUse, Usage: core.Usage{InputTokens: 1, CacheReadTokens: 157, OutputTokens: 20}},
	}
	if got := drain(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// The follow-up turn carrying the tool's result was recorded too: the
// upstream answers it in text.
func TestFixtureToolResult(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-chat-tool-result.json").URL, 0)
	req := toolRequest(false)
	req.Messages = append(req.Messages,
		core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}}},
		core.Message{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "call_1", Content: "18 C and sunny"}}})
	got, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != core.StopEndTurn || len(got.ToolCalls) != 0 || !strings.Contains(got.Text, "18°C") {
		t.Errorf("got %+v", got)
	}
}

// The request body for a tool conversation is the one the fixtures were
// recorded with: tools as functions, the assistant's call with empty
// content, the result as a "tool" message.
func TestToolRequestBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(r *core.Request)
		want string
	}{
		{
			name: "conversation",
			edit: func(r *core.Request) {
				r.Messages = append(r.Messages,
					core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}, {ID: "call_2", Name: "now"}}},
					core.Message{Role: core.RoleUser, Text: "and?", ToolResults: []core.ToolResult{{CallID: "call_1", Content: "18 C and sunny"}, {CallID: "call_2", Content: "noon", IsError: true}}})
			},
			want: `{"messages":[{"content":"What is the weather in Paris?","role":"user"},` +
				`{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"{\"city\":\"Paris\"}","name":"get_weather"},"id":"call_1","type":"function"},{"function":{"arguments":"{}","name":"now"},"id":"call_2","type":"function"}]},` +
				`{"content":"18 C and sunny","role":"tool","tool_call_id":"call_1"},{"content":"noon","role":"tool","tool_call_id":"call_2"},{"content":"and?","role":"user"}],` +
				`"model":"qwen2.5:0.5b","tools":[{"function":{"description":"Get the current weather for a city.","name":"get_weather","parameters":{"properties":{"city":{"description":"City name","type":"string"}},"required":["city"],"type":"object"}},"type":"function"}]}`,
		},
		{
			name: "required, parallel off, no schema",
			edit: func(r *core.Request) {
				r.Tools = []core.Tool{{Name: "f"}}
				r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAny, DisableParallel: true}
			},
			want: `{"messages":[{"content":"What is the weather in Paris?","role":"user"}],"model":"qwen2.5:0.5b","parallel_tool_calls":false,"tool_choice":"required","tools":[{"function":{"name":"f"},"type":"function"}]}`,
		},
		{
			name: "named function",
			edit: func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceTool, Name: "get_weather"} },
			want: `"tool_choice":{"function":{"name":"get_weather"},"type":"function"}`,
		},
		{"auto", func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto} }, `"tool_choice":"auto"`},
		{"none", func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceNone} }, `"tool_choice":"none"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := make(chan map[string]any, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var m map[string]any
				_ = json.NewDecoder(r.Body).Decode(&m)
				got <- m
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"x","model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()
			p := newProvider(t, srv.URL, 0)
			req := toolRequest(false)
			tt.edit(req)
			if _, err := p.Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(<-got)
			if !strings.Contains(string(b), tt.want) {
				t.Errorf("body %s\nwant %s", b, tt.want)
			}
		})
	}
}

func TestStopReasonTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		finish string
		calls  bool
		want   core.StopReason
	}{
		{"stop", false, core.StopEndTurn},
		{"stop", true, core.StopToolUse},
		{"length", false, core.StopMaxTokens},
		{"length", true, core.StopMaxTokens},
		{"tool_calls", true, core.StopToolUse},
		{"function_call", true, core.StopToolUse},
		{"content_filter", false, core.StopContentFilter},
		{"something_new", false, core.StopEndTurn},
	}
	for _, tt := range tests {
		if got := stopReason(tt.finish, tt.calls); got != tt.want {
			t.Errorf("stopReason(%q, %v) = %q, want %q", tt.finish, tt.calls, got, tt.want)
		}
	}
}

// Tool call deltas in the shapes servers send them: OpenAI streams a call's
// arguments over many chunks after one carrying id and name; Ollama sends
// whole calls; some servers leave out the index.
func TestStreamToolCallShapes(t *testing.T) {
	t.Parallel()
	start := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}` + "\n\n"
	chunk := func(calls string) string {
		return `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":` + calls + `},"finish_reason":null}]}` + "\n\n"
	}
	finish := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	head := core.Event{Kind: core.EventStart, ID: "c1", Model: "m"}
	stop := core.Event{Kind: core.EventStop, StopReason: core.StopToolUse}
	tests := []struct {
		name    string
		body    string
		want    []core.Event
		wantErr bool
	}{
		{
			name: "openai pieces",
			body: start +
				chunk(`[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":""}}]`) +
				chunk(`[{"index":0,"function":{"arguments":"{\"x\""}}]`) +
				chunk(`[{"index":0,"function":{"arguments":":1}"}}]`) +
				chunk(`[{"index":1,"id":"call_b","type":"function","function":{"name":"g","arguments":"{}"}}]`) + finish,
			want: []core.Event{head,
				{Kind: core.EventToolCallStart, ToolIndex: 0, ToolCallID: "call_a", ToolName: "f"},
				{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: `{"x"`},
				{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: `:1}`},
				{Kind: core.EventToolCallStart, ToolIndex: 1, ToolCallID: "call_b", ToolName: "g"},
				{Kind: core.EventToolCallDelta, ToolIndex: 1, Arguments: `{}`},
				stop},
		},
		{
			name: "two whole calls in one chunk",
			body: start + chunk(`[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"g","arguments":"{}"}}]`) + finish,
			want: []core.Event{head,
				{Kind: core.EventToolCallStart, ToolIndex: 0, ToolCallID: "a", ToolName: "f"},
				{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: `{}`},
				{Kind: core.EventToolCallStart, ToolIndex: 1, ToolCallID: "b", ToolName: "g"},
				{Kind: core.EventToolCallDelta, ToolIndex: 1, Arguments: `{}`},
				stop},
		},
		{
			name: "no index",
			body: start + chunk(`[{"id":"a","function":{"name":"f","arguments":"{\"y\""}}]`) + chunk(`[{"function":{"arguments":":2}"}}]`) + chunk(`[{"id":"b","function":{"name":"g"}}]`) + finish,
			want: []core.Event{head,
				{Kind: core.EventToolCallStart, ToolIndex: 0, ToolCallID: "a", ToolName: "f"},
				{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: `{"y"`},
				{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: `:2}`},
				{Kind: core.EventToolCallStart, ToolIndex: 1, ToolCallID: "b", ToolName: "g"},
				stop},
		},
		{
			name:    "interleaved calls",
			body:    start + chunk(`[{"index":0,"id":"a","function":{"name":"f"}}]`) + chunk(`[{"index":1,"id":"b","function":{"name":"g"}}]`) + chunk(`[{"index":0,"function":{"arguments":"{}"}}]`) + finish,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			s, err := newProvider(t, srv.URL, 0).Stream(context.Background(), toolRequest(true))
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantErr {
				_, err := core.Collect(s)
				if core.AsError(err).Kind != core.KindUpstream {
					t.Fatalf("err = %v, want an upstream error", err)
				}
				return
			}
			if got := drain(t, s); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

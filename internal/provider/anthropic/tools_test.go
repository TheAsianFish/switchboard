package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// toolRequest is the request the tool fixtures were recorded with.
func toolRequest() *core.Request {
	return &core.Request{
		Model: "qwen2.5:0.5b", MaxTokens: 128,
		Messages: []core.Message{{Role: core.RoleUser, Text: "What is the weather in Paris?"}},
		Tools: []core.Tool{{
			Name: "get_weather", Description: "Get the current weather for a city.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"City name"}},"required":["city"]}`),
		}},
	}
}

func TestFixtureToolCall(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-messages-tools.json"), 0)
	got, err := p.Complete(context.Background(), toolRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := &core.Response{
		ID: "msg_62dd60a883a9554b8e42f8c6", Model: "qwen2.5:0.5b", StopReason: core.StopToolUse,
		ToolCalls: []core.ToolCall{{ID: "call_5bt8qdtz", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}},
		Usage:     core.Usage{InputTokens: 1, CacheReadTokens: 157, OutputTokens: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestFixtureToolCallStream(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-messages-tools-stream.sse"), 0)
	s, err := p.Stream(context.Background(), toolRequest())
	if err != nil {
		t.Fatal(err)
	}
	got, err := collect(t, s)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Event{
		{Kind: core.EventStart, ID: "msg_eda588b54f908e1c2981800b", Model: "qwen2.5:0.5b", Usage: core.Usage{InputTokens: 45}},
		{Kind: core.EventToolCallStart, ToolIndex: 0, ToolCallID: "call_u40lsqpy", ToolName: "get_weather"},
		{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: `{"city":"Paris"}`},
		{Kind: core.EventStop, StopReason: core.StopToolUse, Usage: core.Usage{InputTokens: 1, CacheReadTokens: 157, OutputTokens: 20}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events\n got %+v\nwant %+v", got, want)
	}
}

// The follow-up turn carrying the tool's result was recorded too: the
// upstream answers it in text.
func TestFixtureToolResult(t *testing.T) {
	t.Parallel()
	p := newProvider(t, fixtureServer(t, "ollama-messages-tool-result.json"), 0)
	req := toolRequest()
	req.Messages = append(req.Messages,
		core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "toolu_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}}},
		core.Message{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "toolu_1", Content: "18 C and sunny"}}})
	got, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != core.StopEndTurn || len(got.ToolCalls) != 0 || !strings.Contains(got.Text, "18°C") {
		t.Errorf("got %+v", got)
	}
}

// The request body for a tool conversation matches the recorded one, and
// tool choices map to the API's shapes.
func TestToolRequestBody(t *testing.T) {
	t.Parallel()
	conversation := func(r *core.Request) {
		r.Messages = append(r.Messages,
			core.Message{Role: core.RoleAssistant, Text: "Checking.", ToolCalls: []core.ToolCall{{ID: "toolu_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}, {ID: "toolu_2", Name: "now"}}},
			core.Message{Role: core.RoleUser, Text: "thanks", ToolResults: []core.ToolResult{{CallID: "toolu_1", Content: "18 C and sunny"}, {CallID: "toolu_2", IsError: true}}})
	}
	tests := []struct {
		name string
		edit func(r *core.Request)
		want []string // substrings of the body
		not  []string
	}{
		{
			name: "conversation",
			edit: conversation,
			want: []string{
				`"tools":[{"input_schema":{"type":"object","properties":{"city":{"type":"string","description":"City name"}},"required":["city"]},"name":"get_weather","description":"Get the current weather for a city."}]`,
				`{"content":[{"text":"Checking.","type":"text"},{"id":"toolu_1","input":{"city":"Paris"},"name":"get_weather","type":"tool_use"},{"id":"toolu_2","input":{},"name":"now","type":"tool_use"}],"role":"assistant"}`,
				`{"content":[{"tool_use_id":"toolu_1","content":[{"text":"18 C and sunny","type":"text"}],"type":"tool_result"},{"tool_use_id":"toolu_2","is_error":true,"type":"tool_result"},{"text":"thanks","type":"text"}],"role":"user"}`,
			},
			not: []string{"tool_choice"},
		},
		{"auto", func(r *core.Request) { r.ToolChoice.Mode = core.ToolChoiceAuto }, []string{`"tool_choice":{"type":"auto"}`}, nil},
		{"any, one call", func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAny, DisableParallel: true} }, []string{`"tool_choice":{"disable_parallel_tool_use":true,"type":"any"}`}, nil},
		{"named tool", func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceTool, Name: "get_weather"} }, []string{`"tool_choice":{"name":"get_weather","type":"tool"}`}, nil},
		{"none", func(r *core.Request) { r.ToolChoice.Mode = core.ToolChoiceNone }, []string{`"tool_choice":{"type":"none"}`}, nil},
		{"one call, no mode", func(r *core.Request) { r.ToolChoice.DisableParallel = true }, []string{`"tool_choice":{"disable_parallel_tool_use":true,"type":"auto"}`}, nil},
		{"tool without schema", func(r *core.Request) { r.Tools = []core.Tool{{Name: "f"}} }, []string{`"tools":[{"input_schema":{"type":"object"},"name":"f"}]`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := make(chan string, 1)
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				body <- string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			})
			p := newProvider(t, srv, 0)
			req := toolRequest()
			tt.edit(req)
			if _, err := p.Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			got := <-body
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("body %s\nlacks %s", got, w)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("body %s\nhas %s", got, n)
				}
			}
		})
	}
}

// A tool_use block that arrives with its input up front, rather than as
// input_json_delta events, keeps it.
func TestStreamToolInputUpFront(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"f\",\"input\":{\"a\":1}}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	s, err := newProvider(t, srv, 0).Stream(context.Background(), toolRequest())
	if err != nil {
		t.Fatal(err)
	}
	got, err := core.Collect(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ToolCall{{ID: "t", Name: "f", Arguments: json.RawMessage(`{"a":1}`)}}
	if !reflect.DeepEqual(got.ToolCalls, want) || got.StopReason != core.StopToolUse {
		t.Errorf("got %+v", got)
	}
}

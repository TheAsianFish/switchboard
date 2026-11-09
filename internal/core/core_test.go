package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestCollect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		events  []Event
		want    Response
		wantErr bool
	}{
		{
			name: "input usage from start when stop omits it",
			events: []Event{
				{Kind: EventStart, ID: "a", Model: "m", Usage: Usage{InputTokens: 5, CacheReadTokens: 2}},
				{Kind: EventDelta, Text: "he"}, {Kind: EventDelta, Text: "llo"},
				{Kind: EventStop, StopReason: StopMaxTokens, Usage: Usage{OutputTokens: 2}},
			},
			want: Response{ID: "a", Model: "m", Text: "hello", StopReason: StopMaxTokens, Usage: Usage{InputTokens: 5, OutputTokens: 2, CacheReadTokens: 2}},
		},
		{
			name: "stop usage wins when present",
			events: []Event{
				{Kind: EventStart, Usage: Usage{InputTokens: 12}},
				{Kind: EventStop, StopReason: StopEndTurn, Usage: Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
			},
			want: Response{StopReason: StopEndTurn, Usage: Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
		},
		{name: "no stop event", events: []Event{{Kind: EventStart}, {Kind: EventDelta, Text: "x"}}, wantErr: true},
		{
			name: "text then tool calls",
			events: []Event{
				{Kind: EventStart, ID: "a"},
				{Kind: EventDelta, Text: "let me look"},
				{Kind: EventToolCallStart, ToolIndex: 0, ToolCallID: "c0", ToolName: "f"},
				{Kind: EventToolCallDelta, ToolIndex: 0, Arguments: `{"q":`},
				{Kind: EventToolCallDelta, ToolIndex: 0, Arguments: `"x"}`},
				{Kind: EventToolCallStart, ToolIndex: 1, ToolCallID: "c1", ToolName: "g"},
				{Kind: EventStop, StopReason: StopToolUse, Usage: Usage{InputTokens: 1, OutputTokens: 9}},
			},
			want: Response{
				ID: "a", Text: "let me look", StopReason: StopToolUse, Usage: Usage{InputTokens: 1, OutputTokens: 9},
				ToolCalls: []ToolCall{{ID: "c0", Name: "f", Arguments: json.RawMessage(`{"q":"x"}`)}, {ID: "c1", Name: "g"}},
			},
		},
		{
			name: "tool call index skipped",
			events: []Event{
				{Kind: EventStart},
				{Kind: EventToolCallStart, ToolIndex: 1, ToolCallID: "c", ToolName: "f"},
				{Kind: EventStop, StopReason: StopToolUse},
			},
			wantErr: true,
		},
		{
			name: "arguments for a closed call",
			events: []Event{
				{Kind: EventStart},
				{Kind: EventToolCallStart, ToolIndex: 0, ToolCallID: "a", ToolName: "f"},
				{Kind: EventToolCallStart, ToolIndex: 1, ToolCallID: "b", ToolName: "f"},
				{Kind: EventToolCallDelta, ToolIndex: 0, Arguments: "{}"},
				{Kind: EventStop, StopReason: StopToolUse},
			},
			wantErr: true,
		},
		{
			name:    "arguments before any call",
			events:  []Event{{Kind: EventStart}, {Kind: EventToolCallDelta, Arguments: "{}"}, {Kind: EventStop}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Collect(&SliceStream{Events: tt.events})
			if tt.wantErr {
				if AsError(err).Kind != KindUpstream {
					t.Fatalf("err = %v, want an upstream error", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(*got, tt.want) {
				t.Fatalf("Collect = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestResponseEventsRoundTrip(t *testing.T) {
	t.Parallel()
	r := &Response{ID: "x", Model: "m", Text: "abc", StopReason: StopEndTurn, Usage: Usage{InputTokens: 3, OutputTokens: 2, CacheWriteTokens: 1}}
	got, err := Collect(&SliceStream{Events: ResponseEvents(r, []string{"a", "bc"})})
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("got %+v, %v", got, err)
	}

	tools := &Response{
		ID: "y", Model: "m", Text: "ok", StopReason: StopToolUse, Usage: Usage{InputTokens: 3, OutputTokens: 4},
		ToolCalls: []ToolCall{{ID: "a", Name: "f", Arguments: json.RawMessage(`{"x":1}`)}, {ID: "b", Name: "g"}},
	}
	got, err = Collect(&SliceStream{Events: ResponseEvents(tools, []string{"o", "k"})})
	if err != nil || !reflect.DeepEqual(got, tools) {
		t.Fatalf("got %+v, %v; want %+v", got, err, tools)
	}
}

func TestHasToolContent(t *testing.T) {
	t.Parallel()
	user := Message{Role: RoleUser, Text: "q"}
	tests := []struct {
		name string
		req  Request
		want bool
	}{
		{"text only", Request{Messages: []Message{user}}, false},
		{"tools", Request{Messages: []Message{user}, Tools: []Tool{{Name: "f"}}}, true},
		{"tool choice", Request{Messages: []Message{user}, ToolChoice: ToolChoice{Mode: ToolChoiceNone}}, true},
		{"tool call in history", Request{Messages: []Message{user, {Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "a"}}}, user}}, true},
		{"tool result", Request{Messages: []Message{{Role: RoleUser, ToolResults: []ToolResult{{CallID: "a"}}}}}, true},
	}
	for _, tt := range tests {
		if got := tt.req.HasToolContent(); got != tt.want {
			t.Errorf("%s: HasToolContent = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAsError(t *testing.T) {
	t.Parallel()
	cause := errors.New("secret detail")
	e := AsError(fmt.Errorf("wrapped: %w", Wrap(KindRateLimited, cause, "slow down")))
	if e.Kind != KindRateLimited || e.Message != "slow down" || !errors.Is(e, cause) {
		t.Errorf("classified error = %+v", e)
	}
	plain := AsError(cause)
	if plain.Kind != KindInternal || plain.Message != "internal error" {
		t.Errorf("unclassified error = %+v, want an internal error that hides its cause", plain)
	}
	for status, want := range map[int]ErrorKind{400: KindInvalidRequest, 401: KindAuthentication, 404: KindNotFound, 429: KindRateLimited, 500: KindUpstream, 529: KindOverloaded, 504: KindTimeout} {
		if got := KindFromStatus(status); got != want {
			t.Errorf("KindFromStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

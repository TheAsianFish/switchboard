package fake

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

func request(text string, maxTokens int) *core.Request {
	return &core.Request{Model: "fake", MaxTokens: maxTokens, Messages: []core.Message{{Role: core.RoleUser, Text: text}}}
}

func TestStreamMatchesComplete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		maxTokens int
		wantOut   int
		wantStop  core.StopReason
	}{
		{"default length", 0, 30, core.StopEndTurn},
		{"truncated by max_tokens", 7, 7, core.StopMaxTokens},
		{"max_tokens above length", 100, 30, core.StopEndTurn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := New(Config{Tokens: 30})
			req := request("hello there world", tt.maxTokens)
			full, err := p.Complete(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			s, err := p.Stream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			streamed, err := core.Collect(s)
			if err != nil {
				t.Fatal(err)
			}
			if streamed.Text != full.Text {
				t.Errorf("streamed %q, complete %q", streamed.Text, full.Text)
			}
			if full.Usage != (core.Usage{InputTokens: 3, OutputTokens: tt.wantOut}) || streamed.Usage != full.Usage {
				t.Errorf("usage complete %+v streamed %+v", full.Usage, streamed.Usage)
			}
			if full.StopReason != tt.wantStop || streamed.StopReason != tt.wantStop {
				t.Errorf("stop %q/%q, want %q", full.StopReason, streamed.StopReason, tt.wantStop)
			}
			if p.Calls() != 2 {
				t.Errorf("calls = %d", p.Calls())
			}
		})
	}
}

func TestDeterministic(t *testing.T) {
	t.Parallel()
	a, _ := New(Config{Tokens: 10}).Complete(context.Background(), request("same", 0))
	b, _ := New(Config{Tokens: 10}).Complete(context.Background(), request("same", 0))
	c, _ := New(Config{Tokens: 10}).Complete(context.Background(), request("different", 0))
	if a.Text != b.Text {
		t.Errorf("same prompt gave %q and %q", a.Text, b.Text)
	}
	if a.Text == c.Text {
		t.Errorf("different prompts gave the same reply %q", a.Text)
	}
}

func toolRequest(mutate func(*core.Request)) *core.Request {
	r := request("weather in Paris?", 0)
	r.Tools = []core.Tool{
		{Name: "weather", Parameters: json.RawMessage(`{"type":"object","properties":{"day":{"type":"string"},"city":{"type":"string"}}}`)},
		{Name: "time"},
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

func TestToolCall(t *testing.T) {
	t.Parallel()
	withResult := func(r *core.Request) {
		r.Messages = append(r.Messages,
			core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "c", Name: "weather"}}},
			core.Message{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "c", Content: "rain all day"}}})
	}
	tests := []struct {
		name     string
		req      *core.Request
		wantTool string // "" for a text reply
		wantArgs []string
	}{
		{"first tool, one word per property", toolRequest(nil), "weather", []string{"city", "day"}},
		{"named tool choice", toolRequest(func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceTool, Name: "time"} }), "time", nil},
		{"tool choice none", toolRequest(func(r *core.Request) { r.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceNone} }), "", nil},
		{"after the tool result", toolRequest(withResult), "", nil},
		{"no tools", request("weather in Paris?", 0), "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := New(Config{Tokens: 4})
			full, err := p.Complete(context.Background(), tt.req)
			if err != nil {
				t.Fatal(err)
			}
			s, err := p.Stream(context.Background(), tt.req)
			if err != nil {
				t.Fatal(err)
			}
			streamed, err := core.Collect(s)
			if err != nil {
				t.Fatal(err)
			}
			streamed.ID = full.ID
			if !reflect.DeepEqual(full, streamed) {
				t.Errorf("complete %+v\nstreamed %+v", full, streamed)
			}
			again, _ := New(Config{Tokens: 4}).Complete(context.Background(), tt.req)
			again.ID = full.ID
			if !reflect.DeepEqual(full, again) {
				t.Errorf("not deterministic: %+v and %+v", full, again)
			}
			if tt.wantTool == "" {
				if len(full.ToolCalls) != 0 || full.Text == "" || full.StopReason != core.StopEndTurn {
					t.Errorf("want a text reply, got %+v", full)
				}
				return
			}
			if len(full.ToolCalls) != 1 || full.Text != "" || full.StopReason != core.StopToolUse || full.Usage.OutputTokens != callTokens {
				t.Fatalf("want one tool call, got %+v", full)
			}
			c := full.ToolCalls[0]
			var args map[string]string
			if err := json.Unmarshal(c.Arguments, &args); err != nil {
				t.Fatalf("arguments %s: %v", c.Arguments, err)
			}
			if c.Name != tt.wantTool || !strings.HasPrefix(c.ID, "call_") || len(args) != len(tt.wantArgs) {
				t.Errorf("call %+v", c)
			}
			for _, k := range tt.wantArgs {
				if args[k] == "" {
					t.Errorf("argument %q missing from %s", k, c.Arguments)
				}
			}
		})
	}
}

// The text reply is pinned: benchmarks and other packages' tests compare
// against it, and tool support must not change it.
func TestTextReplyPinned(t *testing.T) {
	t.Parallel()
	got, err := New(Config{Tokens: 5}).Complete(context.Background(), request("hello there world", 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := "kilo november quebec tango whiskey"; got.Text != want {
		t.Errorf("text %q, want %q", got.Text, want)
	}
}

func TestCancelledDuringLatency(t *testing.T) {
	t.Parallel()
	p := New(Config{Latency: time.Hour, Tokens: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Complete(ctx, request("x", 0))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	s, _ := p.Stream(ctx, request("x", 0))
	if _, err := s.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream err = %v, want context.Canceled", err)
	}
}

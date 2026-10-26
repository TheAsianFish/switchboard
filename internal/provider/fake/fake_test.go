package fake

import (
	"context"
	"errors"
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

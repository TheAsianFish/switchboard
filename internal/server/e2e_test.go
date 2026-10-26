package server_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkanthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/provider/anthropic"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/provider/openaicompat"
	"github.com/TheAsianFish/switchboard/internal/server"
)

const (
	gatewayKey = "sk-e2e"
	prompt     = "Name three primary colours."
	maxTokens  = 64
)

var fakeShape = fake.Config{Tokens: 9, TokenInterval: time.Millisecond}

// stack is the whole chain in one process: an unmodified SDK talks to the
// gateway's front end, the gateway talks over HTTP to a separate fake
// provider server through one of its two providers, and the fake answers in
// that provider's wire format. Each route crosses a different translation.
type stack struct {
	gateway string
}

func newStack(t *testing.T) stack {
	t.Helper()
	discard := slog.New(slog.DiscardHandler)
	upstream := httptest.NewServer(server.Handler(pipeline.Open{Provider: fake.New(fakeShape), Names: []string{"fake"}}, discard))
	t.Cleanup(upstream.Close)

	viaOpenAI, err := openaicompat.New(openaicompat.Config{BaseURL: upstream.URL + "/v1", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	viaMessages, err := anthropic.New(anthropic.Config{BaseURL: upstream.URL, APIKey: "upstream-key", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	gw := pipeline.New(auth.NewKeys([]config.Key{{Name: "e2e", Key: gatewayKey}}), []pipeline.Route{
		{Model: "via-chat-completions", ProviderName: "fake-openai", Upstream: "fake", Provider: viaOpenAI},
		{Model: "via-messages", ProviderName: "fake-anthropic", Upstream: "fake", Provider: viaMessages},
	})
	front := httptest.NewServer(server.Handler(gw, discard))
	t.Cleanup(front.Close)
	return stack{gateway: front.URL}
}

// want is what the fake replies to the test prompt.
func want(t *testing.T) *core.Response {
	t.Helper()
	resp, err := fake.New(fakeShape).Complete(context.Background(), &core.Request{
		Model:     "fake",
		MaxTokens: maxTokens,
		Messages:  []core.Message{{Role: core.RoleUser, Text: prompt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

var routes = []string{"via-chat-completions", "via-messages"}

func TestOpenAISDKThroughGateway(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	exp := want(t)
	client := openai.NewClient(openaioption.WithBaseURL(s.gateway+"/v1/"), openaioption.WithAPIKey(gatewayKey), openaioption.WithMaxRetries(0))
	for _, model := range routes {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			params := openai.ChatCompletionNewParams{
				Model:     model,
				MaxTokens: openai.Int(maxTokens),
				Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
			}
			full, err := client.Chat.Completions.New(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			if got := full.Choices[0].Message.Content; got != exp.Text {
				t.Errorf("complete text = %q, want %q", got, exp.Text)
			}
			if full.Model != model || full.Choices[0].FinishReason != "stop" {
				t.Errorf("model %q finish %q", full.Model, full.Choices[0].FinishReason)
			}
			if full.Usage.CompletionTokens != int64(exp.Usage.OutputTokens) || full.Usage.PromptTokens != int64(exp.Usage.InputTokens) {
				t.Errorf("usage = %+v, want %+v", full.Usage, exp.Usage)
			}

			params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
			stream := client.Chat.Completions.NewStreaming(ctx, params)
			var acc openai.ChatCompletionAccumulator
			chunks := 0
			for stream.Next() {
				chunk := stream.Current()
				acc.AddChunk(chunk)
				if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
					chunks++
				}
			}
			if err := stream.Err(); err != nil {
				t.Fatal(err)
			}
			if got := acc.Choices[0].Message.Content; got != exp.Text {
				t.Errorf("streamed text = %q, want %q", got, exp.Text)
			}
			if chunks != fakeShape.Tokens {
				t.Errorf("content chunks = %d, want one per token (%d)", chunks, fakeShape.Tokens)
			}
			if acc.Usage.CompletionTokens != int64(exp.Usage.OutputTokens) {
				t.Errorf("streamed usage = %+v", acc.Usage)
			}
		})
	}

	t.Run("bad key", func(t *testing.T) {
		t.Parallel()
		bad := openai.NewClient(openaioption.WithBaseURL(s.gateway+"/v1/"), openaioption.WithAPIKey("sk-wrong"), openaioption.WithMaxRetries(0))
		_, err := bad.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
			Model: routes[0], Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
		})
		var apiErr *openai.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("err = %v, want a 401 from the SDK", err)
		}
	})
}

func TestAnthropicSDKThroughGateway(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	exp := want(t)
	client := sdkanthropic.NewClient(anthropicoption.WithBaseURL(s.gateway), anthropicoption.WithAPIKey(gatewayKey), anthropicoption.WithMaxRetries(0))
	for _, model := range routes {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			params := sdkanthropic.MessageNewParams{
				Model:     model,
				MaxTokens: maxTokens,
				Messages:  []sdkanthropic.MessageParam{sdkanthropic.NewUserMessage(sdkanthropic.NewTextBlock(prompt))},
			}
			full, err := client.Messages.New(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			if len(full.Content) != 1 || full.Content[0].Text != exp.Text {
				t.Errorf("complete content = %+v, want %q", full.Content, exp.Text)
			}
			if full.Model != model || full.StopReason != sdkanthropic.StopReasonEndTurn {
				t.Errorf("model %q stop %q", full.Model, full.StopReason)
			}
			if full.Usage.OutputTokens != int64(exp.Usage.OutputTokens) || full.Usage.InputTokens != int64(exp.Usage.InputTokens) {
				t.Errorf("usage = %+v, want %+v", full.Usage, exp.Usage)
			}

			stream := client.Messages.NewStreaming(ctx, params)
			var msg sdkanthropic.Message
			deltas := 0
			for stream.Next() {
				ev := stream.Current()
				if err := msg.Accumulate(ev); err != nil {
					t.Fatal(err)
				}
				if ev.Type == "content_block_delta" {
					deltas++
				}
			}
			if err := stream.Err(); err != nil {
				t.Fatal(err)
			}
			if len(msg.Content) != 1 || msg.Content[0].Text != exp.Text {
				t.Errorf("streamed content = %+v, want %q", msg.Content, exp.Text)
			}
			if deltas != fakeShape.Tokens {
				t.Errorf("deltas = %d, want one per token (%d)", deltas, fakeShape.Tokens)
			}
			if msg.StopReason != sdkanthropic.StopReasonEndTurn || msg.Usage.OutputTokens != int64(exp.Usage.OutputTokens) {
				t.Errorf("streamed stop %q usage %+v", msg.StopReason, msg.Usage)
			}
		})
	}

	t.Run("unknown model", func(t *testing.T) {
		t.Parallel()
		_, err := client.Messages.New(context.Background(), sdkanthropic.MessageNewParams{
			Model: "nope", MaxTokens: 8,
			Messages: []sdkanthropic.MessageParam{sdkanthropic.NewUserMessage(sdkanthropic.NewTextBlock(prompt))},
		})
		var apiErr *sdkanthropic.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			t.Fatalf("err = %v, want a 404 from the SDK", err)
		}
	})
}

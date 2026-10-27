package server_test

import (
	"context"
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
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/pipeline"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/server"
)

// A reply filled through the OpenAI front end is replayed to an
// unmodified Anthropic SDK as an Anthropic event stream, and the other way
// round, with the cache status in the response headers.
func TestCachedReplyCrossesWireFormats(t *testing.T) {
	t.Parallel()
	f := fake.New(fakeShape)
	c := cache.New(cache.Options{
		KV:        cache.NewMemoryKV(100, time.Now),
		NewIndex:  func(d int) (cache.Index, error) { return cache.NewMemoryIndex(d, 100, time.Now), nil },
		Embedder:  &embed.Fake{Dims: 64},
		Threshold: 0.9,
		TTL:       time.Hour,
	})
	gw := pipeline.New(auth.NewKeys([]config.Key{{Name: "e2e", Key: gatewayKey, Cache: "semantic"}}),
		[]pipeline.Route{{Model: "fake", ProviderName: "fake", Upstream: "fake", Provider: f}},
		pipeline.Policy{Cache: c})
	front := httptest.NewServer(server.Handler(gw, slog.New(slog.DiscardHandler)))
	t.Cleanup(front.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var raw *http.Response
	oai := openai.NewClient(openaioption.WithBaseURL(front.URL+"/v1/"), openaioption.WithAPIKey(gatewayKey), openaioption.WithMaxRetries(0))
	ant := sdkanthropic.NewClient(anthropicoption.WithBaseURL(front.URL), anthropicoption.WithAPIKey(gatewayKey), anthropicoption.WithMaxRetries(0))

	// Fill through OpenAI, non-streaming.
	full, err := oai.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: "fake", MaxTokens: openai.Int(maxTokens), Temperature: openai.Float(0),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	}, openaioption.WithResponseInto(&raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.Header.Get(cache.StatusHeader); got != "miss" {
		t.Fatalf("first call: %s = %q, want miss", cache.StatusHeader, got)
	}
	c.Wait()

	// Replay through Anthropic, streaming, with the prompt reworded.
	stream := ant.Messages.NewStreaming(ctx, sdkanthropic.MessageNewParams{
		Model: "fake", MaxTokens: maxTokens, Temperature: sdkanthropic.Float(0),
		Messages: []sdkanthropic.MessageParam{sdkanthropic.NewUserMessage(sdkanthropic.NewTextBlock("Primary colours: name three."))},
	}, anthropicoption.WithResponseInto(&raw))
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
	if got := raw.Header.Get(cache.StatusHeader); got != "hit-semantic" || raw.Header.Get(cache.SimilarityHeader) == "" {
		t.Fatalf("replay headers: status %q similarity %q", got, raw.Header.Get(cache.SimilarityHeader))
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != full.Choices[0].Message.Content {
		t.Errorf("replayed content = %+v, want %q", msg.Content, full.Choices[0].Message.Content)
	}
	if deltas < 2 || msg.Model != "fake" || msg.StopReason != sdkanthropic.StopReasonEndTurn {
		t.Errorf("replay: %d deltas, model %q, stop %q", deltas, msg.Model, msg.StopReason)
	}
	if f.Calls() != 1 {
		t.Errorf("upstream calls = %d, want 1", f.Calls())
	}

	// And back: an exact hit for the OpenAI client, streaming.
	s := oai.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model: "fake", MaxTokens: openai.Int(maxTokens), Temperature: openai.Float(0),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	}, openaioption.WithResponseInto(&raw))
	var acc openai.ChatCompletionAccumulator
	for s.Next() {
		acc.AddChunk(s.Current())
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if got := raw.Header.Get(cache.StatusHeader); got != "hit-exact" {
		t.Errorf("streamed exact hit: status %q", got)
	}
	if acc.Choices[0].Message.Content != full.Choices[0].Message.Content || f.Calls() != 1 {
		t.Errorf("streamed hit text %q, upstream calls %d", acc.Choices[0].Message.Content, f.Calls())
	}
}

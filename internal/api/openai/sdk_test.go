package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/TheAsianFish/switchboard/internal/api/openai"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

const sdkKey = "sk-sdk"

// fakeGateway serves a fake provider to callers holding sdkKey.
type fakeGateway struct{ p *fake.Provider }

func (g fakeGateway) Authenticate(ctx context.Context, key string) (context.Context, error) {
	if key != sdkKey {
		return nil, core.Errorf(core.KindAuthentication, "invalid API key")
	}
	return ctx, nil
}

func (g fakeGateway) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	return g.p.Complete(ctx, req)
}

func (g fakeGateway) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	return g.p.Stream(ctx, req)
}

func (g fakeGateway) Models(context.Context) []string { return []string{"fake"} }

func sdkServer(t *testing.T) (*httptest.Server, *fake.Provider) {
	t.Helper()
	p := fake.New(fake.Config{Tokens: 12})
	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", openai.ChatCompletions(fakeGateway{p}))
	mux.Handle("/v1/models", openai.ListModels(fakeGateway{p}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, p
}

func sdkClient(srv *httptest.Server, key string) *sdk.Client {
	c := sdk.NewClient(
		option.WithBaseURL(srv.URL+"/v1/"),
		option.WithAPIKey(key),
		option.WithHTTPClient(srv.Client()),
		option.WithMaxRetries(0),
	)
	return &c
}

func sdkParams() sdk.ChatCompletionNewParams {
	return sdk.ChatCompletionNewParams{
		Model: "fake",
		Messages: []sdk.ChatCompletionMessageParamUnion{
			sdk.SystemMessage("be brief"),
			sdk.UserMessage("name three colours"),
		},
	}
}

// expected is what the fake answers to sdkParams.
func expected(t *testing.T, p *fake.Provider) *core.Response {
	t.Helper()
	want, err := p.Complete(context.Background(), &core.Request{
		Model:    "fake",
		System:   "be brief",
		Messages: []core.Message{{Role: core.RoleUser, Text: "name three colours"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return want
}

func TestSDKComplete(t *testing.T) {
	t.Parallel()
	srv, p := sdkServer(t)
	got, err := sdkClient(srv, sdkKey).Chat.Completions.New(context.Background(), sdkParams())
	if err != nil {
		t.Fatal(err)
	}
	want := expected(t, p)
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != want.Text {
		t.Fatalf("choices %+v, want text %q", got.Choices, want.Text)
	}
	if got.Choices[0].FinishReason != "stop" || got.Model != "fake" || !strings.HasPrefix(got.ID, "chatcmpl-") {
		t.Errorf("finish %q model %q id %q", got.Choices[0].FinishReason, got.Model, got.ID)
	}
	if got.Usage.PromptTokens != 5 || got.Usage.CompletionTokens != 12 || got.Usage.TotalTokens != 17 {
		t.Errorf("usage %+v", got.Usage)
	}
}

func TestSDKStream(t *testing.T) {
	t.Parallel()
	srv, p := sdkServer(t)
	params := sdkParams()
	params.StreamOptions = sdk.ChatCompletionStreamOptionsParam{IncludeUsage: sdk.Bool(true)}
	stream := sdkClient(srv, sdkKey).Chat.Completions.NewStreaming(context.Background(), params)
	defer func() { _ = stream.Close() }()

	var acc sdk.ChatCompletionAccumulator
	var text strings.Builder
	chunks := 0
	for stream.Next() {
		c := stream.Current()
		acc.AddChunk(c)
		chunks++
		if len(c.Choices) > 0 {
			text.WriteString(c.Choices[0].Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	want := expected(t, p)
	if text.String() != want.Text {
		t.Errorf("streamed %q, want %q", text.String(), want.Text)
	}
	// role chunk, 12 deltas, finish chunk, usage chunk
	if chunks != 15 {
		t.Errorf("chunks = %d", chunks)
	}
	if len(acc.Choices) != 1 || acc.Choices[0].Message.Content != want.Text || acc.Choices[0].FinishReason != "stop" {
		t.Errorf("accumulated %+v", acc.Choices)
	}
	if acc.Usage.PromptTokens != 5 || acc.Usage.CompletionTokens != 12 || acc.Usage.TotalTokens != 17 {
		t.Errorf("usage %+v", acc.Usage)
	}
}

// A streamed tool-call conversation: the fake calls the tool, the SDK
// accumulates the call, and the tool's result goes back in a "tool"
// message for a text answer.
func TestSDKStreamToolConversation(t *testing.T) {
	t.Parallel()
	srv, p := sdkServer(t)
	client := sdkClient(srv, sdkKey)
	ctx := context.Background()
	params := sdk.ChatCompletionNewParams{
		Model:    "fake",
		Messages: []sdk.ChatCompletionMessageParamUnion{sdk.UserMessage("weather in Paris?")},
		Tools: []sdk.ChatCompletionToolUnionParam{sdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "get_weather",
			Description: sdk.String("Weather for a city"),
			Parameters:  shared.FunctionParameters{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		})},
		ToolChoice: sdk.ChatCompletionToolChoiceOptionUnionParam{OfAuto: sdk.String("auto")},
	}

	turn := func() (sdk.ChatCompletionAccumulator, []sdk.FinishedChatCompletionToolCall) {
		t.Helper()
		stream := client.Chat.Completions.NewStreaming(ctx, params)
		defer func() { _ = stream.Close() }()
		var acc sdk.ChatCompletionAccumulator
		var finished []sdk.FinishedChatCompletionToolCall
		for stream.Next() {
			acc.AddChunk(stream.Current())
			if tc, ok := acc.JustFinishedToolCall(); ok {
				finished = append(finished, tc)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		if len(acc.Choices) != 1 {
			t.Fatalf("choices %+v", acc.Choices)
		}
		return acc, finished
	}

	acc, finished := turn()
	msg := acc.Choices[0].Message
	if acc.Choices[0].FinishReason != "tool_calls" || len(msg.ToolCalls) != 1 || len(finished) != 1 {
		t.Fatalf("finish %q, calls %+v, finished %+v", acc.Choices[0].FinishReason, msg.ToolCalls, finished)
	}
	call := msg.ToolCalls[0]
	var args map[string]string
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args["city"] == "" {
		t.Fatalf("arguments %q: %v", call.Function.Arguments, err)
	}
	if call.Function.Name != "get_weather" || !strings.HasPrefix(call.ID, "call_") || finished[0].ID != call.ID || finished[0].Arguments != call.Function.Arguments {
		t.Errorf("call %+v, finished %+v", call, finished[0])
	}

	params.Messages = append(params.Messages, msg.ToParam(), sdk.ToolMessage("18 C and sunny", call.ID))
	acc, finished = turn()
	if acc.Choices[0].FinishReason != "stop" || acc.Choices[0].Message.Content == "" || len(finished) != 0 {
		t.Fatalf("answer %+v", acc.Choices[0])
	}
	got := p.LastRequest()
	want := []core.Message{
		{Role: core.RoleUser, Text: "weather in Paris?"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: call.ID, Name: "get_weather", Arguments: json.RawMessage(call.Function.Arguments)}}},
		{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: call.ID, Content: "18 C and sunny"}}},
	}
	if !reflect.DeepEqual(got.Messages, want) || len(got.Tools) != 1 || got.ToolChoice.Mode != core.ToolChoiceAuto {
		t.Errorf("provider saw %+v\nwant messages %+v", got, want)
	}
}

func TestSDKInvalidKey(t *testing.T) {
	t.Parallel()
	srv, _ := sdkServer(t)
	client := sdkClient(srv, "wrong")

	_, err := client.Chat.Completions.New(context.Background(), sdkParams())
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *openai.Error", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "invalid_api_key" || apiErr.Type != "invalid_request_error" {
		t.Errorf("error %d code %q type %q", apiErr.StatusCode, apiErr.Code, apiErr.Type)
	}

	stream := client.Chat.Completions.NewStreaming(context.Background(), sdkParams())
	defer func() { _ = stream.Close() }()
	if stream.Next() {
		t.Fatal("stream yielded a chunk with an invalid key")
	}
	if !errors.As(stream.Err(), &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("stream err = %v", stream.Err())
	}
}

func TestSDKListModels(t *testing.T) {
	t.Parallel()
	srv, _ := sdkServer(t)
	page, err := sdkClient(srv, sdkKey).Models.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "fake" || page.Data[0].OwnedBy != "switchboard" {
		t.Errorf("models %+v", page.Data)
	}
}

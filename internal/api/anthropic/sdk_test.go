package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// sdkServer serves the front end over a gateway wrapping the fake provider.
func sdkServer(t *testing.T) (*httptest.Server, *fake.Provider) {
	t.Helper()
	p := fake.New(fake.Config{Tokens: 12})
	mux := http.NewServeMux()
	gw := &testGateway{p: p, models: []string{"fake"}}
	mux.Handle("POST /v1/messages", Messages(gw))
	mux.Handle("GET /v1/models", ListModels(gw))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, p
}

func sdkClient(srv *httptest.Server, key string) sdk.Client {
	return sdk.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(srv.URL),
		option.WithAPIKey(key),
		option.WithHTTPClient(srv.Client()),
		option.WithMaxRetries(0),
	)
}

func sdkParams() sdk.MessageNewParams {
	return sdk.MessageNewParams{
		Model:     "fake",
		MaxTokens: 100,
		System:    []sdk.TextBlockParam{{Text: "be terse"}},
		Messages:  []sdk.MessageParam{sdk.NewUserMessage(sdk.NewTextBlock("name three colours"))},
	}
}

func TestSDKMessages(t *testing.T) {
	t.Parallel()
	srv, p := sdkServer(t)
	client := sdkClient(srv, testKey)
	ctx := context.Background()

	want, err := p.Complete(ctx, &core.Request{Model: "fake", System: "be terse", MaxTokens: 100, Messages: []core.Message{{Role: core.RoleUser, Text: "name three colours"}}})
	if err != nil {
		t.Fatal(err)
	}

	msg, err := client.Messages.New(ctx, sdkParams())
	if err != nil {
		t.Fatal(err)
	}
	check := func(mode string, m *sdk.Message) {
		t.Helper()
		if len(m.Content) != 1 || m.Content[0].Type != "text" || m.Content[0].Text != want.Text {
			t.Errorf("%s: content %+v, want %q", mode, m.Content, want.Text)
		}
		if m.StopReason != sdk.StopReasonEndTurn || m.Model != "fake" || !strings.HasPrefix(m.ID, "msg_") {
			t.Errorf("%s: stop %q model %q id %q", mode, m.StopReason, m.Model, m.ID)
		}
		if m.Usage.InputTokens != int64(want.Usage.InputTokens) || m.Usage.OutputTokens != 12 {
			t.Errorf("%s: usage in %d out %d, want %d 12", mode, m.Usage.InputTokens, m.Usage.OutputTokens, want.Usage.InputTokens)
		}
	}
	check("new", msg)

	stream := client.Messages.NewStreaming(ctx, sdkParams())
	defer func() { _ = stream.Close() }()
	var acc sdk.Message
	var types []string
	for stream.Next() {
		ev := stream.Current()
		types = append(types, ev.Type)
		if err := acc.Accumulate(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	check("stream", &acc)
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" || len(types) != 12+5 {
		t.Errorf("event types %v", types)
	}

	models, err := client.Models.List(ctx, sdk.ModelListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 1 || models.Data[0].ID != "fake" {
		t.Errorf("models %+v", models.Data)
	}
}

func TestSDKBadKey(t *testing.T) {
	t.Parallel()
	srv, p := sdkServer(t)
	client := sdkClient(srv, "wrong")
	ctx := context.Background()

	_, err := client.Messages.New(ctx, sdkParams())
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || apiErr.Type() != sdk.ErrorTypeAuthenticationError {
		t.Fatalf("err %v, want a 401 authentication_error", err)
	}

	stream := client.Messages.NewStreaming(ctx, sdkParams())
	defer func() { _ = stream.Close() }()
	if stream.Next() {
		t.Fatal("stream yielded an event with a bad key")
	}
	if !errors.As(stream.Err(), &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stream err %v, want 401", stream.Err())
	}
	if p.Calls() != 0 {
		t.Errorf("provider called %d times", p.Calls())
	}
}

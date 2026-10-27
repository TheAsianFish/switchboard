package provider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/provider"
)

// replay serves one recorded capture whatever the request.
func replay(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if strings.HasSuffix(path, ".sse") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Every provider reports usage in the canonical convention (input excludes
// cache reads), so one price table prices both wire formats the same way.
// The captures are real Ollama replies to the same prompt through each API;
// the cached share differs between captures because Ollama's prompt cache
// was warm for some of them, which is exactly what the split has to get
// right.
func TestUsageIsPricedTheSameAcrossProviders(t *testing.T) {
	t.Parallel()
	haiku := cost.Defaults["claude-haiku-5-5"]
	tests := []struct {
		name, typ, fixture string
		stream             bool
		want               core.Usage
	}{
		{"openai json", config.TypeOpenAICompat, "openai/ollama-chat.json", false, core.Usage{InputTokens: 21, CacheReadTokens: 3, OutputTokens: 6}},
		{"openai stream", config.TypeOpenAICompat, "openai/ollama-chat-stream.sse", true, core.Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
		{"anthropic json", config.TypeAnthropic, "anthropic/ollama-messages.json", false, core.Usage{InputTokens: 1, CacheReadTokens: 23, OutputTokens: 6}},
		{"anthropic stream", config.TypeAnthropic, "anthropic/ollama-messages-stream.sse", true, core.Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
	}
	streamed := map[cost.Nanos]bool{}
	for _, tt := range tests {
		url := replay(t, filepath.Join("testdata", tt.fixture))
		if tt.typ == config.TypeOpenAICompat {
			url += "/v1"
		}
		p, err := provider.Build(config.Provider{Name: "x", Type: tt.typ, BaseURL: url, APIKey: "k", Timeout: config.DefaultTimeout, DefaultMaxTokens: 64})
		if err != nil {
			t.Fatal(err)
		}
		req := &core.Request{Model: "qwen2.5:0.5b", MaxTokens: 50, Stream: tt.stream, Messages: []core.Message{{Role: core.RoleUser, Text: "Name the three primary colours."}}}
		var resp *core.Response
		if tt.stream {
			s, err := p.Stream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			resp, err = core.Collect(s)
			if err != nil {
				t.Fatal(err)
			}
		} else if resp, err = p.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if resp.Usage != tt.want {
			t.Errorf("%s: usage %+v, want %+v", tt.name, resp.Usage, tt.want)
		}
		got := haiku.Of(resp.Usage)
		want := cost.Nanos(tt.want.InputTokens*100 + tt.want.CacheReadTokens*10 + tt.want.OutputTokens*500)
		if got != want {
			t.Errorf("%s: priced %v, want %v", tt.name, got, want)
		}
		if tt.stream {
			streamed[got] = true
		}
	}
	// The two streamed captures saw the same cache state, so through
	// either wire format they cost the same.
	if len(streamed) != 1 {
		t.Errorf("the streamed captures priced %d different ways, want 1", len(streamed))
	}
}

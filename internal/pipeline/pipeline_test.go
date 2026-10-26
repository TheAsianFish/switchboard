package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/auth"
	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

func newPipeline(t *testing.T) (*Pipeline, *fake.Provider) {
	t.Helper()
	f := fake.New(fake.Config{Tokens: 5})
	keys := auth.NewKeys([]config.Key{
		{Name: "all", Key: "sk-all"},
		{Name: "small", Key: "sk-small", Models: []string{"small"}},
	})
	return New(keys, []Route{
		{Model: "small", ProviderName: "fake", Upstream: "fake-small", Provider: f},
		{Model: "big", ProviderName: "fake", Upstream: "fake-big", Provider: f},
	}), f
}

func TestResolve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		key      string
		model    string
		wantKind core.ErrorKind
		wantErr  bool
	}{
		{"allowed", "sk-all", "big", 0, false},
		{"restricted key on its model", "sk-small", "small", 0, false},
		{"restricted key elsewhere", "sk-small", "big", core.KindPermission, true},
		{"unknown model", "sk-all", "nope", core.KindNotFound, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, f := newPipeline(t)
			ctx, err := p.Authenticate(context.Background(), tt.key)
			if err != nil {
				t.Fatal(err)
			}
			req := &core.Request{Model: tt.model, Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}
			resp, err := p.Complete(ctx, req)
			if tt.wantErr {
				if err == nil || core.AsError(err).Kind != tt.wantKind {
					t.Fatalf("err = %v, want kind %v", err, tt.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.Model != tt.model {
				t.Errorf("response model = %q, want the client's name %q", resp.Model, tt.model)
			}
			if got := f.LastRequest().Model; got != "fake-"+tt.model {
				t.Errorf("provider saw model %q, want upstream name", got)
			}
			if req.Model != tt.model {
				t.Errorf("caller's request was modified: %q", req.Model)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	p, _ := newPipeline(t)
	for _, key := range []string{"", "sk-wrong"} {
		if _, err := p.Authenticate(context.Background(), key); core.AsError(err).Kind != core.KindAuthentication {
			t.Errorf("key %q: err = %v", key, err)
		}
	}
	if _, err := p.Complete(context.Background(), &core.Request{Model: "big"}); core.AsError(err).Kind != core.KindAuthentication {
		t.Errorf("unauthenticated Complete: err = %v", err)
	}
	ctx, _ := p.Authenticate(context.Background(), "sk-small")
	if got := p.Models(ctx); len(got) != 1 || got[0] != "small" {
		t.Errorf("Models = %v", got)
	}
	ctx, _ = p.Authenticate(context.Background(), "sk-all")
	if got := p.Models(ctx); strings.Join(got, ",") != "big,small" {
		t.Errorf("Models = %v", got)
	}
}

func TestStreamRewritesModelAndLogsOnce(t *testing.T) {
	t.Parallel()
	p, _ := newPipeline(t)
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx := telemetry.WithRequest(context.Background(), base, "req_test")
	ctx, err := p.Authenticate(ctx, "sk-all")
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.Stream(ctx, &core.Request{Model: "big", Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.Next()
	if err != nil || ev.Kind != core.EventStart || ev.Model != "big" {
		t.Fatalf("first event = %+v, %v", ev, err)
	}
	resp, err := core.Collect(s)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	out := buf.String()
	if n := strings.Count(out, `"msg":"llm call"`); n != 1 {
		t.Fatalf("logged %d call lines, want 1:\n%s", n, out)
	}
	for _, want := range []string{`"request_id":"req_test"`, `"key":"all"`, `"output_tokens":5`, `"ttft_ms"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log line lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-all") {
		t.Error("log contains the API key")
	}
}

func TestStreamAbandoned(t *testing.T) {
	t.Parallel()
	p, _ := newPipeline(t)
	var buf bytes.Buffer
	ctx := telemetry.WithRequest(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)), "r")
	ctx, _ = p.Authenticate(ctx, "sk-all")
	s, err := p.Stream(ctx, &core.Request{Model: "big"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"outcome":"abandoned"`) {
		t.Errorf("log = %s", buf.String())
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	f := fake.New(fake.Config{Tokens: 2})
	var gw core.Gateway = Open{Provider: f, Names: []string{"fake"}}
	ctx, err := gw.Authenticate(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gw.Complete(ctx, &core.Request{Model: "anything"})
	if err != nil || resp.Model != "anything" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

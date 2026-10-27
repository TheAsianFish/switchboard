package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
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
	for _, want := range []string{`"request_id":"req_test"`, `"key":"all"`, `"output_tokens":5`, `"ttft_ms"`, `"cache_write_tokens":`} {
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

// scripted is a provider that fails or blocks as a test needs.
type scripted struct {
	err    error
	stream core.Stream
}

func (s scripted) Complete(context.Context, *core.Request) (*core.Response, error) { return nil, s.err }
func (s scripted) Stream(context.Context, *core.Request) (core.Stream, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.stream, nil
}

func TestUpstreamCredentialErrorsAreNotTheClients(t *testing.T) {
	t.Parallel()
	for _, kind := range []core.ErrorKind{core.KindAuthentication, core.KindPermission} {
		upstream := core.Errorf(kind, "upstream: Incorrect API key provided: sk-proj-abc*****wxyz")
		p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk"}}), []Route{
			{Model: "m", ProviderName: "openai", Upstream: "m", Provider: scripted{err: upstream}},
		})
		ctx, _ := p.Authenticate(context.Background(), "sk")
		for name, call := range map[string]func() error{
			"complete": func() error { _, err := p.Complete(ctx, &core.Request{Model: "m"}); return err },
			"stream":   func() error { _, err := p.Stream(ctx, &core.Request{Model: "m"}); return err },
		} {
			e := core.AsError(call())
			if e.Kind != core.KindUpstream || strings.Contains(e.Message, "sk-proj") || !errors.Is(e, upstream) {
				t.Errorf("%v %s: got %v (message %q)", kind, name, e.Kind, e.Message)
			}
		}
	}
}

// blocking blocks in Next until closed, like an upstream read.
type blocking struct{ closed chan struct{} }

func (b *blocking) Next() (core.Event, error) {
	<-b.closed
	return core.Event{}, errors.New("read on closed connection")
}
func (b *blocking) Close() error { close(b.closed); return nil }

// endless yields a start event and then deltas forever, with nothing that
// orders its reads against a concurrent Close.
type endless struct{ n atomic.Int64 }

func (e *endless) Next() (core.Event, error) {
	if e.n.Add(1) == 1 {
		return core.Event{Kind: core.EventStart, Usage: core.Usage{InputTokens: 1}}, nil
	}
	return core.Event{Kind: core.EventDelta, Text: "x"}, nil
}
func (e *endless) Close() error { return nil }

func TestStreamCloseFromAnotherGoroutine(t *testing.T) {
	t.Parallel()
	p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk"}}), []Route{
		{Model: "m", ProviderName: "x", Upstream: "m", Provider: scripted{stream: &endless{}}},
	})
	ctx, _ := p.Authenticate(context.Background(), "sk")
	s, err := p.Stream(ctx, &core.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.Next()
			}
		}
	}()
	// Close logs the call, reading what Next writes; -race catches it if
	// that state is not guarded.
	_ = s.Close()
	close(stop)
	<-done
}

func TestStreamClientGoneIsAbandoned(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base, cancel := context.WithCancel(context.Background())
	inner := &blocking{closed: make(chan struct{})}
	p := New(auth.NewKeys([]config.Key{{Name: "k", Key: "sk"}}), []Route{
		{Model: "m", ProviderName: "x", Upstream: "m", Provider: scripted{stream: inner}},
	})
	ctx := telemetry.WithRequest(base, slog.New(slog.NewJSONHandler(&buf, nil)), "r")
	ctx, _ = p.Authenticate(ctx, "sk")
	s, _ := p.Stream(ctx, &core.Request{Model: "m"})
	cancel()
	close(inner.closed) // the cancelled context fails the upstream read
	if _, err := s.Next(); err == nil {
		t.Fatal("want the read error")
	}
	if !strings.Contains(buf.String(), `"outcome":"abandoned"`) || strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("log = %s", buf.String())
	}
}

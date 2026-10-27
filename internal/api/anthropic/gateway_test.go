package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
)

const testKey = "sk-test-key"

// testGateway accepts testKey and forwards to a provider.
type testGateway struct {
	p      core.Provider
	models []string
}

func (g *testGateway) Authenticate(ctx context.Context, apiKey string) (context.Context, error) {
	if apiKey != testKey {
		return nil, core.Errorf(core.KindAuthentication, "invalid x-api-key")
	}
	return ctx, nil
}

func (g *testGateway) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	return g.p.Complete(ctx, req)
}

func (g *testGateway) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	return g.p.Stream(ctx, req)
}

func (g *testGateway) Models(context.Context) []string { return g.models }

// stubProvider answers with fixed results.
type stubProvider struct {
	resp      *core.Response
	err       error
	newStream func(ctx context.Context) core.Stream
	last      *core.Request
}

func (p *stubProvider) Complete(_ context.Context, req *core.Request) (*core.Response, error) {
	p.last = req
	return p.resp, p.err
}

func (p *stubProvider) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	p.last = req
	if p.err != nil {
		return nil, p.err
	}
	return p.newStream(ctx), nil
}

// errStream returns events, then err.
type errStream struct {
	core.SliceStream
	err error
}

func (s *errStream) Next() (core.Event, error) {
	ev, err := s.SliceStream.Next()
	if errors.Is(err, io.EOF) && s.err != nil {
		return core.Event{}, s.err
	}
	return ev, err
}

// post sends body to the Messages handler with the test key.
func post(t *testing.T, gw core.Gateway, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("x-api-key", testKey)
	r.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	Messages(gw).ServeHTTP(w, r)
	return w
}

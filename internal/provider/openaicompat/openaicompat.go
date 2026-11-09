// Package openaicompat is a provider for any server that speaks the OpenAI
// chat completions API at /v1/chat/completions: Ollama, vLLM, llama.cpp and
// similar inference servers.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// DefaultTimeout bounds a whole call, including a stream, when Config.Timeout
// is zero.
const DefaultTimeout = 10 * time.Minute

// maxResponseBytes bounds a non-streamed response body.
const maxResponseBytes = 32 << 20

// maxErrorBytes bounds the part of an error body that is read.
const maxErrorBytes = 1 << 20

// Config configures a Provider.
type Config struct {
	// BaseURL is the API root, for example http://localhost:11434/v1.
	BaseURL string
	// APIKey is sent as a bearer token when set.
	APIKey string
	// HTTPClient is used for every call; nil means a client whose transport
	// keeps up to 256 idle connections to the upstream.
	HTTPClient *http.Client
	// Timeout is the deadline for a whole call, stream included; zero means
	// DefaultTimeout.
	Timeout time.Duration
}

// Provider calls an OpenAI-compatible server. It is safe for concurrent use.
type Provider struct {
	endpoint string
	apiKey   string
	client   *http.Client
	timeout  time.Duration
}

// New returns a provider for cfg.
func New(cfg Config) (*Provider, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: base URL %q: %w", cfg.BaseURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("openaicompat: base URL %q: want an absolute http or https URL", cfg.BaseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("openaicompat: base URL %q: must not have a query or fragment", cfg.BaseURL)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: newTransport()}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Provider{
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		apiKey:   cfg.APIKey,
		client:   client,
		timeout:  timeout,
	}, nil
}

// newTransport is tuned for many concurrent calls to one upstream host.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Complete implements core.Provider.
func (p *Provider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	resp, err := p.post(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	var out completion
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, core.Wrap(core.KindUpstream, err, "upstream: invalid response body")
	}
	if out.Error != nil {
		return nil, core.Errorf(core.KindUpstream, "upstream: %s", out.Error.message())
	}
	if len(out.Choices) == 0 {
		return nil, core.Errorf(core.KindUpstream, "upstream: response has no choices")
	}
	c := out.Choices[0]
	r := &core.Response{
		ID:         out.ID,
		Model:      out.Model,
		ToolCalls:  toolCalls(c.Message.ToolCalls),
		StopReason: stopReason(c.FinishReason, len(c.Message.ToolCalls) > 0),
	}
	if c.Message.Content != nil {
		r.Text = *c.Message.Content
	}
	if out.Usage != nil {
		r.Usage = out.Usage.canonical()
	}
	return r, nil
}

// Stream implements core.Provider. It returns once the upstream has answered
// with a success status; events are read by the returned stream.
func (p *Provider) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	resp, err := p.post(ctx, req, true) //nolint:bodyclose // the stream closes the body
	if err != nil {
		cancel()
		return nil, err
	}
	return newStream(ctx, cancel, resp.Body), nil
}

// post sends req and returns a response with a 2xx status; any other status
// is turned into a classified error.
func (p *Provider) post(ctx context.Context, req *core.Request, stream bool) (*http.Response, error) {
	body, err := json.Marshal(buildRequest(req, stream))
	if err != nil {
		return nil, core.Wrap(core.KindInternal, err, "encode upstream request")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, core.Wrap(core.KindInternal, err, "build upstream request")
	}
	hreq.Header.Set("Content-Type", "application/json")
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	} else {
		hreq.Header.Set("Accept", "application/json")
	}
	if p.apiKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp)
	}
	return resp, nil
}

// statusError classifies a non-2xx upstream response.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	msg := errorMessage(body)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
		if msg == "" {
			msg = "status " + strconv.Itoa(resp.StatusCode)
		}
	}
	return &core.Error{
		Kind:       core.KindFromStatus(resp.StatusCode),
		Message:    "upstream: " + msg,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		Err:        fmt.Errorf("upstream status %d", resp.StatusCode),
	}
}

// errorMessage extracts the message from an error body in the shapes
// OpenAI-compatible servers use: {"error":{"message":..}}, {"error":".."}
// and {"message":..}.
func errorMessage(body []byte) string {
	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	if len(env.Error) > 0 {
		var obj apiError
		if json.Unmarshal(env.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
		var s string
		if json.Unmarshal(env.Error, &s) == nil && s != "" {
			return s
		}
	}
	return env.Message
}

// retryAfter parses a Retry-After header in seconds or as an HTTP date.
func retryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// transportError classifies a failure to talk to the upstream.
func transportError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return core.Wrap(core.KindTimeout, err, "upstream timed out")
	}
	return core.Wrap(core.KindUpstream, err, "upstream request failed")
}

// stopReason maps finish_reason to a canonical stop reason (the table is in
// core). Some servers end a reply that called tools with "stop" rather
// than "tool_calls"; a reply with tool calls that stopped on its own is
// read as tool_use either way, so clients know to run the tools.
func stopReason(finish string, calledTools bool) core.StopReason {
	switch finish {
	case "length":
		return core.StopMaxTokens
	case "content_filter":
		return core.StopContentFilter
	case "tool_calls", "function_call":
		return core.StopToolUse
	default:
		if calledTools {
			return core.StopToolUse
		}
		return core.StopEndTurn
	}
}

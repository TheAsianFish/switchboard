// Package anthropic is the provider for the Anthropic Messages API, built on
// the official anthropic-sdk-go client. It also serves any upstream that
// speaks the same wire format, such as Ollama's /v1/messages.
package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/TheAsianFish/switchboard/internal/core"
)

const (
	defaultTimeout   = 10 * time.Minute
	defaultMaxTokens = 4096
)

// Config configures a Provider.
type Config struct {
	// BaseURL is the API root; "" means the SDK default,
	// https://api.anthropic.com. For Ollama use e.g. http://localhost:11434.
	BaseURL string
	// APIKey is sent as x-api-key; "" sends no key (Ollama needs none).
	APIKey string
	// HTTPClient carries the requests; nil means a client whose transport
	// keeps up to 256 idle connections per host.
	HTTPClient *http.Client
	// Timeout is the deadline for a whole call, a stream included; 0 means
	// 10 minutes.
	Timeout time.Duration
	// DefaultMaxTokens is sent when a request has no MaxTokens, since the
	// Messages API requires one; 0 means 4096.
	DefaultMaxTokens int
}

// Provider implements core.Provider over the Messages API. It is safe for
// concurrent use.
type Provider struct {
	client    sdk.Client
	timeout   time.Duration
	maxTokens int
}

var _ core.Provider = (*Provider)(nil)

// New builds a provider from cfg. The SDK's own retries are off: retrying
// is the gateway's decision. The SDK's environment defaults
// (ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL, profiles) are not read, so the
// configuration is exactly cfg.
func New(cfg Config) (*Provider, error) {
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("anthropic provider: negative timeout %s", cfg.Timeout)
	}
	if cfg.DefaultMaxTokens < 0 {
		return nil, fmt.Errorf("anthropic provider: negative default max tokens %d", cfg.DefaultMaxTokens)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.DefaultMaxTokens == 0 {
		cfg.DefaultMaxTokens = defaultMaxTokens
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: newTransport()}
	}
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithHTTPClient(hc),
		option.WithMaxRetries(0),
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("anthropic provider: base URL %q: want an absolute http or https URL", cfg.BaseURL)
		}
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	return &Provider{
		client:    sdk.NewClient(opts...),
		timeout:   cfg.Timeout,
		maxTokens: cfg.DefaultMaxTokens,
	}, nil
}

// newTransport is the default transport, tuned for many concurrent
// requests to one upstream host.
func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = false
	t.MaxIdleConns = 1024
	t.MaxIdleConnsPerHost = 256
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// Complete implements core.Provider.
func (p *Provider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	// An explicit request timeout also stops the SDK from refusing
	// non-streamed calls whose max_tokens suggests a long generation.
	msg, err := p.client.Messages.New(ctx, p.params(req), option.WithRequestTimeout(p.timeout))
	if err != nil {
		return nil, classify(ctx, err)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return &core.Response{
		ID:         msg.ID,
		Model:      msg.Model,
		Text:       text.String(),
		StopReason: stopReason(msg.StopReason),
		Usage: core.Usage{
			InputTokens:      int(msg.Usage.InputTokens),
			OutputTokens:     int(msg.Usage.OutputTokens),
			CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
		},
	}, nil
}

// Stream implements core.Provider. Errors before the first event (status
// codes, connection failures) are returned here; the returned stream holds
// the call's deadline until it ends or is closed.
func (p *Provider) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	s := p.client.Messages.NewStreaming(ctx, p.params(req))
	if err := s.Err(); err != nil {
		err = classify(ctx, err) // before cancel, which would read as a cancellation
		_ = s.Close()
		cancel()
		return nil, err
	}
	return &stream{ctx: ctx, cancel: cancel, s: s}, nil
}

// params translates a canonical request into the SDK's parameters.
func (p *Provider) params(req *core.Request) sdk.MessageNewParams {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.maxTokens
	}
	params := sdk.MessageNewParams{
		Model:     req.Model,
		MaxTokens: int64(maxTokens),
		Messages:  make([]sdk.MessageParam, 0, len(req.Messages)),
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	for _, m := range req.Messages {
		block := sdk.NewTextBlock(m.Text)
		if m.Role == core.RoleAssistant {
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(block))
		} else {
			params.Messages = append(params.Messages, sdk.NewUserMessage(block))
		}
	}
	if req.Temperature != nil {
		params.Temperature = sdk.Float(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = sdk.Float(*req.TopP)
	}
	if len(req.Stop) > 0 {
		params.StopSequences = req.Stop
	}
	return params
}

// stopReason maps Anthropic's stop reason to the canonical one.
func stopReason(s sdk.StopReason) core.StopReason {
	switch s {
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		// Both cut the reply off; clients must not read it as finished.
		return core.StopMaxTokens
	case sdk.StopReasonStopSequence:
		return core.StopSequence
	case sdk.StopReasonRefusal:
		return core.StopContentFilter
	default:
		return core.StopEndTurn
	}
}

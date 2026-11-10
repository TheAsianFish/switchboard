// Package anthropic is the provider for the Anthropic Messages API, built on
// the official anthropic-sdk-go client. It also serves any upstream that
// speaks the same wire format, such as Ollama's /v1/messages.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

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
	var calls []core.ToolCall
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, core.ToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input})
		}
	}
	return &core.Response{
		ID:         msg.ID,
		Model:      msg.Model,
		Text:       text.String(),
		ToolCalls:  calls,
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
	return &stream{ctx: ctx, cancel: cancel, s: s, toolBlock: -1}, nil
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
		if m.Role == core.RoleAssistant {
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(assistantBlocks(m)...))
		} else {
			params.Messages = append(params.Messages, sdk.NewUserMessage(userBlocks(m)...))
		}
	}
	for _, t := range req.Tools {
		params.Tools = append(params.Tools, toolParam(t))
	}
	params.ToolChoice = toolChoice(req.ToolChoice)
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

// userBlocks renders a user turn: its tool results, then its text, which
// is left out when empty after tool results (the API refuses empty text
// blocks; a plain empty turn is sent as it was asked for).
func userBlocks(m core.Message) []sdk.ContentBlockParamUnion {
	blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.ToolResults)+1)
	for _, r := range m.ToolResults {
		res := sdk.ToolResultBlockParam{ToolUseID: r.CallID}
		if r.Content != "" {
			res.Content = []sdk.ToolResultBlockParamContentUnion{{OfText: &sdk.TextBlockParam{Text: r.Content}}}
		}
		if r.IsError {
			res.IsError = sdk.Bool(true)
		}
		blocks = append(blocks, sdk.ContentBlockParamUnion{OfToolResult: &res})
	}
	if m.Text != "" || len(blocks) == 0 {
		blocks = append(blocks, sdk.NewTextBlock(m.Text))
	}
	return blocks
}

// assistantBlocks renders an assistant turn: its text, then its tool calls.
// Arguments are sent as raw JSON; none means {}.
func assistantBlocks(m core.Message) []sdk.ContentBlockParamUnion {
	blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.ToolCalls)+1)
	if m.Text != "" || len(m.ToolCalls) == 0 {
		blocks = append(blocks, sdk.NewTextBlock(m.Text))
	}
	for _, c := range m.ToolCalls {
		input := c.Arguments
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, sdk.NewToolUseBlock(c.ID, input, c.Name))
	}
	return blocks
}

// toolParam renders a tool with its schema as sent; a tool without one
// (the OpenAI format allows that) gets an empty object schema.
func toolParam(t core.Tool) sdk.ToolUnionParam {
	raw := t.Parameters
	if len(raw) == 0 {
		raw = json.RawMessage(`{"type":"object"}`) // a zero schema param would be left out
	}
	var schema sdk.ToolInputSchemaParam
	param.SetJSON(raw, &schema)
	tool := sdk.ToolParam{Name: t.Name, InputSchema: schema}
	if t.Description != "" {
		tool.Description = sdk.String(t.Description)
	}
	return sdk.ToolUnionParam{OfTool: &tool}
}

// toolChoice renders a tool choice. With no mode but parallel calls off it
// is auto, the default the client left unsaid.
func toolChoice(tc core.ToolChoice) sdk.ToolChoiceUnionParam {
	var disable param.Opt[bool]
	if tc.DisableParallel {
		disable = sdk.Bool(true)
	}
	switch tc.Mode {
	case core.ToolChoiceNone:
		return sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}
	case core.ToolChoiceAny:
		return sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{DisableParallelToolUse: disable}}
	case core.ToolChoiceTool:
		return sdk.ToolChoiceUnionParam{OfTool: &sdk.ToolChoiceToolParam{Name: tc.Name, DisableParallelToolUse: disable}}
	case core.ToolChoiceAuto:
		return sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{DisableParallelToolUse: disable}}
	default:
		if tc.DisableParallel {
			return sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{DisableParallelToolUse: disable}}
		}
		return sdk.ToolChoiceUnionParam{}
	}
}

// stopReason maps Anthropic's stop reason to the canonical one (the table
// is in core).
func stopReason(s sdk.StopReason) core.StopReason {
	switch s {
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		// Both cut the reply off; clients must not read it as finished.
		return core.StopMaxTokens
	case sdk.StopReasonStopSequence:
		return core.StopSequence
	case sdk.StopReasonToolUse:
		return core.StopToolUse
	case sdk.StopReasonRefusal:
		return core.StopContentFilter
	default:
		// end_turn, and pause_turn: that belongs to server tools, which
		// the gateway never sends, and leaves nothing to continue.
		return core.StopEndTurn
	}
}

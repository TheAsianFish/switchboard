package openaicompat

import (
	"encoding/json"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// chatRequest is the upstream request body.
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []tool         `json:"tools,omitempty"`
	// ToolChoice is a string or a named function; nil leaves it unsaid.
	ToolChoice        any   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// chatMessage is one message. Content is always a string, empty in an
// assistant message that only calls tools: every server tried accepts
// that, where some refuse null.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toolCall is a tool call in a request, a reply or (with Index set) a
// streamed delta, where every field but Index may be missing.
type toolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type namedChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// buildRequest translates a canonical request into the upstream body.
func buildRequest(req *core.Request, stream bool) chatRequest {
	out := chatRequest{
		Model:       req.Model,
		Messages:    make([]chatMessage, 0, len(req.Messages)+1),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	if req.MaxTokens > 0 {
		out.MaxTokens = req.MaxTokens
	}
	if req.System != "" {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		out.Messages = appendMessage(out.Messages, m)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, tool{Type: "function", Function: toolFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	out.ToolChoice = toolChoice(req.ToolChoice)
	if req.ToolChoice.DisableParallel {
		off := false
		out.ParallelToolCalls = &off
	}
	if stream {
		out.Stream = true
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return out
}

// appendMessage renders one canonical turn. Tool results become one "tool"
// message each, before the turn's text, which is left out when empty; the
// is_error flag has no place in this format and is dropped. Tool call
// arguments go as the JSON text they are, {} when there are none.
func appendMessage(msgs []chatMessage, m core.Message) []chatMessage {
	for _, r := range m.ToolResults {
		msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: r.CallID, Content: r.Content})
	}
	if len(m.ToolResults) > 0 && m.Text == "" && len(m.ToolCalls) == 0 {
		return msgs
	}
	msg := chatMessage{Role: string(m.Role), Content: m.Text}
	for _, c := range m.ToolCalls {
		args := string(c.Arguments)
		if args == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, toolCall{ID: c.ID, Type: "function", Function: functionCall{Name: c.Name, Arguments: args}})
	}
	return append(msgs, msg)
}

func toolChoice(tc core.ToolChoice) any {
	switch tc.Mode {
	case core.ToolChoiceAuto:
		return "auto"
	case core.ToolChoiceNone:
		return "none"
	case core.ToolChoiceAny:
		return "required"
	case core.ToolChoiceTool:
		var c namedChoice
		c.Type = "function"
		c.Function.Name = tc.Name
		return c
	default:
		return nil
	}
}

// toolCalls converts the tool calls of a non-streamed reply.
func toolCalls(in []toolCall) []core.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]core.ToolCall, len(in))
	for i, c := range in {
		out[i] = core.ToolCall{ID: c.ID, Name: c.Function.Name}
		if c.Function.Arguments != "" {
			out[i].Arguments = json.RawMessage(c.Function.Arguments)
		}
	}
	return out
}

// completion is a non-streamed response body.
type completion struct {
	ID      string    `json:"id"`
	Model   string    `json:"model"`
	Choices []choice  `json:"choices"`
	Usage   *usage    `json:"usage"`
	Error   *apiError `json:"error"`
}

type choice struct {
	Message struct {
		Content   *string    `json:"content"`
		ToolCalls []toolCall `json:"tool_calls"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

// chunk is one streamed event.
type chunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage"`
	Error   *apiError     `json:"error"`
}

type chunkChoice struct {
	Delta struct {
		Content   *string    `json:"content"`
		ToolCalls []toolCall `json:"tool_calls"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// canonical converts OpenAI usage, whose prompt count includes cache reads,
// into the canonical form, whose input count does not.
func (u *usage) canonical() core.Usage {
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return core.Usage{
		InputTokens:     max(u.PromptTokens-cached, 0),
		CacheReadTokens: cached,
		OutputTokens:    u.CompletionTokens,
	}
}

type apiError struct {
	Message string `json:"message"`
}

func (e *apiError) message() string {
	if e.Message == "" {
		return "unknown error"
	}
	return e.Message
}

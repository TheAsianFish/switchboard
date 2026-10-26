package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// chatRequest is the subset of the chat completions request body the gateway
// understands. Fields it refuses are decoded raw so their presence can be
// detected; any other field is ignored.
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	MaxTokens           *int            `json:"max_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	StreamOptions       *streamOptions  `json:"stream_options"`
	N                   *int            `json:"n"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	Functions           json.RawMessage `json:"functions"`
	FunctionCall        json.RawMessage `json:"function_call"`
	ResponseFormat      json.RawMessage `json:"response_format"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

type contentPart struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

// options are the parts of a request that change how the reply is rendered
// but have no place in core.Request.
type options struct {
	includeUsage bool
}

const errToolUse = "tool use is not supported yet"

// parseRequest turns a request body into a canonical request. Every error is
// a *core.Error of kind KindInvalidRequest with a client-safe message.
func parseRequest(body []byte) (*core.Request, options, error) {
	var in chatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, options{}, decodeError(err)
	}
	if in.Model == "" {
		return nil, options{}, invalid("model is required")
	}
	if len(in.Messages) == 0 {
		return nil, options{}, invalid("messages is required and must not be empty")
	}
	if present(in.Tools) || present(in.ToolChoice) || present(in.Functions) || present(in.FunctionCall) {
		return nil, options{}, invalid(errToolUse)
	}
	if in.N != nil && *in.N != 1 {
		return nil, options{}, invalid("n must be 1: only one choice per request is supported")
	}
	if err := checkResponseFormat(in.ResponseFormat); err != nil {
		return nil, options{}, err
	}

	req := &core.Request{
		Model:       in.Model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	switch {
	case in.MaxCompletionTokens != nil:
		if *in.MaxCompletionTokens < 0 {
			return nil, options{}, invalid("max_completion_tokens must not be negative")
		}
		req.MaxTokens = *in.MaxCompletionTokens
	case in.MaxTokens != nil:
		if *in.MaxTokens < 0 {
			return nil, options{}, invalid("max_tokens must not be negative")
		}
		req.MaxTokens = *in.MaxTokens
	}
	stop, err := parseStop(in.Stop)
	if err != nil {
		return nil, options{}, err
	}
	req.Stop = stop

	var system []string
	for i, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			if len(req.Messages) > 0 {
				return nil, options{}, invalid("messages[%d]: %s messages must come before all user and assistant messages", i, m.Role)
			}
			text, err := parseContent(m.Content, i)
			if err != nil {
				return nil, options{}, err
			}
			system = append(system, text)
		case "user", "assistant":
			if present(m.ToolCalls) || present(m.FunctionCall) {
				return nil, options{}, invalid("messages[%d]: %s", i, errToolUse)
			}
			text, err := parseContent(m.Content, i)
			if err != nil {
				return nil, options{}, err
			}
			req.Messages = append(req.Messages, core.Message{Role: core.Role(m.Role), Text: text})
		case "tool", "function":
			return nil, options{}, invalid("messages[%d]: role %q: %s", i, m.Role, errToolUse)
		case "":
			return nil, options{}, invalid("messages[%d].role is required", i)
		default:
			return nil, options{}, invalid("messages[%d]: unknown role %q", i, m.Role)
		}
	}
	if len(req.Messages) == 0 {
		return nil, options{}, invalid("messages must contain at least one user or assistant message")
	}
	req.System = strings.Join(system, "\n\n")

	var opts options
	if in.StreamOptions != nil {
		opts.includeUsage = in.StreamOptions.IncludeUsage
	}
	return req, opts, nil
}

// parseContent reads a message's content: a string or an array of text parts.
func parseContent(raw json.RawMessage, i int) (string, error) {
	if !present(raw) {
		return "", invalid("messages[%d].content is required and must not be null", i)
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", invalid("messages[%d].content is not a valid string", i)
		}
		return s, nil
	case '[':
		var parts []json.RawMessage
		if err := json.Unmarshal(raw, &parts); err != nil {
			return "", invalid("messages[%d].content must be a string or an array of content parts", i)
		}
		var b strings.Builder
		for j, p := range parts {
			var part contentPart
			if err := json.Unmarshal(p, &part); err != nil {
				return "", invalid("messages[%d].content[%d] must be an object with a type", i, j)
			}
			if part.Type != "text" {
				return "", invalid("messages[%d].content[%d]: only text content is supported", i, j)
			}
			if part.Text == nil {
				return "", invalid("messages[%d].content[%d].text is required", i, j)
			}
			b.WriteString(*part.Text)
		}
		return b.String(), nil
	default:
		return "", invalid("messages[%d].content must be a string or an array of content parts", i)
	}
}

// parseStop reads stop as a string or an array of strings.
func parseStop(raw json.RawMessage) ([]string, error) {
	if !present(raw) {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, invalid("stop must be a string or an array of strings")
		}
		return []string{s}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, invalid("stop must be a string or an array of strings")
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, nil
}

// checkResponseFormat accepts only the default plain text format.
func checkResponseFormat(raw json.RawMessage) error {
	if !present(raw) {
		return nil
	}
	var rf struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil || rf.Type != "text" {
		return invalid("response_format: only {\"type\": \"text\"} is supported")
	}
	return nil
}

// present reports whether a raw field was sent with a non-null value.
func present(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// decodeError turns a JSON decoding error into a client-safe message.
func decodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return core.Wrap(core.KindInvalidRequest, err, "field "+typeErr.Field+" has the wrong type: got JSON "+typeErr.Value)
	}
	return core.Wrap(core.KindInvalidRequest, err, "request body is not valid JSON")
}

func invalid(format string, args ...any) error {
	return core.Errorf(core.KindInvalidRequest, format, args...)
}

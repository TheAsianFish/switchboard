package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// wireRequest is the subset of a Messages request the gateway reads. Fields
// it does not model (top_k, metadata, service_tier, ...) are ignored.
type wireRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int            `json:"max_tokens"`
	Messages      []wireMessage   `json:"messages"`
	System        json.RawMessage `json:"system"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Tools         json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Thinking      json.RawMessage `json:"thinking"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// wireBlock is a content block; only text blocks are accepted. Other keys,
// cache_control among them, are ignored.
type wireBlock struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

func invalid(format string, args ...any) error {
	return core.Errorf(core.KindInvalidRequest, format, args...)
}

// parseRequest decodes and validates a Messages request body.
func parseRequest(body io.Reader) (*core.Request, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		// The caller tells *http.MaxBytesError apart through errors.As.
		return nil, core.Wrap(core.KindInvalidRequest, err, "could not read the request body")
	}
	var wr wireRequest
	if err := json.Unmarshal(raw, &wr); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return nil, invalid("%s: invalid type %s", typeErr.Field, typeErr.Value)
		}
		return nil, invalid("request body is not valid JSON")
	}
	if wr.Model == "" {
		return nil, invalid("model: field required")
	}
	if wr.MaxTokens == nil {
		return nil, invalid("max_tokens: field required")
	}
	if *wr.MaxTokens <= 0 {
		return nil, invalid("max_tokens: must be greater than 0")
	}
	if present(wr.Tools) && !emptyArray(wr.Tools) || present(wr.ToolChoice) {
		return nil, invalid("tool use is not supported yet")
	}
	if present(wr.Thinking) && !thinkingDisabled(wr.Thinking) {
		return nil, invalid("extended thinking is not supported yet")
	}
	if len(wr.Messages) == 0 {
		return nil, invalid("messages: at least one message is required")
	}
	req := &core.Request{
		Model:       wr.Model,
		MaxTokens:   *wr.MaxTokens,
		Temperature: wr.Temperature,
		TopP:        wr.TopP,
		Stream:      wr.Stream,
		Messages:    make([]core.Message, 0, len(wr.Messages)),
	}
	if len(wr.StopSequences) > 0 {
		req.Stop = wr.StopSequences
	}
	if present(wr.System) {
		req.System, err = textContent(wr.System, "system", "\n\n")
		if err != nil {
			return nil, err
		}
	}
	for i, m := range wr.Messages {
		var role core.Role
		switch m.Role {
		case "user":
			role = core.RoleUser
		case "assistant":
			role = core.RoleAssistant
		default:
			return nil, invalid("messages.%d.role: must be \"user\" or \"assistant\"", i)
		}
		if !present(m.Content) {
			return nil, invalid("messages.%d.content: field required", i)
		}
		text, err := textContent(m.Content, fmt.Sprintf("messages.%d.content", i), "")
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, core.Message{Role: role, Text: text})
	}
	return req, nil
}

// textContent reads a string or an array of text blocks, joining the blocks
// with sep.
func textContent(raw json.RawMessage, field, sep string) (string, error) {
	raw = bytes.TrimSpace(raw)
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", invalid("%s: invalid string", field)
		}
		return s, nil
	case '[':
		var blocks []wireBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", invalid("%s: must be a string or an array of content blocks", field)
		}
		parts := make([]string, 0, len(blocks))
		for j, b := range blocks {
			if b.Type != "text" {
				return "", invalid("%s.%d: only text content is supported", field, j)
			}
			if b.Text == nil {
				return "", invalid("%s.%d.text: field required", field, j)
			}
			parts = append(parts, *b.Text)
		}
		return strings.Join(parts, sep), nil
	default:
		return "", invalid("%s: must be a string or an array of content blocks", field)
	}
}

// present reports whether a raw field was sent with a non-null value.
func present(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func emptyArray(raw json.RawMessage) bool {
	var a []json.RawMessage
	return json.Unmarshal(raw, &a) == nil && len(a) == 0
}

// thinkingDisabled accepts {"type":"disabled"}, which asks for nothing the
// gateway lacks.
func thinkingDisabled(raw json.RawMessage) bool {
	var t struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &t) == nil && t.Type == "disabled"
}

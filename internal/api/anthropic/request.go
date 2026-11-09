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

// wireBlock is a content block: text, tool_use or tool_result. Other keys,
// cache_control among them, are ignored.
type wireBlock struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// wireTool is a client tool. Server tools (web search, code execution and
// the like, told apart by their type) are refused: the gateway cannot run
// them and no other provider could.
type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
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
	if req.Tools, err = parseTools(wr.Tools); err != nil {
		return nil, err
	}
	if req.ToolChoice, err = parseToolChoice(wr.ToolChoice); err != nil {
		return nil, err
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
		msg, err := messageContent(m.Content, role, fmt.Sprintf("messages.%d.content", i))
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, msg)
	}
	return req, nil
}

// parseTools reads tools; absent or empty means none.
func parseTools(raw json.RawMessage) ([]core.Tool, error) {
	if !present(raw) {
		return nil, nil
	}
	var in []wireTool
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, invalid("tools: must be an array of tools")
	}
	if len(in) == 0 {
		return nil, nil
	}
	tools := make([]core.Tool, 0, len(in))
	for i, t := range in {
		if t.Type != "" && t.Type != "custom" {
			return nil, invalid("tools.%d: server tools (type %q) are not supported", i, t.Type)
		}
		if t.Name == "" {
			return nil, invalid("tools.%d.name: field required", i)
		}
		if !present(t.InputSchema) {
			return nil, invalid("tools.%d.input_schema: field required", i)
		}
		schema, ok := compactObject(t.InputSchema)
		if !ok {
			return nil, invalid("tools.%d.input_schema: must be an object", i)
		}
		tools = append(tools, core.Tool{Name: t.Name, Description: t.Description, Parameters: schema})
	}
	return tools, nil
}

// parseToolChoice reads tool_choice; absent means the zero choice.
func parseToolChoice(raw json.RawMessage) (core.ToolChoice, error) {
	if !present(raw) {
		return core.ToolChoice{}, nil
	}
	var in wireToolChoice
	if err := json.Unmarshal(raw, &in); err != nil {
		return core.ToolChoice{}, invalid("tool_choice: must be an object")
	}
	tc := core.ToolChoice{DisableParallel: in.DisableParallelToolUse}
	switch in.Type {
	case "auto":
		tc.Mode = core.ToolChoiceAuto
	case "any":
		tc.Mode = core.ToolChoiceAny
	case "none":
		tc.Mode = core.ToolChoiceNone
	case "tool":
		if in.Name == "" {
			return core.ToolChoice{}, invalid("tool_choice.name: field required")
		}
		tc.Mode, tc.Name = core.ToolChoiceTool, in.Name
	default:
		return core.ToolChoice{}, invalid("tool_choice.type: must be one of \"auto\", \"any\", \"tool\" or \"none\"")
	}
	return tc, nil
}

// messageContent reads a message's content: a string, or text blocks plus
// tool_use blocks in an assistant turn or tool_result blocks in a user
// turn. Text blocks are joined; see core.Message for the order kept.
func messageContent(raw json.RawMessage, role core.Role, field string) (core.Message, error) {
	msg := core.Message{Role: role}
	raw = bytes.TrimSpace(raw)
	if raw[0] != '[' {
		text, err := textContent(raw, field, "")
		msg.Text = text
		return msg, err
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return msg, invalid("%s: must be a string or an array of content blocks", field)
	}
	var text strings.Builder
	for j, b := range blocks {
		at := fmt.Sprintf("%s.%d", field, j)
		switch {
		case b.Type == "text":
			if b.Text == nil {
				return msg, invalid("%s.text: field required", at)
			}
			text.WriteString(*b.Text)
		case b.Type == "tool_use" && role == core.RoleAssistant:
			call, err := toolUse(b, at)
			if err != nil {
				return msg, err
			}
			msg.ToolCalls = append(msg.ToolCalls, call)
		case b.Type == "tool_result" && role == core.RoleUser:
			res, err := toolResult(b, at)
			if err != nil {
				return msg, err
			}
			msg.ToolResults = append(msg.ToolResults, res)
		case b.Type == "tool_use" || b.Type == "tool_result":
			return msg, invalid("%s: %s blocks are not allowed in %s messages", at, b.Type, role)
		default:
			return msg, invalid("%s: only text, tool_use and tool_result content is supported", at)
		}
	}
	msg.Text = text.String()
	return msg, nil
}

func toolUse(b wireBlock, at string) (core.ToolCall, error) {
	if b.ID == "" {
		return core.ToolCall{}, invalid("%s.id: field required", at)
	}
	if b.Name == "" {
		return core.ToolCall{}, invalid("%s.name: field required", at)
	}
	if !present(b.Input) {
		return core.ToolCall{}, invalid("%s.input: field required", at)
	}
	input, ok := compactObject(b.Input)
	if !ok {
		return core.ToolCall{}, invalid("%s.input: must be an object", at)
	}
	return core.ToolCall{ID: b.ID, Name: b.Name, Arguments: input}, nil
}

// toolResult reads a tool_result block, whose content is absent, a string
// or an array of text blocks (images are not modelled).
func toolResult(b wireBlock, at string) (core.ToolResult, error) {
	if b.ToolUseID == "" {
		return core.ToolResult{}, invalid("%s.tool_use_id: field required", at)
	}
	res := core.ToolResult{CallID: b.ToolUseID, IsError: b.IsError}
	if present(b.Content) {
		text, err := textContent(b.Content, at+".content", "")
		if err != nil {
			return core.ToolResult{}, err
		}
		res.Content = text
	}
	return res, nil
}

// compactObject reports whether raw is a JSON object and returns it
// compacted.
func compactObject(raw json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return nil, false
	}
	return b.Bytes(), true
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

// thinkingDisabled accepts {"type":"disabled"}, which asks for nothing the
// gateway lacks.
func thinkingDisabled(raw json.RawMessage) bool {
	var t struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &t) == nil && t.Type == "disabled"
}

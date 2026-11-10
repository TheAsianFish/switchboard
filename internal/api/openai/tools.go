package openai

import (
	"bytes"
	"encoding/json"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// wireTool is one entry of tools. Only function tools are modelled; the
// strict flag is accepted and not passed on, since the Anthropic format
// has no equivalent and an OpenAI-compatible upstream may not support it.
type wireTool struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// wireToolCall is a tool call in an assistant message, in a request or a
// non-streamed reply.
type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// parseTools reads tools; an absent or empty list means none.
func parseTools(raw json.RawMessage) ([]core.Tool, error) {
	if !present(raw) {
		return nil, nil
	}
	var in []wireTool
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, invalid("tools must be an array of tool objects")
	}
	if len(in) == 0 {
		return nil, nil
	}
	tools := make([]core.Tool, 0, len(in))
	for i, t := range in {
		if t.Type != "function" {
			return nil, invalid("tools[%d]: only function tools are supported", i)
		}
		if t.Function == nil || t.Function.Name == "" {
			return nil, invalid("tools[%d].function.name is required", i)
		}
		tool := core.Tool{Name: t.Function.Name, Description: t.Function.Description}
		if present(t.Function.Parameters) {
			params, err := compactObject(t.Function.Parameters)
			if err != nil {
				return nil, invalid("tools[%d].function.parameters must be a JSON schema object", i)
			}
			tool.Parameters = params
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// parseToolChoice reads tool_choice ("none", "auto", "required" or a named
// function) and parallel_tool_calls, whose false is the one value that
// asks for something.
func parseToolChoice(raw json.RawMessage, parallel *bool) (core.ToolChoice, error) {
	var tc core.ToolChoice
	if parallel != nil && !*parallel {
		tc.DisableParallel = true
	}
	if !present(raw) {
		return tc, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none":
			tc.Mode = core.ToolChoiceNone
		case "auto":
			tc.Mode = core.ToolChoiceAuto
		case "required":
			tc.Mode = core.ToolChoiceAny
		default:
			return tc, invalid("tool_choice must be \"none\", \"auto\", \"required\" or a function")
		}
		return tc, nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" {
		return tc, invalid("tool_choice must be \"none\", \"auto\", \"required\" or a function")
	}
	if obj.Function.Name == "" {
		return tc, invalid("tool_choice.function.name is required")
	}
	tc.Mode, tc.Name = core.ToolChoiceTool, obj.Function.Name
	return tc, nil
}

// parseAssistant reads an assistant message: text, tool calls or both. Its
// content may be null only when it has tool calls.
func parseAssistant(m chatMessage, i int) (core.Message, error) {
	msg := core.Message{Role: core.RoleAssistant}
	if present(m.ToolCalls) {
		var calls []wireToolCall
		if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
			return msg, invalid("messages[%d].tool_calls must be an array of tool calls", i)
		}
		for j, c := range calls {
			if c.Type != "" && c.Type != "function" {
				return msg, invalid("messages[%d].tool_calls[%d]: only function calls are supported", i, j)
			}
			if c.ID == "" {
				return msg, invalid("messages[%d].tool_calls[%d].id is required", i, j)
			}
			if c.Function.Name == "" {
				return msg, invalid("messages[%d].tool_calls[%d].function.name is required", i, j)
			}
			call := core.ToolCall{ID: c.ID, Name: c.Function.Name}
			if c.Function.Arguments != "" {
				if !json.Valid([]byte(c.Function.Arguments)) {
					return msg, invalid("messages[%d].tool_calls[%d].function.arguments is not valid JSON", i, j)
				}
				call.Arguments = json.RawMessage(c.Function.Arguments)
			}
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	if len(msg.ToolCalls) > 0 && !present(m.Content) {
		return msg, nil
	}
	text, err := parseContent(m.Content, i)
	if err != nil {
		return msg, err
	}
	msg.Text = text
	return msg, nil
}

// parseToolResult reads a "tool" message.
func parseToolResult(m chatMessage, i int) (core.ToolResult, error) {
	if m.ToolCallID == "" {
		return core.ToolResult{}, invalid("messages[%d].tool_call_id is required", i)
	}
	text, err := parseContent(m.Content, i)
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{CallID: m.ToolCallID, Content: text}, nil
}

// isToolTurn reports whether m was built from tool messages alone.
func isToolTurn(m core.Message) bool {
	return m.Role == core.RoleUser && len(m.ToolResults) > 0 && m.Text == ""
}

// compactObject checks that raw is a JSON object and returns it compacted.
func compactObject(raw json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, invalid("not a JSON object")
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, invalid("not a JSON object")
	}
	return b.Bytes(), nil
}

// toolCallID returns the client-facing id of a tool call, making one up
// when the provider sent none.
func toolCallID(id string) string {
	if id != "" {
		return id
	}
	return "call_" + randomHex()
}

// toolCalls renders tool calls for a non-streamed reply. Calls without
// arguments get "{}", as OpenAI sends for a function with no parameters.
func toolCalls(calls []core.ToolCall) []wireToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]wireToolCall, len(calls))
	for i, c := range calls {
		args := string(c.Arguments)
		if args == "" {
			args = "{}"
		}
		out[i] = wireToolCall{ID: toolCallID(c.ID), Type: "function", Function: wireFunction{Name: c.Name, Arguments: args}}
	}
	return out
}

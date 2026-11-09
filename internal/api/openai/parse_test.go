package openai

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
)

func ptr[T any](v T) *T { return &v }

func TestParseRequestValid(t *testing.T) {
	t.Parallel()
	user := func(text string) core.Message { return core.Message{Role: core.RoleUser, Text: text} }
	tests := []struct {
		name  string
		body  string
		want  core.Request
		usage bool
	}{
		{
			name: "minimal",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}},
		},
		{
			name: "system and developer joined",
			body: `{"model":"m","messages":[{"role":"system","content":"a"},{"role":"developer","content":[{"type":"text","text":"b"}]},{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", System: "a\n\nb", Messages: []core.Message{user("hi")}},
		},
		{
			name: "text parts joined",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"he"},{"type":"text","text":"llo"}]}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hello")}},
		},
		{
			name: "conversation",
			body: `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":""}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("q"), {Role: core.RoleAssistant, Text: "a"}, user("")}},
		},
		{
			name: "max_completion_tokens preferred",
			body: `{"model":"m","max_tokens":5,"max_completion_tokens":9,"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", MaxTokens: 9, Messages: []core.Message{user("hi")}},
		},
		{
			name: "max_tokens",
			body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", MaxTokens: 5, Messages: []core.Message{user("hi")}},
		},
		{
			name: "sampling and stop string",
			body: `{"model":"m","temperature":0.5,"top_p":0.9,"stop":"END","messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Temperature: ptr(0.5), TopP: ptr(0.9), Stop: []string{"END"}, Messages: []core.Message{user("hi")}},
		},
		{
			name: "zero temperature kept",
			body: `{"model":"m","temperature":0,"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Temperature: ptr(0.0), Messages: []core.Message{user("hi")}},
		},
		{
			name: "stop array",
			body: `{"model":"m","stop":["a","b"],"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Stop: []string{"a", "b"}, Messages: []core.Message{user("hi")}},
		},
		{
			name: "empty stop array and nulls",
			body: `{"model":"m","stop":[],"tools":null,"n":null,"response_format":null,"temperature":null,"messages":[{"role":"user","content":"hi","tool_calls":null}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}},
		},
		{
			name:  "stream with usage",
			body:  `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
			want:  core.Request{Model: "m", Stream: true, Messages: []core.Message{user("hi")}},
			usage: true,
		},
		{
			name: "stream without usage",
			body: `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Stream: true, Messages: []core.Message{user("hi")}},
		},
		{
			name: "n=1, text format and unknown fields",
			body: `{"model":"m","n":1,"response_format":{"type":"text"},"user":"x","seed":4,"logit_bias":{},"messages":[{"role":"user","content":"hi","name":"bob"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}},
		},
		{
			name: "unicode",
			body: `{"model":"m","messages":[{"role":"user","content":"日本語   😀\n"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("日本語   😀\n")}},
		},
		{
			name: "tools compacted, strict ignored, parameters optional",
			body: `{"model":"m","tools":[{"type":"function","function":{"name":"f","description":"d","strict":true,"parameters":{ "type": "object" }}},{"type":"function","function":{"name":"g"}}],"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}, Tools: []core.Tool{
				{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)},
				{Name: "g"},
			}},
		},
		{
			name: "empty tools",
			body: `{"model":"m","tools":[],"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}},
		},
		{
			name: "tool_choice required, parallel off",
			body: `{"model":"m","tool_choice":"required","parallel_tool_calls":false,"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceAny, DisableParallel: true}},
		},
		{
			name: "tool_choice none, parallel on",
			body: `{"model":"m","tool_choice":"none","parallel_tool_calls":true,"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceNone}},
		},
		{
			name: "tool_choice auto",
			body: `{"model":"m","tool_choice":"auto","messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceAuto}},
		},
		{
			name: "tool_choice named function",
			body: `{"model":"m","tool_choice":{"type":"function","function":{"name":"f"}},"messages":[{"role":"user","content":"hi"}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{user("hi")}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceTool, Name: "f"}},
		},
		{
			name: "tool calls and results",
			body: `{"model":"m","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{\"x\": 1}"}},{"id":"b","type":"function","function":{"name":"g","arguments":""}}]},` +
				`{"role":"tool","tool_call_id":"a","content":"one"},` +
				`{"role":"tool","tool_call_id":"b","content":[{"type":"text","text":"tw"},{"type":"text","text":"o"}]},` +
				`{"role":"user","content":"and?"},` +
				`{"role":"assistant","content":"sure","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"c","content":""}]}`,
			want: core.Request{Model: "m", Messages: []core.Message{
				user("q"),
				{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "a", Name: "f", Arguments: json.RawMessage(`{"x": 1}`)}, {ID: "b", Name: "g"}}},
				{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "a", Content: "one"}, {CallID: "b", Content: "two"}}},
				user("and?"),
				{Role: core.RoleAssistant, Text: "sure", ToolCalls: []core.ToolCall{{ID: "c", Name: "f", Arguments: json.RawMessage(`{}`)}}},
				{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "c"}}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, opts, err := parseRequest([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("got %+v\nwant %+v", *got, tt.want)
			}
			if opts.includeUsage != tt.usage {
				t.Errorf("includeUsage = %v, want %v", opts.includeUsage, tt.usage)
			}
		})
	}
}

func TestParseRequestInvalid(t *testing.T) {
	t.Parallel()
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	tests := []struct {
		name string
		body string
		want string // substring of the client message
	}{
		{"not json", `{"model":`, "not valid JSON"},
		{"trailing garbage", `{"model":"m",` + msgs + `} x`, "not valid JSON"},
		{"wrong type", `{"model":5,` + msgs + `}`, "model has the wrong type"},
		{"missing model", `{` + msgs + `}`, "model is required"},
		{"missing messages", `{"model":"m"}`, "messages is required"},
		{"empty messages", `{"model":"m","messages":[]}`, "messages is required"},
		{"only system", `{"model":"m","messages":[{"role":"system","content":"s"}]}`, "at least one user or assistant"},
		{"late system", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"system","content":"s"}]}`, "must come before"},
		{"late developer", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"developer","content":"s"}]}`, "must come before"},
		{"image part", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "only text content is supported"},
		{"audio part after text", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"input_audio"}]}]}`, "only text content is supported"},
		{"text part without text", `{"model":"m","messages":[{"role":"user","content":[{"type":"text"}]}]}`, "text is required"},
		{"numeric content", `{"model":"m","messages":[{"role":"user","content":5}]}`, "must be a string or an array"},
		{"missing content", `{"model":"m","messages":[{"role":"user"}]}`, "content is required"},
		{"assistant null content", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null}]}`, "content is required"},
		{"tool call without name", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"1"}]}]}`, "function.name is required"},
		{"tool call without id", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f"}}]}]}`, "tool_calls[0].id is required"},
		{"tool call bad arguments", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{x"}}]}]}`, "arguments is not valid JSON"},
		{"custom tool call", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"custom","custom":{"name":"f"}}]}]}`, "only function calls"},
		{"tool calls not an array", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":{}}]}`, "must be an array"},
		{"user tool calls", `{"model":"m","messages":[{"role":"user","content":"q","tool_calls":[{"id":"1","type":"function","function":{"name":"f"}}]}]}`, "only assistant messages carry tool calls"},
		{"assistant function call", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"","function_call":{"name":"f"}}]}`, "legacy functions API"},
		{"tool without call id", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"tool","content":"r"}]}`, "tool_call_id is required"},
		{"tool without content", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"tool","tool_call_id":"1"}]}`, "content is required"},
		{"tool image content", `{"model":"m","messages":[{"role":"tool","tool_call_id":"1","content":[{"type":"image_url"}]}]}`, "only text content is supported"},
		{"function role", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"function","content":"r"}]}`, "legacy functions API"},
		{"unknown role", `{"model":"m","messages":[{"role":"wizard","content":"q"}]}`, "unknown role"},
		{"missing role", `{"model":"m","messages":[{"content":"q"}]}`, "role is required"},
		{"tool without function", `{"model":"m","tools":[{"type":"function"}],` + msgs + `}`, "tools[0].function.name is required"},
		{"custom tool", `{"model":"m","tools":[{"type":"custom","custom":{"name":"f"}}],` + msgs + `}`, "only function tools"},
		{"tools not an array", `{"model":"m","tools":{},` + msgs + `}`, "tools must be an array"},
		{"parameters not an object", `{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":[1]}}],` + msgs + `}`, "parameters must be a JSON schema object"},
		{"unknown tool_choice", `{"model":"m","tool_choice":"sometimes",` + msgs + `}`, "tool_choice must be"},
		{"allowed_tools choice", `{"model":"m","tool_choice":{"type":"allowed_tools","allowed_tools":{}},` + msgs + `}`, "tool_choice must be"},
		{"function choice without name", `{"model":"m","tool_choice":{"type":"function","function":{}},` + msgs + `}`, "tool_choice.function.name is required"},
		{"functions", `{"model":"m","functions":[],` + msgs + `}`, "legacy functions API"},
		{"function_call", `{"model":"m","function_call":"auto",` + msgs + `}`, "legacy functions API"},
		{"n=2", `{"model":"m","n":2,` + msgs + `}`, "n must be 1"},
		{"n=0", `{"model":"m","n":0,` + msgs + `}`, "n must be 1"},
		{"json_object format", `{"model":"m","response_format":{"type":"json_object"},` + msgs + `}`, "response_format"},
		{"json_schema format", `{"model":"m","response_format":{"type":"json_schema","json_schema":{}},` + msgs + `}`, "response_format"},
		{"bad stop", `{"model":"m","stop":5,` + msgs + `}`, "stop must be"},
		{"bad stop element", `{"model":"m","stop":["a",1],` + msgs + `}`, "stop must be"},
		{"negative max_tokens", `{"model":"m","max_tokens":-1,` + msgs + `}`, "max_tokens must not be negative"},
		{"negative max_completion_tokens", `{"model":"m","max_completion_tokens":-1,` + msgs + `}`, "max_completion_tokens must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseRequest([]byte(tt.body))
			var e *core.Error
			if !errors.As(err, &e) || e.Kind != core.KindInvalidRequest {
				t.Fatalf("err = %v, want an invalid-request error", err)
			}
			if !strings.Contains(e.Message, tt.want) {
				t.Errorf("message %q does not contain %q", e.Message, tt.want)
			}
		})
	}
}

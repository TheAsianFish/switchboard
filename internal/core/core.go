// Package core holds the canonical request, response and stream event types.
//
// Front ends parse their wire format into these types and providers translate
// them into their upstream's wire format. Nothing in this package knows about
// JSON field names of either API.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// Role is the author of a message in a conversation.
type Role string

// Roles a message can have.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of a conversation. Text, tool calls and tool results
// are modelled; front ends refuse anything else (images, documents,
// thinking blocks) with an invalid-request error.
//
// The order of content within a turn is fixed rather than kept: a user
// turn is its tool results, then its text; an assistant turn is its text,
// then its tool calls. Both wire formats put content in that order in
// practice, and the Anthropic API requires it for tool results.
type Message struct {
	Role Role
	// Text is the turn's text, every text part joined.
	Text string
	// ToolCalls are the tools an assistant turn called.
	ToolCalls []ToolCall
	// ToolResults answer the previous assistant turn's tool calls; only a
	// user turn carries them. The OpenAI format sends each one as its own
	// "tool" message; its front end folds consecutive ones into one turn.
	ToolResults []ToolResult
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	// Parameters is the JSON schema of the arguments, compacted; nil means
	// the client sent none (only the OpenAI format allows that).
	Parameters json.RawMessage
}

// ToolChoiceMode says whether and which tool the model must call.
type ToolChoiceMode string

// Tool choice modes. The zero value means the client did not say, which
// every provider reads as auto when tools are given.
const (
	ToolChoiceAuto ToolChoiceMode = "auto"
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceAny requires some tool call: Anthropic's "any", OpenAI's
	// "required".
	ToolChoiceAny ToolChoiceMode = "any"
	// ToolChoiceTool requires a call to the tool named in ToolChoice.Name.
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice constrains tool use. The zero value constrains nothing.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is the tool to call when Mode is ToolChoiceTool.
	Name string
	// DisableParallel asks for at most one tool call per turn: Anthropic's
	// disable_parallel_tool_use, OpenAI's parallel_tool_calls false.
	DisableParallel bool
}

// ToolCall is one call the model made.
type ToolCall struct {
	// ID is the provider's id for the call; the client echoes it in the
	// matching ToolResult.
	ID   string
	Name string
	// Arguments is the JSON text of the arguments; empty means none ({}).
	// Front ends check that a client's arguments are valid JSON; a
	// provider's are passed on as the model wrote them.
	Arguments json.RawMessage
}

// ToolResult is the outcome of a tool call, sent back by the client.
type ToolResult struct {
	// CallID is the ToolCall.ID this result answers.
	CallID string
	// Content is the result as text, every text part joined.
	Content string
	// IsError marks a failed call. Only the Anthropic format can say so;
	// it is dropped on the way to an OpenAI-format upstream.
	IsError bool
}

// Request is a chat request in canonical form.
type Request struct {
	// Model is the name the client asked for, as configured in the gateway.
	Model string
	// System is the system prompt; empty means none.
	System   string
	Messages []Message
	// MaxTokens is the output limit; 0 means the provider's default.
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stop        []string
	Stream      bool
	// Tools the model may call; nil means none.
	Tools      []Tool
	ToolChoice ToolChoice
}

// HasToolContent reports whether the request carries tools or a tool
// choice, or tool calls or results anywhere in its conversation.
func (r *Request) HasToolContent() bool {
	if len(r.Tools) > 0 || r.ToolChoice != (ToolChoice{}) {
		return true
	}
	for _, m := range r.Messages {
		if len(m.ToolCalls) > 0 || len(m.ToolResults) > 0 {
			return true
		}
	}
	return false
}

// StopReason says why the model stopped generating.
type StopReason string

// Canonical stop reasons. Each front end and provider maps its own wire
// format's; the whole table:
//
//	canonical       Anthropic stop_reason              OpenAI finish_reason
//	end_turn        end_turn, pause_turn (in)          stop
//	max_tokens      max_tokens,                        length
//	                model_context_window_exceeded (in)
//	stop_sequence   stop_sequence                      stop (out only)
//	tool_use        tool_use                           tool_calls, function_call (in)
//	content_filter  refusal                            content_filter
//
// "(in)" values are only read from an upstream, never written. OpenAI's
// "stop" does not tell a stop sequence from a natural end, so it reads
// back as end_turn. pause_turn belongs to server-side tools, which the
// gateway refuses, so reaching it means the turn ended with nothing left
// to continue. Anything unknown reads as end_turn.
const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopSequence      StopReason = "stop_sequence"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
)

// Usage counts tokens as reported by the provider. InputTokens excludes
// prompt-cache reads and writes (the Anthropic convention); the OpenAI
// convention of one prompt total including cached tokens is
// InputTokens + CacheReadTokens + CacheWriteTokens.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// PromptTokens is the total prompt size, cached or not.
func (u Usage) PromptTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Response is a complete, non-streamed reply.
type Response struct {
	ID    string
	Model string
	Text  string
	// ToolCalls are the calls the model made, in order.
	ToolCalls  []ToolCall
	StopReason StopReason
	Usage      Usage
}

// EventKind is the type of a stream event.
type EventKind int

const (
	// EventStart opens a stream. ID and Model are set; Usage may carry the
	// input token count when the provider reports it up front.
	EventStart EventKind = iota + 1
	// EventDelta carries a piece of generated text.
	EventDelta
	// EventStop closes the stream. StopReason and the final Usage are set.
	EventStop
	// EventToolCallStart opens a tool call. ToolIndex, ToolCallID and
	// ToolName are set.
	EventToolCallStart
	// EventToolCallDelta carries a piece of the open tool call's arguments
	// JSON in Arguments; ToolIndex repeats the call's index.
	EventToolCallDelta
)

func (k EventKind) String() string {
	switch k {
	case EventStart:
		return "start"
	case EventDelta:
		return "delta"
	case EventStop:
		return "stop"
	case EventToolCallStart:
		return "tool_call_start"
	case EventToolCallDelta:
		return "tool_call_delta"
	default:
		return "unknown"
	}
}

// Event is one item of a canonical stream. A well-formed stream is exactly
// one EventStart, then any number of EventDelta, EventToolCallStart and
// EventToolCallDelta, then exactly one EventStop.
//
// Tool calls are sequential, as both wire formats stream them: the first
// EventToolCallStart has ToolIndex 0 and each next one the index after,
// and an EventToolCallDelta belongs to the call opened last. Text deltas
// may come before, between or after tool calls.
type Event struct {
	Kind       EventKind
	ID         string
	Model      string
	Text       string
	StopReason StopReason
	Usage      Usage

	// ToolIndex is a tool call's position in the reply, from 0.
	ToolIndex  int
	ToolCallID string
	ToolName   string
	// Arguments is a fragment of a call's arguments JSON; the fragments of
	// one call joined are its ToolCall.Arguments.
	Arguments string
}

// Stream yields canonical events. Next returns io.EOF after the EventStop
// event has been returned. Close releases the upstream connection and may be
// called at any time, including before the stream is drained.
type Stream interface {
	Next() (Event, error)
	Close() error
}

// Provider is a backend that serves chat requests. The request's Model has
// already been resolved to the provider's own model name.
type Provider interface {
	Complete(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request) (Stream, error)
}

// Collect drains a stream into a Response. It is used where a caller needs a
// complete reply from a provider that only streams, and in tests. A stream
// whose tool calls break the sequence rule is an upstream error.
func Collect(s Stream) (*Response, error) {
	defer func() { _ = s.Close() }()
	var resp Response
	var text []byte
	var args [][]byte
	sawStop := false
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch ev.Kind {
		case EventStart:
			resp.ID, resp.Model = ev.ID, ev.Model
			resp.Usage = ev.Usage
		case EventDelta:
			text = append(text, ev.Text...)
		case EventToolCallStart:
			if ev.ToolIndex != len(resp.ToolCalls) {
				return nil, Errorf(KindUpstream, "stream opened tool call %d after %d calls", ev.ToolIndex, len(resp.ToolCalls))
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: ev.ToolCallID, Name: ev.ToolName})
			args = append(args, nil)
		case EventToolCallDelta:
			if ev.ToolIndex != len(resp.ToolCalls)-1 {
				return nil, Errorf(KindUpstream, "stream sent arguments for tool call %d, which is not open", ev.ToolIndex)
			}
			args[ev.ToolIndex] = append(args[ev.ToolIndex], ev.Arguments...)
		case EventStop:
			resp.StopReason = ev.StopReason
			resp.Usage = mergeUsage(resp.Usage, ev.Usage)
			sawStop = true
		}
	}
	if !sawStop {
		return nil, Errorf(KindUpstream, "stream ended without a stop event")
	}
	resp.Text = string(text)
	for i, a := range args {
		if len(a) > 0 {
			resp.ToolCalls[i].Arguments = a
		}
	}
	return &resp, nil
}

// mergeUsage keeps the input count from the start event when the stop event
// does not repeat it.
func mergeUsage(start, stop Usage) Usage {
	if stop.PromptTokens() == 0 {
		stop.InputTokens = start.InputTokens
		stop.CacheReadTokens = start.CacheReadTokens
		stop.CacheWriteTokens = start.CacheWriteTokens
	}
	return stop
}

// SliceStream is a Stream over a fixed list of events, used by tests and by
// providers that already hold the whole reply.
type SliceStream struct {
	Events []Event
	i      int
	closed bool
}

// Next implements Stream.
func (s *SliceStream) Next() (Event, error) {
	if s.closed || s.i >= len(s.Events) {
		return Event{}, io.EOF
	}
	ev := s.Events[s.i]
	s.i++
	return ev, nil
}

// Close implements Stream.
func (s *SliceStream) Close() error {
	s.closed = true
	return nil
}

// ResponseEvents renders a complete response as the stream that would have
// produced it: one delta per chunk of text, then each tool call as a start
// and, when it has arguments, one delta carrying all of them.
func ResponseEvents(r *Response, chunks []string) []Event {
	evs := make([]Event, 0, len(chunks)+2*len(r.ToolCalls)+2)
	evs = append(evs, Event{Kind: EventStart, ID: r.ID, Model: r.Model, Usage: Usage{InputTokens: r.Usage.InputTokens, CacheReadTokens: r.Usage.CacheReadTokens, CacheWriteTokens: r.Usage.CacheWriteTokens}})
	for _, c := range chunks {
		evs = append(evs, Event{Kind: EventDelta, Text: c})
	}
	for i, tc := range r.ToolCalls {
		evs = append(evs, Event{Kind: EventToolCallStart, ToolIndex: i, ToolCallID: tc.ID, ToolName: tc.Name})
		if len(tc.Arguments) > 0 {
			evs = append(evs, Event{Kind: EventToolCallDelta, ToolIndex: i, Arguments: string(tc.Arguments)})
		}
	}
	evs = append(evs, Event{Kind: EventStop, StopReason: r.StopReason, Usage: r.Usage})
	return evs
}

// Gateway is what a front end calls once it has parsed a request. The
// pipeline implements it for `serve`; a thin adapter without authentication
// implements it for `fake-provider`.
type Gateway interface {
	// Authenticate resolves a client API key. The returned context carries
	// the caller's identity and must be passed to Complete and Stream.
	Authenticate(ctx context.Context, apiKey string) (context.Context, error)
	Complete(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request) (Stream, error)
	// Models lists the model names the caller may use.
	Models(ctx context.Context) []string
}

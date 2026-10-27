// Package core holds the canonical request, response and stream event types.
//
// Front ends parse their wire format into these types and providers translate
// them into their upstream's wire format. Nothing in this package knows about
// JSON field names of either API.
package core

import (
	"context"
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

// Message is one turn of a conversation. Only text content is modelled so
// far; front ends refuse anything else with an invalid-request error.
type Message struct {
	Role Role
	Text string
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
}

// StopReason says why the model stopped generating.
type StopReason string

// Canonical stop reasons.
const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopSequence      StopReason = "stop_sequence"
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
	ID         string
	Model      string
	Text       string
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
)

func (k EventKind) String() string {
	switch k {
	case EventStart:
		return "start"
	case EventDelta:
		return "delta"
	case EventStop:
		return "stop"
	default:
		return "unknown"
	}
}

// Event is one item of a canonical stream. A well-formed stream is exactly
// one EventStart, any number of EventDelta and exactly one EventStop.
type Event struct {
	Kind       EventKind
	ID         string
	Model      string
	Text       string
	StopReason StopReason
	Usage      Usage
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
// complete reply from a provider that only streams, and in tests.
func Collect(s Stream) (*Response, error) {
	defer func() { _ = s.Close() }()
	var resp Response
	var text []byte
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
// produced it, one delta per chunk of text.
func ResponseEvents(r *Response, chunks []string) []Event {
	evs := make([]Event, 0, len(chunks)+2)
	evs = append(evs, Event{Kind: EventStart, ID: r.ID, Model: r.Model, Usage: Usage{InputTokens: r.Usage.InputTokens, CacheReadTokens: r.Usage.CacheReadTokens, CacheWriteTokens: r.Usage.CacheWriteTokens}})
	for _, c := range chunks {
		evs = append(evs, Event{Kind: EventDelta, Text: c})
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

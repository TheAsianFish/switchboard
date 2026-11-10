package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/sse"
)

// stream translates an upstream SSE body into canonical events. Next must
// not be called concurrently; Close may be called from any goroutine.
type stream struct {
	ctx    context.Context
	r      *sse.Reader
	closer func()

	started bool
	finish  string // last finish_reason seen; "" while generating
	// calls maps an upstream tool call index to the canonical one, in the
	// order the calls opened; the last is the open call.
	calls []int
	usage core.Usage
	queue []core.Event
	err   error // sticky: io.EOF once stopped, or the failure
}

func newStream(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser) *stream {
	var once sync.Once
	return &stream{
		ctx: ctx,
		r:   sse.NewReader(body),
		closer: func() {
			once.Do(func() {
				_ = body.Close()
				cancel()
			})
		},
	}
}

// Next implements core.Stream.
func (s *stream) Next() (core.Event, error) {
	for {
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, nil
		}
		if s.err != nil {
			return core.Event{}, s.err
		}
		s.read()
	}
}

// read consumes one upstream event, queueing canonical events or setting
// s.err.
func (s *stream) read() {
	ev, err := s.r.Next()
	switch {
	case errors.Is(err, io.EOF):
		s.end()
		return
	case err != nil:
		s.fail(transportError(s.ctx, err))
		return
	}
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		s.end()
		return
	}
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		s.fail(core.Wrap(core.KindUpstream, err, "upstream: invalid stream chunk"))
		return
	}
	if c.Error != nil {
		s.fail(core.Errorf(core.KindUpstream, "upstream: %s", c.Error.message()))
		return
	}
	if !s.started {
		s.started = true
		s.queue = append(s.queue, core.Event{Kind: core.EventStart, ID: c.ID, Model: c.Model})
	}
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			s.queue = append(s.queue, core.Event{Kind: core.EventDelta, Text: *ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if err := s.toolCall(tc); err != nil {
				s.fail(err)
				return
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finish = *ch.FinishReason
		}
	}
	if c.Usage != nil {
		s.usage = c.Usage.canonical()
	}
}

// toolCall queues the events for one delta.tool_calls entry. A new index
// (or, without an index, a new id) opens a call; anything else adds to the
// open call. Servers stream calls one after another, which is what the
// canonical stream allows: a fragment for a call already closed is an
// upstream error rather than something to reorder.
func (s *stream) toolCall(tc toolCall) error {
	n := len(s.calls)
	idx := n - 1
	switch {
	case tc.Index != nil:
		idx = *tc.Index
	case tc.ID != "" || n == 0:
		idx = n // no index: count each id as the next call
	}
	open := n > 0 && s.calls[n-1] == idx
	if !open {
		for _, u := range s.calls {
			if u == idx {
				return core.Errorf(core.KindUpstream, "upstream: tool call %d continued after the next one started", idx)
			}
		}
		s.calls = append(s.calls, idx)
		s.queue = append(s.queue, core.Event{Kind: core.EventToolCallStart, ToolIndex: n, ToolCallID: tc.ID, ToolName: tc.Function.Name})
	}
	if tc.Function.Arguments != "" {
		s.queue = append(s.queue, core.Event{Kind: core.EventToolCallDelta, ToolIndex: len(s.calls) - 1, Arguments: tc.Function.Arguments})
	}
	return nil
}

// end handles [DONE] or the end of the body: a stream that saw a
// finish_reason stops cleanly, any other was cut short.
func (s *stream) end() {
	if s.finish == "" {
		s.fail(core.Errorf(core.KindUpstream, "upstream stream ended early"))
		return
	}
	s.queue = append(s.queue, core.Event{Kind: core.EventStop, StopReason: stopReason(s.finish, len(s.calls) > 0), Usage: s.usage})
	s.err = io.EOF
	s.closer()
}

func (s *stream) fail(err error) {
	s.err = err
	s.closer()
}

// Close implements core.Stream.
func (s *stream) Close() error {
	s.closer()
	return nil
}

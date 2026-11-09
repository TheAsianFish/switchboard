package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// stream translates the SDK's event union into canonical events.
type stream struct {
	ctx    context.Context
	cancel context.CancelFunc
	s      *ssestream.Stream[sdk.MessageStreamEventUnion]

	usage core.Usage
	stop  core.StopReason
	err   error // sticky: io.EOF after the stop event, or the failure

	// tools counts the tool_use blocks seen; toolBlock is the content
	// block index of the open one, -1 when none is open.
	tools     int
	toolBlock int64
	// pending is an event to return before reading on.
	pending *core.Event

	once   sync.Once
	closed atomic.Bool
}

// Next implements core.Stream.
func (s *stream) Next() (core.Event, error) {
	if s.err != nil {
		return core.Event{}, s.err
	}
	if s.closed.Load() {
		return core.Event{}, io.EOF
	}
	if s.pending != nil {
		ev := *s.pending
		s.pending = nil
		return ev, nil
	}
	for s.s.Next() {
		ev := s.s.Current()
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			s.usage = core.Usage{
				InputTokens:      int(u.InputTokens),
				CacheReadTokens:  int(u.CacheReadInputTokens),
				CacheWriteTokens: int(u.CacheCreationInputTokens),
			}
			return core.Event{Kind: core.EventStart, ID: ev.Message.ID, Model: ev.Message.Model, Usage: s.usage}, nil
		case "content_block_start":
			if ev.ContentBlock.Type != "tool_use" {
				continue
			}
			i := s.tools
			s.tools++
			s.toolBlock = ev.Index
			start := core.Event{Kind: core.EventToolCallStart, ToolIndex: i, ToolCallID: ev.ContentBlock.ID, ToolName: ev.ContentBlock.Name}
			// The API opens a tool_use block with input {} and streams
			// the arguments; an upstream that sends them up front gets
			// them passed on as the first fragment.
			if in, ok := ev.ContentBlock.Input.(map[string]any); ok && len(in) > 0 {
				if raw, err := json.Marshal(in); err == nil {
					s.pending = &core.Event{Kind: core.EventToolCallDelta, ToolIndex: i, Arguments: string(raw)}
				}
			}
			return start, nil
		case "content_block_stop":
			if ev.Index == s.toolBlock {
				s.toolBlock = -1
			}
		case "content_block_delta":
			switch {
			case ev.Delta.Type == "text_delta" && ev.Delta.Text != "":
				return core.Event{Kind: core.EventDelta, Text: ev.Delta.Text}, nil
			case ev.Delta.Type == "input_json_delta" && ev.Delta.PartialJSON != "":
				if ev.Index != s.toolBlock {
					s.finish(core.Errorf(core.KindUpstream, "upstream sent tool arguments for block %d, which is not an open tool_use block", ev.Index))
					return core.Event{}, s.err
				}
				return core.Event{Kind: core.EventToolCallDelta, ToolIndex: s.tools - 1, Arguments: ev.Delta.PartialJSON}, nil
			}
		case "message_delta":
			s.stop = stopReason(ev.Delta.StopReason)
			u := ev.Usage
			s.usage.OutputTokens = int(u.OutputTokens)
			// Usage on message_delta is cumulative; input counts repeat
			// message_start's or correct them (Ollama reports them only here).
			if u.InputTokens != 0 {
				s.usage.InputTokens = int(u.InputTokens)
			}
			if u.CacheReadInputTokens != 0 {
				s.usage.CacheReadTokens = int(u.CacheReadInputTokens)
			}
			if u.CacheCreationInputTokens != 0 {
				s.usage.CacheWriteTokens = int(u.CacheCreationInputTokens)
			}
		case "message_stop":
			s.finish(io.EOF)
			stop := s.stop
			if stop == "" {
				stop = core.StopEndTurn
			}
			return core.Event{Kind: core.EventStop, StopReason: stop, Usage: s.usage}, nil
		}
	}
	var err error
	if cause := s.s.Err(); cause != nil {
		err = classify(s.ctx, cause)
	} else {
		err = core.Errorf(core.KindUpstream, "upstream stream ended early")
	}
	s.finish(err)
	return core.Event{}, err
}

// finish records the stream's last result and releases the connection and
// the deadline.
func (s *stream) finish(err error) {
	if s.err == nil {
		s.err = err
	}
	s.release()
}

func (s *stream) release() {
	s.once.Do(func() {
		_ = s.s.Close()
		s.cancel()
	})
}

// Close implements core.Stream. It may be called at any time, also from
// another goroutine to abort a Next blocked on the upstream.
func (s *stream) Close() error {
	s.closed.Store(true)
	s.release()
	return nil
}

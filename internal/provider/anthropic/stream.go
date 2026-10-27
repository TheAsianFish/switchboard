package anthropic

import (
	"context"
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
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				return core.Event{Kind: core.EventDelta, Text: ev.Delta.Text}, nil
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

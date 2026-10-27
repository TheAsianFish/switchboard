package anthropic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/sse"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// message is a Messages response, also the payload of message_start.
type message struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []textBlock `json:"content"`
	StopReason   *string     `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        usage       `json:"usage"`
}

func wireUsage(u core.Usage) usage {
	return usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
	}
}

// renderMessage builds the non-streamed response body.
func renderMessage(r *core.Response) message {
	stop := stopReason(r.StopReason)
	return message{
		ID:         messageID(r.ID),
		Type:       "message",
		Role:       "assistant",
		Model:      r.Model,
		Content:    []textBlock{{Type: "text", Text: r.Text}},
		StopReason: &stop,
		Usage:      wireUsage(r.Usage),
	}
}

// stopReason maps a canonical stop reason to Anthropic's.
func stopReason(s core.StopReason) string {
	switch s {
	case core.StopMaxTokens:
		return "max_tokens"
	case core.StopSequence:
		return "stop_sequence"
	case core.StopContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// messageID gives a provider id Anthropic's "msg_" prefix, or makes one up
// when the provider sent none.
func messageID(id string) string {
	if id == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		return "msg_" + hex.EncodeToString(b[:])
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

// Stream event payloads, in the order they are sent.
type (
	messageStartEvent struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}
	contentBlockStartEvent struct {
		Type         string    `json:"type"`
		Index        int       `json:"index"`
		ContentBlock textBlock `json:"content_block"`
	}
	pingEvent struct {
		Type string `json:"type"`
	}
	contentBlockDeltaEvent struct {
		Type  string    `json:"type"`
		Index int       `json:"index"`
		Delta textBlock `json:"delta"`
	}
	contentBlockStopEvent struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}
	messageDeltaEvent struct {
		Type  string       `json:"type"`
		Delta messageDelta `json:"delta"`
		Usage deltaUsage   `json:"usage"`
	}
	messageDelta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	}
	// deltaUsage carries the cumulative output count and, when the provider
	// reported them at the end, the input counts.
	deltaUsage struct {
		InputTokens              *int `json:"input_tokens,omitempty"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
		OutputTokens             int  `json:"output_tokens"`
	}
	messageStopEvent struct {
		Type string `json:"type"`
	}
	errorEvent = errorBody
)

// serveStream answers a streaming request. Nothing is written until the
// first token or the stop event: a start event alone proves little (the
// Anthropic API sends message_start before it generates, and can still
// fail with overloaded_error), so failures before the first token are
// ordinary JSON errors with the right status.
func serveStream(ctx context.Context, w http.ResponseWriter, gw core.Gateway, req *core.Request) {
	s, err := gw.Stream(ctx, req)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	defer func() { _ = s.Close() }()

	first, err := s.Next()
	if errors.Is(err, io.EOF) {
		err = core.Errorf(core.KindUpstream, "upstream stream ended early")
	}
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	start := first
	if first.Kind == core.EventStart {
		first, err = s.Next()
		if errors.Is(err, io.EOF) {
			err = core.Errorf(core.KindUpstream, "upstream stream ended early")
		}
		if err != nil {
			writeError(ctx, w, err)
			return
		}
	} else {
		// Tolerate a provider that skips the start event.
		start = core.Event{Kind: core.EventStart}
	}

	sw := sse.NewWriter(w)
	w.WriteHeader(http.StatusOK)
	st := &streamWriter{ctx: ctx, w: sw}
	if start.Model == "" {
		start.Model = req.Model
	}
	if !st.start(start) {
		return
	}
	if !st.event(first) {
		return
	}
	for {
		ev, err := s.Next()
		if ctx.Err() != nil {
			return // client gone
		}
		if errors.Is(err, io.EOF) {
			err = core.Errorf(core.KindUpstream, "upstream stream ended early")
		}
		if err != nil {
			st.fail(err)
			return
		}
		if !st.event(ev) {
			return
		}
	}
}

// streamWriter writes Anthropic stream events; each write reports whether
// the client is still there.
type streamWriter struct {
	ctx context.Context
	w   *sse.Writer
}

func (s *streamWriter) write(name string, payload any) bool {
	data, err := marshal(payload)
	if err != nil {
		telemetry.Logger(s.ctx).Error("encode stream event", "event", name, "error", err.Error())
		return false
	}
	if err := s.w.Write(name, data); err != nil {
		telemetry.Logger(s.ctx).Debug("client went away", "error", err.Error())
		return false
	}
	return true
}

func (s *streamWriter) start(ev core.Event) bool {
	u := wireUsage(ev.Usage)
	u.OutputTokens = 0
	msg := message{
		ID:      messageID(ev.ID),
		Type:    "message",
		Role:    "assistant",
		Model:   ev.Model,
		Content: []textBlock{},
		Usage:   u,
	}
	return s.write("message_start", messageStartEvent{Type: "message_start", Message: msg}) &&
		s.write("content_block_start", contentBlockStartEvent{Type: "content_block_start", ContentBlock: textBlock{Type: "text", Text: ""}}) &&
		s.write("ping", pingEvent{Type: "ping"})
}

func (s *streamWriter) stop(ev core.Event) {
	du := deltaUsage{OutputTokens: ev.Usage.OutputTokens}
	if ev.Usage.PromptTokens() > 0 {
		in, cw, cr := ev.Usage.InputTokens, ev.Usage.CacheWriteTokens, ev.Usage.CacheReadTokens
		du.InputTokens, du.CacheCreationInputTokens, du.CacheReadInputTokens = &in, &cw, &cr
	}
	if s.write("content_block_stop", contentBlockStopEvent{Type: "content_block_stop"}) &&
		s.write("message_delta", messageDeltaEvent{Type: "message_delta", Delta: messageDelta{StopReason: stopReason(ev.StopReason)}, Usage: du}) {
		s.write("message_stop", messageStopEvent{Type: "message_stop"})
	}
}

// event writes a delta or the closing events and reports whether the stream
// goes on.
func (s *streamWriter) event(ev core.Event) bool {
	switch ev.Kind {
	case core.EventDelta:
		if ev.Text == "" {
			return true
		}
		return s.write("content_block_delta", contentBlockDeltaEvent{Type: "content_block_delta", Delta: textBlock{Type: "text_delta", Text: ev.Text}})
	case core.EventStop:
		s.stop(ev)
		return false
	default:
		return true // a repeated start carries nothing to send
	}
}

// fail ends a stream that already started with an error event.
func (s *streamWriter) fail(err error) {
	_, status, detail := clientError(s.ctx, err) // logs server-side failures
	if status < 500 {
		telemetry.Logger(s.ctx).Warn("stream failed", "status", status, "error", err.Error())
	}
	_ = s.write("error", errorEvent{Type: "error", Error: detail})
}

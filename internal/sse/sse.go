// Package sse reads and writes server-sent events as both LLM APIs use them.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Event is one server-sent event. Name is the "event:" field and is empty for
// streams that only send "data:" lines (the OpenAI format).
type Event struct {
	Name string
	Data []byte
}

// Writer writes events to an HTTP response and flushes after each one, so a
// token reaches the client as soon as the provider produced it.
type Writer struct {
	w       io.Writer
	flusher http.Flusher
}

// NewWriter sets the streaming headers on w. It must be called before the
// status code is written.
func NewWriter(w http.ResponseWriter) *Writer {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Tells nginx-style proxies in front of the gateway not to buffer.
	h.Set("X-Accel-Buffering", "no")
	f, _ := w.(http.Flusher)
	return &Writer{w: w, flusher: f}
}

// Write sends one event and flushes it.
func (w *Writer) Write(name string, data []byte) error {
	var buf bytes.Buffer
	buf.Grow(len(name) + len(data) + 16)
	if name != "" {
		buf.WriteString("event: ")
		buf.WriteString(name)
		buf.WriteByte('\n')
	}
	// A data payload with newlines must be split over several data lines.
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	if _, err := w.w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// ErrLineTooLong is returned when a single line exceeds the reader's limit.
var ErrLineTooLong = errors.New("sse: line too long")

// maxLine bounds one line so a misbehaving upstream cannot make the gateway
// buffer without limit.
const maxLine = 4 << 20

// Reader parses an event stream.
type Reader struct {
	s *bufio.Scanner
}

// NewReader reads events from r.
func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), maxLine)
	return &Reader{s: s}
}

// Next returns the next event, or io.EOF when the stream ends cleanly. A
// stream that ends in the middle of an event returns that event first.
func (r *Reader) Next() (Event, error) {
	var ev Event
	var data [][]byte
	seen := false
	for r.s.Scan() {
		line := r.s.Bytes()
		if len(line) == 0 {
			if seen {
				ev.Data = bytes.Join(data, []byte("\n"))
				return ev, nil
			}
			continue
		}
		if line[0] == ':' {
			continue // comment, used as a keep-alive
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.Name = string(value)
			seen = true
		case "data":
			data = append(data, bytes.Clone(value))
			seen = true
		}
	}
	if err := r.s.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Event{}, ErrLineTooLong
		}
		return Event{}, fmt.Errorf("read event stream: %w", err)
	}
	if seen {
		ev.Data = bytes.Join(data, []byte("\n"))
		return ev, nil
	}
	return Event{}, io.EOF
}

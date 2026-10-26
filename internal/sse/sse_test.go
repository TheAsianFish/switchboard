package sse

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  []Event
	}{
		{
			name:  "data only",
			input: "data: {\"a\":1}\n\ndata: [DONE]\n\n",
			want:  []Event{{Data: []byte(`{"a":1}`)}, {Data: []byte("[DONE]")}},
		},
		{
			name:  "named events with comments and blank lines",
			input: ": ping\n\n\nevent: message_start\ndata: {}\n\nevent: ping\ndata: {\"type\": \"ping\"}\n\n",
			want:  []Event{{Name: "message_start", Data: []byte("{}")}, {Name: "ping", Data: []byte(`{"type": "ping"}`)}},
		},
		{
			name:  "multi-line data and CRLF",
			input: "data: a\r\ndata: b\r\n\r\n",
			want:  []Event{{Data: []byte("a\nb")}},
		},
		{
			name:  "no space after colon",
			input: "event:x\ndata:y\n\n",
			want:  []Event{{Name: "x", Data: []byte("y")}},
		},
		{
			name:  "truncated final event",
			input: "data: last",
			want:  []Event{{Data: []byte("last")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewReader(strings.NewReader(tt.input))
			var got []Event
			for {
				ev, err := r.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				got = append(got, ev)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d events %q, want %d", len(got), got, len(tt.want))
			}
			for i := range got {
				if got[i].Name != tt.want[i].Name || string(got[i].Data) != string(tt.want[i].Data) {
					t.Errorf("event %d = %q/%q, want %q/%q", i, got[i].Name, got[i].Data, tt.want[i].Name, tt.want[i].Data)
				}
			}
		})
	}
}

func TestReaderLineTooLong(t *testing.T) {
	t.Parallel()
	r := NewReader(strings.NewReader("data: " + strings.Repeat("x", maxLine+1) + "\n\n"))
	if _, err := r.Next(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestWriterRoundTrip(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := NewWriter(rec)
	if err := w.Write("delta", []byte("one\ntwo")); err != nil {
		t.Fatal(err)
	}
	if err := w.Write("", []byte("[DONE]")); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if !rec.Flushed {
		t.Error("writer did not flush")
	}
	r := NewReader(rec.Body)
	ev, err := r.Next()
	if err != nil || ev.Name != "delta" || string(ev.Data) != "one\ntwo" {
		t.Fatalf("first event = %q %q %v", ev.Name, ev.Data, err)
	}
	ev, err = r.Next()
	if err != nil || ev.Name != "" || string(ev.Data) != "[DONE]" {
		t.Fatalf("second event = %q %q %v", ev.Name, ev.Data, err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

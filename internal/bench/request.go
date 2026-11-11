package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/TheAsianFish/switchboard/internal/sse"
)

// Outcomes of a request, the keys of Result.Outcomes. A non-2xx status is
// recorded as "http_<status>", for example "http_500".
const (
	OutcomeOK = "ok"
	// OutcomeTimeout: the per-request timeout expired before the reply was
	// complete.
	OutcomeTimeout = "timeout"
	// OutcomeTransport: the connection failed (refused, reset, ...).
	OutcomeTransport = "transport_error"
	// OutcomeMalformedStream: a 200 stream that is cut short, ends without
	// its terminal event or carries an event that does not parse.
	OutcomeMalformedStream = "malformed_stream"
	// OutcomeMalformedBody: a 200 non-streamed reply that is not JSON.
	OutcomeMalformedBody = "malformed_body"
	// OutcomeStreamError: the stream carried an error event.
	OutcomeStreamError = "stream_error"
	// OutcomeDropped: not sent because MaxInFlight requests were already
	// outstanding at its scheduled time.
	OutcomeDropped = "dropped"
	// OutcomeCanceled: the run's context ended while the request was out.
	OutcomeCanceled = "canceled"
)

func httpOutcome(status int) string { return fmt.Sprintf("http_%d", status) }

// maxBody bounds how much of a non-streamed reply is buffered.
const maxBody = 16 << 20

// sample is the timing of one request. Times are absolute and carry the
// monotonic clock reading, so differences are immune to wall-clock steps.
type sample struct {
	outcome string
	// status is the HTTP status, 0 if no response arrived; cache is the
	// gateway's cache status header, "" if absent.
	status     int
	cache      string
	sent       time.Time // handed to the HTTP client
	firstToken time.Time // first generated text; zero if none arrived
	done       time.Time // reply complete (or failed)
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiRequest struct {
	Model         string         `json:"model"`
	Messages      []message      `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type anthropicRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
	Stream    bool      `json:"stream,omitempty"`
}

// requestBody is the JSON body of request i in cfg's format.
func requestBody(cfg *Config, i int) ([]byte, error) {
	msgs := []message{{Role: "user", Content: prompt(cfg, i)}}
	if cfg.Format == FormatAnthropic {
		return json.Marshal(anthropicRequest{Model: cfg.Model, MaxTokens: cfg.MaxTokens, Messages: msgs, Stream: cfg.Stream})
	}
	r := openaiRequest{Model: cfg.Model, Messages: msgs, MaxTokens: cfg.MaxTokens, Stream: cfg.Stream}
	if cfg.Stream {
		r.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return json.Marshal(r)
}

// newRequest builds the HTTP request for request i.
func newRequest(ctx context.Context, cfg *Config, i int) (*http.Request, error) {
	body, err := requestBody(cfg, i)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	path := "/v1/chat/completions"
	if cfg.Format == FormatAnthropic {
		path = "/v1/messages"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if cfg.CacheDirective != "" {
		req.Header.Set(CacheDirectiveHeader, cfg.CacheDirective)
	}
	if cfg.Format == FormatAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
		if cfg.APIKey != "" {
			req.Header.Set("x-api-key", cfg.APIKey)
		}
	} else if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	return req, nil
}

// send performs request i and times it. parent is the run's context; the
// request gets its own deadline of cfg.Timeout from the moment it is sent.
func send(parent context.Context, client *http.Client, cfg *Config, i int) sample {
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()
	var s sample
	finish := func(outcome string) sample {
		s.outcome = outcome
		s.done = time.Now()
		return s
	}
	fail := func(err error) sample { return finish(classify(parent, ctx, err)) }

	req, err := newRequest(ctx, cfg, i)
	if err != nil {
		return fail(err)
	}
	s.sent = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	s.status, s.cache = resp.StatusCode, resp.Header.Get(CacheStatusHeader)

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return finish(httpOutcome(resp.StatusCode))
	}
	if !cfg.Stream {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, io.LimitReader(resp.Body, maxBody)); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) && ctx.Err() == nil {
				return finish(OutcomeMalformedBody) // body cut short
			}
			return fail(err)
		}
		if !json.Valid(buf.Bytes()) {
			return finish(OutcomeMalformedBody)
		}
		s = finish(OutcomeOK)
		s.firstToken = s.done
		return s
	}

	outcome, err := readStream(resp.Body, cfg.Format, func() {
		if s.firstToken.IsZero() {
			s.firstToken = time.Now()
		}
	})
	if err != nil {
		return fail(err)
	}
	if outcome != OutcomeOK && ctx.Err() != nil {
		return fail(ctx.Err()) // cut short by the deadline, not by the server
	}
	if outcome == OutcomeOK {
		// Drain whatever follows the terminal event so the connection is
		// reused and "total" covers the whole body.
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return fail(err)
		}
	}
	return finish(outcome)
}

// readStream consumes an event stream up to its terminal event and calls
// onToken for each event that carries generated text. It returns an outcome
// for stream-level results (ok, malformed, error event) and an error for
// read failures, which the caller classifies.
func readStream(body io.Reader, format string, onToken func()) (string, error) {
	r := sse.NewReader(body)
	for {
		ev, err := r.Next()
		switch {
		case errors.Is(err, io.EOF):
			return OutcomeMalformedStream, nil // ended without a terminal event
		case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, sse.ErrLineTooLong):
			return OutcomeMalformedStream, nil // cut short mid-transfer, or garbage
		}
		if err != nil {
			return "", err
		}
		var outcome string
		if format == FormatAnthropic {
			outcome = anthropicEvent(ev, onToken)
		} else {
			outcome = openaiEvent(ev, onToken)
		}
		if outcome != "" {
			return outcome, nil
		}
	}
}

// openaiEvent handles one chat-completions chunk. It returns "" to keep
// reading, or the stream's outcome.
func openaiEvent(ev sse.Event, onToken func()) string {
	if string(ev.Data) == "[DONE]" {
		return OutcomeOK
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(ev.Data, &chunk); err != nil {
		return OutcomeMalformedStream
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		return OutcomeStreamError
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			onToken()
			break
		}
	}
	return ""
}

// anthropicEvent handles one messages-API event. It returns "" to keep
// reading, or the stream's outcome.
func anthropicEvent(ev sse.Event, onToken func()) string {
	if !json.Valid(ev.Data) {
		return OutcomeMalformedStream
	}
	switch ev.Name {
	case "content_block_delta":
		onToken()
	case "message_stop":
		return OutcomeOK
	case "error":
		return OutcomeStreamError
	}
	return ""
}

// classify maps a request error to an outcome. parent is the run's context
// and ctx the request's own; their states tell a cancelled run from a
// request that ran out of time.
func classify(parent, ctx context.Context, err error) string {
	switch {
	case parent.Err() != nil:
		return OutcomeCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return OutcomeTimeout
	}
	return OutcomeTransport
}

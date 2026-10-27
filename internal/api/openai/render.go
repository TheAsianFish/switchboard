package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/sse"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

type completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Index        int       `json:"index"`
	Message      message   `json:"message"`
	Logprobs     *struct{} `json:"logprobs"`
	FinishReason string    `json:"finish_reason"`
}

type message struct {
	Role    string  `json:"role"`
	Content string  `json:"content"`
	Refusal *string `json:"refusal"`
}

type usage struct {
	PromptTokens        int           `json:"prompt_tokens"`
	CompletionTokens    int           `json:"completion_tokens"`
	TotalTokens         int           `json:"total_tokens"`
	PromptTokensDetails promptDetails `json:"prompt_tokens_details"`
}

type promptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int       `json:"index"`
	Delta        delta     `json:"delta"`
	Logprobs     *struct{} `json:"logprobs"`
	FinishReason *string   `json:"finish_reason"`
}

type delta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

func toUsage(u core.Usage) usage {
	prompt := u.PromptTokens()
	return usage{
		PromptTokens:        prompt,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         prompt + u.OutputTokens,
		PromptTokensDetails: promptDetails{CachedTokens: u.CacheReadTokens},
	}
}

// finishReason maps a canonical stop reason to OpenAI's finish_reason.
func finishReason(r core.StopReason) string {
	switch r {
	case core.StopMaxTokens:
		return "length"
	case core.StopContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

// serveComplete answers a non-streaming request.
func serveComplete(ctx context.Context, w http.ResponseWriter, gw core.Gateway, req *core.Request) {
	resp, err := gw.Complete(ctx, req)
	if err != nil {
		writeError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusOK, completion{
		ID:      completionID(resp.ID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []choice{{
			Message:      message{Role: "assistant", Content: resp.Text},
			FinishReason: finishReason(resp.StopReason),
		}},
		Usage: toUsage(resp.Usage),
	})
}

// chunkWriter renders canonical events as chat.completion.chunk events that
// share one id, creation time and model.
type chunkWriter struct {
	sw      *sse.Writer
	id      string
	created int64
	model   string
}

func (c *chunkWriter) write(choices []chunkChoice, u *usage) error {
	data, err := json.Marshal(chunk{
		ID:      c.id,
		Object:  "chat.completion.chunk",
		Created: c.created,
		Model:   c.model,
		Choices: choices,
		Usage:   u,
	})
	if err != nil {
		return err
	}
	return c.sw.Write("", data)
}

func (c *chunkWriter) delta(d delta, finish *string) error {
	return c.write([]chunkChoice{{Delta: d, FinishReason: finish}}, nil)
}

// serveStream answers a streaming request. Nothing is written until the
// first token or the stop event: a start event alone proves little (the
// Anthropic API sends message_start before it generates, and can still
// fail with overloaded_error), so a failure before the first token is an
// ordinary JSON error with its own status code.
func serveStream(ctx context.Context, w http.ResponseWriter, gw core.Gateway, req *core.Request, opts options) {
	s, err := gw.Stream(ctx, req)
	if err != nil {
		writeError(w, ctx, err)
		return
	}
	defer func() { _ = s.Close() }()

	ev, err := s.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = core.Errorf(core.KindUpstream, "upstream stream ended early")
		}
		writeError(w, ctx, err)
		return
	}

	cw := &chunkWriter{sw: sse.NewWriter(w), created: time.Now().Unix(), model: req.Model}
	var startUsage core.Usage
	if ev.Kind == core.EventStart {
		cw.id = completionID(ev.ID)
		if ev.Model != "" {
			cw.model = ev.Model
		}
		startUsage = ev.Usage
		if ev, err = s.Next(); err != nil {
			if errors.Is(err, io.EOF) {
				err = core.Errorf(core.KindUpstream, "upstream stream ended early")
			}
			writeError(w, ctx, err)
			return
		}
	} else {
		cw.id = completionID("")
	}
	w.WriteHeader(http.StatusOK)
	empty := ""
	if err := cw.delta(delta{Role: "assistant", Content: &empty}, nil); err != nil {
		return
	}

	for {
		switch ev.Kind {
		case core.EventDelta:
			if ev.Text != "" {
				text := ev.Text
				if err := cw.delta(delta{Content: &text}, nil); err != nil {
					return
				}
			}
		case core.EventStop:
			reason := finishReason(ev.StopReason)
			if err := cw.delta(delta{}, &reason); err != nil {
				return
			}
			if opts.includeUsage {
				u := toUsage(mergeUsage(startUsage, ev.Usage))
				if err := cw.write([]chunkChoice{}, &u); err != nil {
					return
				}
			}
			_ = cw.sw.Write("", []byte("[DONE]"))
			return
		}
		if ctx.Err() != nil {
			return
		}
		ev, err = s.Next()
		if err != nil {
			if ctx.Err() != nil {
				return // the client went away; nobody is left to tell
			}
			if errors.Is(err, io.EOF) {
				err = core.Errorf(core.KindUpstream, "upstream stream ended early")
			}
			writeStreamError(ctx, cw, err)
			return
		}
	}
}

// writeStreamError reports a failure after the headers went out. The stream
// ends without [DONE] so clients see it as incomplete.
func writeStreamError(ctx context.Context, cw *chunkWriter, err error) {
	e := core.AsError(err)
	status, obj := classify(e)
	logError(ctx, "stream failed", status, e)
	if status < 500 {
		telemetry.Logger(ctx).Warn("stream failed", "status", status, "kind", e.Kind.String(), "error", e.Error())
	}
	data, mErr := json.Marshal(errorBody{Error: obj})
	if mErr != nil {
		return
	}
	_ = cw.sw.Write("", data)
}

// mergeUsage keeps the prompt counts from the start event when the stop
// event does not repeat them, as core.Collect does.
func mergeUsage(start, stop core.Usage) core.Usage {
	if stop.PromptTokens() == 0 {
		stop.InputTokens = start.InputTokens
		stop.CacheReadTokens = start.CacheReadTokens
		stop.CacheWriteTokens = start.CacheWriteTokens
	}
	return stop
}

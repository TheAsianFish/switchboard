package routereval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/sse"
)

// RecordingPath is where a candidate's labels are kept: one JSONL file per
// model, named after it with characters unsafe in a file name replaced.
func RecordingPath(dir, model string) string {
	return filepath.Join(dir, safeName(model)+".jsonl")
}

func safeName(model string) string {
	return strings.NewReplacer(":", "_", "/", "_").Replace(model)
}

func labelMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eval router label", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "http://127.0.0.1:8091", "the gateway's base URL")
	keyEnv := fs.String("key-env", "SWITCHBOARD_EVAL_KEY", "environment variable holding the gateway API key")
	models := fs.String("models", "", "comma-separated candidate model names, as configured in the gateway")
	questions := fs.String("questions", "eval/router/data/questions.jsonl", "the eval set")
	dir := fs.String("recordings", "eval/recordings/router", "directory of per-model recordings")
	limit := fs.Int("limit", 0, "label at most this many questions per model (0: all)")
	timeout := fs.Duration("timeout", 5*time.Minute, "deadline for one request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *models == "" {
		return errors.New("-models is required")
	}
	key := os.Getenv(*keyEnv)
	if key == "" {
		return fmt.Errorf("%s is not set", *keyEnv)
	}
	qs, err := readJSONL[Question](*questions)
	if err != nil {
		return err
	}
	if *limit > 0 && *limit < len(qs) {
		qs = qs[:*limit]
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	c := &Client{Base: strings.TrimRight(*url, "/"), Key: key, HTTP: &http.Client{}, Timeout: *timeout}
	for _, m := range strings.Split(*models, ",") {
		m = strings.TrimSpace(m)
		if err := labelModel(ctx, c, m, qs, RecordingPath(*dir, m), stdout); err != nil {
			return err
		}
	}
	return nil
}

// labelModel sends every question m has no label for yet and appends each
// graded reply to path as it arrives, so an interrupted run resumes where
// it stopped. A failed request is reported and skipped, not recorded.
func labelModel(ctx context.Context, c *Client, m string, qs []Question, path string, stdout io.Writer) error {
	done := map[string]bool{}
	if old, err := readJSONL[Label](path); err == nil {
		for _, l := range old {
			done[l.ID] = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var todo, failed, correct int
	start := time.Now()
	for _, q := range qs {
		if done[q.ID] {
			continue
		}
		todo++
		l := c.Ask(ctx, m, q)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if l.Error != "" {
			failed++
			_, _ = fmt.Fprintf(stdout, "%s %s: %s\n", m, q.ID, l.Error)
			continue
		}
		if l.Correct {
			correct++
		}
		b, _ := json.Marshal(l)
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
		if todo%25 == 0 {
			_, _ = fmt.Fprintf(stdout, "%s: %d sent, %d correct, %d failed, %s\n", m, todo, correct, failed, time.Since(start).Round(time.Second))
		}
	}
	_, _ = fmt.Fprintf(stdout, "%s: done; %d sent this run (%d correct, %d failed), %d already recorded\n", m, todo, correct, failed, len(done))
	return nil
}

// Client sends eval questions to the gateway in the OpenAI format,
// streamed, so the label carries time to first token as well as total
// latency.
type Client struct {
	Base, Key string
	HTTP      *http.Client
	Timeout   time.Duration
	Now       func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Ask sends q to model m and grades the reply.
func (c *Client) Ask(ctx context.Context, m string, q Question) Label {
	system, user := Prompt(q)
	temp := 0.0
	body, _ := json.Marshal(map[string]any{
		"model": m, "stream": true, "stream_options": map[string]bool{"include_usage": true},
		"temperature": temp, "max_tokens": MaxTokens(q.Dataset),
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
	})
	start := c.now()
	l := Label{ID: q.ID, Model: m, At: start.UTC().Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		l.Error = err.Error()
		return l
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		l.Error = fmt.Sprintf("status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
		return l
	}
	var text strings.Builder
	r := sse.NewReader(bufio.NewReader(resp.Body))
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			l.Error = err.Error()
			return l
		}
		if string(ev.Data) == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(ev.Data, &chunk); err != nil {
			l.Error = fmt.Sprintf("decode chunk: %v", err)
			return l
		}
		if chunk.Error != nil {
			l.Error = chunk.Error.Message
			return l
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" && l.TTFTMillis == 0 {
				l.TTFTMillis = millis(c.now().Sub(start))
			}
			text.WriteString(ch.Delta.Content)
			if ch.FinishReason != nil {
				l.StopReason = *ch.FinishReason
			}
		}
		if chunk.Usage != nil {
			l.InputTokens, l.OutputTokens = chunk.Usage.Prompt, chunk.Usage.Completion
		}
	}
	l.TotalMillis = millis(c.now().Sub(start))
	if l.StopReason == "" {
		l.Error = "stream ended without a finish reason"
		return l
	}
	l.Output = text.String()
	l.Extracted, l.Correct = Grade(q, l.Output)
	if p, ok := cost.Defaults[m]; ok {
		l.Nanodollars = int64(p.Of(core.Usage{InputTokens: l.InputTokens, OutputTokens: l.OutputTokens}))
	}
	return l
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

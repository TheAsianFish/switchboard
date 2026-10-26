// Package fake is a deterministic provider with configurable latency. It
// backs unit tests in process and, wrapped in the OpenAI and Anthropic front
// ends by `switchboard fake-provider`, the overhead benchmark across
// processes.
package fake

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// Config sets the shape of every reply.
type Config struct {
	// Latency is the delay before the first byte, in both modes. A
	// non-streamed reply arrives after Latency.
	Latency time.Duration
	// Tokens is the number of output tokens (words) per reply.
	Tokens int
	// TokenInterval is the delay between streamed deltas.
	TokenInterval time.Duration
}

// DefaultConfig is the shape PLAN.md's Q1 method uses.
var DefaultConfig = Config{Latency: 20 * time.Millisecond, Tokens: 30, TokenInterval: 5 * time.Millisecond}

// Provider is the fake backend. It is safe for concurrent use.
type Provider struct {
	cfg   Config
	calls atomic.Int64
	seq   atomic.Int64

	mu   sync.Mutex
	last *core.Request
}

// New returns a fake provider with cfg.
func New(cfg Config) *Provider {
	if cfg.Tokens <= 0 {
		cfg.Tokens = 1
	}
	return &Provider{cfg: cfg}
}

// Calls is the number of requests the provider has received.
func (p *Provider) Calls() int64 { return p.calls.Load() }

// LastRequest returns a copy of the most recent request, or nil.
func (p *Provider) LastRequest() *core.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		return nil
	}
	c := *p.last
	c.Messages = append([]core.Message(nil), p.last.Messages...)
	return &c
}

func (p *Provider) record(req *core.Request) string {
	p.calls.Add(1)
	c := *req
	c.Messages = append([]core.Message(nil), req.Messages...)
	p.mu.Lock()
	p.last = &c
	p.mu.Unlock()
	return fmt.Sprintf("fake-%d", p.seq.Add(1))
}

// Complete implements core.Provider.
func (p *Provider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	id := p.record(req)
	if err := sleep(ctx, p.cfg.Latency); err != nil {
		return nil, err
	}
	words, stop := p.reply(req)
	return &core.Response{
		ID:         id,
		Model:      req.Model,
		Text:       strings.Join(words, ""),
		StopReason: stop,
		Usage:      core.Usage{InputTokens: InputTokens(req), OutputTokens: len(words)},
	}, nil
}

// Stream implements core.Provider.
func (p *Provider) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	id := p.record(req)
	words, stop := p.reply(req)
	return &stream{
		ctx:   ctx,
		cfg:   p.cfg,
		id:    id,
		model: req.Model,
		words: words,
		stop:  stop,
		input: InputTokens(req),
	}, nil
}

// reply builds the deterministic word list for req: the same request always
// gets the same reply, and different prompts get different ones.
func (p *Provider) reply(req *core.Request) ([]string, core.StopReason) {
	h := fnv.New64a()
	_, _ = io.WriteString(h, req.System)
	for _, m := range req.Messages {
		_, _ = io.WriteString(h, string(m.Role))
		_, _ = io.WriteString(h, m.Text)
	}
	seed := h.Sum64()
	n := p.cfg.Tokens
	stop := core.StopEndTurn
	if req.MaxTokens > 0 && req.MaxTokens < n {
		n = req.MaxTokens
		stop = core.StopMaxTokens
	}
	words := make([]string, n)
	for i := range words {
		w := vocabulary[(seed+uint64(i)*2654435761)%uint64(len(vocabulary))]
		if i > 0 {
			w = " " + w
		}
		words[i] = w
	}
	return words, stop
}

var vocabulary = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	"india", "juliett", "kilo", "lima", "mike", "november", "oscar", "papa",
	"quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey",
	"xray", "yankee", "zulu",
}

// InputTokens is the fake's token count for a request: whitespace-separated
// words in the system prompt and every message.
func InputTokens(req *core.Request) int {
	n := len(strings.Fields(req.System))
	for _, m := range req.Messages {
		n += len(strings.Fields(m.Text))
	}
	return n
}

type stream struct {
	ctx   context.Context
	cfg   Config
	id    string
	model string
	words []string
	stop  core.StopReason
	input int

	state  int // 0 before start, 1..len(words) deltas sent, then stop, then done
	closed bool
}

func (s *stream) Next() (core.Event, error) {
	if s.closed {
		return core.Event{}, io.EOF
	}
	switch {
	case s.state == 0:
		if err := sleep(s.ctx, s.cfg.Latency); err != nil {
			return core.Event{}, err
		}
		s.state++
		return core.Event{Kind: core.EventStart, ID: s.id, Model: s.model, Usage: core.Usage{InputTokens: s.input}}, nil
	case s.state <= len(s.words):
		if s.state > 1 {
			if err := sleep(s.ctx, s.cfg.TokenInterval); err != nil {
				return core.Event{}, err
			}
		}
		w := s.words[s.state-1]
		s.state++
		return core.Event{Kind: core.EventDelta, Text: w}, nil
	case s.state == len(s.words)+1:
		s.state++
		return core.Event{Kind: core.EventStop, StopReason: s.stop, Usage: core.Usage{InputTokens: s.input, OutputTokens: len(s.words)}}, nil
	default:
		return core.Event{}, io.EOF
	}
}

func (s *stream) Close() error {
	s.closed = true
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return core.Wrap(core.KindTimeout, ctx.Err(), "request cancelled")
	case <-t.C:
		return nil
	}
}

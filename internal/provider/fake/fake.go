// Package fake is a deterministic provider with configurable latency. It
// backs unit tests in process and, wrapped in the OpenAI and Anthropic front
// ends by `switchboard fake-provider`, the overhead benchmark across
// processes.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
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
	// LatencySigma, when positive, makes each request's first-byte delay
	// log-normal with median Latency: Latency·exp(σ·Z), Z standard normal.
	LatencySigma float64
	// SlowFraction of requests, drawn independently, take SlowFactor times
	// as long to their first byte: a slow tail such as a busy upstream
	// replica. Q4 uses 5% at 10×.
	SlowFraction float64
	SlowFactor   float64
	// Seed seeds the latency draws; runs with the same seed and the same
	// arrival order draw the same delays.
	Seed uint64
}

// DefaultConfig is the shape PLAN.md's Q1 method uses.
var DefaultConfig = Config{Latency: 20 * time.Millisecond, Tokens: 30, TokenInterval: 5 * time.Millisecond}

// Provider is the fake backend. It is safe for concurrent use.
type Provider struct {
	cfg   Config
	calls atomic.Int64
	seq   atomic.Int64

	mu     sync.Mutex
	last   *core.Request
	ledger []Entry
	rnd    *rand.Rand
}

// Entry is one request in the provider's ledger: when it arrived and the
// usage the provider reported for it. It is the ground truth the limits
// test checks the gateway against: what actually reached the provider and
// what it said each request used.
type Entry struct {
	// Time is when the request arrived.
	Time   time.Time `json:"time"`
	Model  string    `json:"model"`
	Stream bool      `json:"stream"`
	// InputTokens and OutputTokens are the usage reported to the caller;
	// for a stream closed early, the output sent so far.
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	Completed    bool `json:"completed"`
}

// Ledger returns a copy of every request received since the last reset.
func (p *Provider) Ledger() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Entry(nil), p.ledger...)
}

// ResetLedger empties the ledger.
func (p *Provider) ResetLedger() {
	p.mu.Lock()
	p.ledger = nil
	p.mu.Unlock()
}

func (p *Provider) settle(i int, output int, completed bool) {
	p.mu.Lock()
	p.ledger[i].OutputTokens = output
	p.ledger[i].Completed = completed
	p.mu.Unlock()
}

// New returns a fake provider with cfg.
func New(cfg Config) *Provider {
	if cfg.Tokens <= 0 {
		cfg.Tokens = 1
	}
	return &Provider{cfg: cfg, rnd: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))}
}

// latency draws one request's first-byte delay.
func (p *Provider) latency() time.Duration {
	c := p.cfg
	if c.LatencySigma <= 0 && c.SlowFraction <= 0 {
		return c.Latency
	}
	p.mu.Lock()
	z, u := p.rnd.NormFloat64(), p.rnd.Float64()
	p.mu.Unlock()
	d := float64(c.Latency)
	if c.LatencySigma > 0 {
		d *= math.Exp(c.LatencySigma * z)
	}
	if u < c.SlowFraction {
		d *= c.SlowFactor
	}
	return time.Duration(d)
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

// record notes a request and returns its reply id and ledger index.
func (p *Provider) record(req *core.Request) (string, int) {
	p.calls.Add(1)
	c := *req
	c.Messages = append([]core.Message(nil), req.Messages...)
	p.mu.Lock()
	p.last = &c
	p.ledger = append(p.ledger, Entry{Time: time.Now(), Model: req.Model, Stream: req.Stream, InputTokens: InputTokens(req)})
	i := len(p.ledger) - 1
	p.mu.Unlock()
	return fmt.Sprintf("fake-%d", p.seq.Add(1)), i
}

// Complete implements core.Provider.
func (p *Provider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	id, entry := p.record(req)
	if err := sleep(ctx, p.latency()); err != nil {
		return nil, err
	}
	words, stop := p.reply(req)
	p.settle(entry, len(words), true)
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
	id, entry := p.record(req)
	words, stop := p.reply(req)
	return &stream{
		p:     p,
		entry: entry,
		ctx:   ctx,
		cfg:   p.cfg,
		delay: p.latency(),
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
	p     *Provider
	entry int
	ctx   context.Context
	cfg   Config
	delay time.Duration
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
		if err := sleep(s.ctx, s.delay); err != nil {
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
		s.p.settle(s.entry, s.state, false)
		s.state++
		return core.Event{Kind: core.EventDelta, Text: w}, nil
	case s.state == len(s.words)+1:
		s.state++
		s.p.settle(s.entry, len(s.words), true)
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

// LedgerHandler serves the ledger: GET returns {"entries": [...]}, POST to
// .../reset empties it. `switchboard fake-provider` mounts it at /ledger.
func (p *Provider) LedgerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ledger", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Entries []Entry `json:"entries"`
		}{Entries: p.Ledger()})
	})
	mux.HandleFunc("POST /ledger/reset", func(w http.ResponseWriter, _ *http.Request) {
		p.ResetLedger()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

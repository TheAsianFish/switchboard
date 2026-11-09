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
	"slices"
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
	if call := toolCall(req); call != nil {
		p.settle(entry, callTokens, true)
		return &core.Response{
			ID:         id,
			Model:      req.Model,
			ToolCalls:  []core.ToolCall{*call},
			StopReason: core.StopToolUse,
			Usage:      core.Usage{InputTokens: InputTokens(req), OutputTokens: callTokens},
		}, nil
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
	s := &stream{
		p:     p,
		entry: entry,
		ctx:   ctx,
		cfg:   p.cfg,
		delay: p.latency(),
		id:    id,
		model: req.Model,
		input: InputTokens(req),
	}
	if s.call = toolCall(req); s.call != nil {
		s.stop = core.StopToolUse
	} else {
		s.words, s.stop = p.reply(req)
	}
	return s, nil
}

// callTokens is the output a tool call reply reports.
const callTokens = 2

// toolCall returns the fake's tool call for req, or nil when it should
// answer in text. A request offering tools whose last turn carries no tool
// result gets one call to the first tool, as a model asked to use a tool
// would; once the result comes back it answers in text. The call's id and
// arguments derive from the request: each top-level property of the
// tool's schema, in name order, gets a word.
func toolCall(req *core.Request) *core.ToolCall {
	n := len(req.Messages)
	if len(req.Tools) == 0 || req.ToolChoice.Mode == core.ToolChoiceNone || n > 0 && len(req.Messages[n-1].ToolResults) > 0 {
		return nil
	}
	seed := seedOf(req)
	t := req.Tools[0]
	if req.ToolChoice.Mode == core.ToolChoiceTool {
		for _, c := range req.Tools {
			if c.Name == req.ToolChoice.Name {
				t = c
			}
		}
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(t.Parameters, &schema) // no schema, no arguments
	names := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		names = append(names, k)
	}
	slices.Sort(names)
	args := make(map[string]string, len(names))
	for i, k := range names {
		args[k] = vocabulary[(seed+uint64(i)*2654435761)%uint64(len(vocabulary))]
	}
	raw, _ := json.Marshal(args) // a map of strings always encodes
	return &core.ToolCall{ID: fmt.Sprintf("call_%016x", seed), Name: t.Name, Arguments: raw}
}

// seedOf hashes the prompt: the same request always gets the same reply,
// and different prompts get different ones.
func seedOf(req *core.Request) uint64 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, req.System)
	for _, m := range req.Messages {
		_, _ = io.WriteString(h, string(m.Role))
		_, _ = io.WriteString(h, m.Text)
	}
	return h.Sum64()
}

// reply builds the deterministic word list for req: the same request always
// gets the same reply, and different prompts get different ones.
func (p *Provider) reply(req *core.Request) ([]string, core.StopReason) {
	seed := seedOf(req)
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
// words in the system prompt and every message, tool results included.
func InputTokens(req *core.Request) int {
	n := len(strings.Fields(req.System))
	for _, m := range req.Messages {
		n += len(strings.Fields(m.Text))
		for _, r := range m.ToolResults {
			n += len(strings.Fields(r.Content))
		}
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
	// call, when set, is the reply instead of words: a tool call start and
	// one arguments delta.
	call  *core.ToolCall
	stop  core.StopReason
	input int

	state  int // 0 before start, then the body events, then stop, then done
	closed bool
}

// body is the number of events between start and stop.
func (s *stream) body() int {
	if s.call != nil {
		return 2
	}
	return len(s.words)
}

// output is the output token count of the whole reply.
func (s *stream) output() int {
	if s.call != nil {
		return callTokens
	}
	return len(s.words)
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
	case s.state <= s.body():
		if s.state > 1 {
			if err := sleep(s.ctx, s.cfg.TokenInterval); err != nil {
				return core.Event{}, err
			}
		}
		i := s.state
		s.p.settle(s.entry, min(i, s.output()), false)
		s.state++
		switch {
		case s.call == nil:
			return core.Event{Kind: core.EventDelta, Text: s.words[i-1]}, nil
		case i == 1:
			return core.Event{Kind: core.EventToolCallStart, ToolIndex: 0, ToolCallID: s.call.ID, ToolName: s.call.Name}, nil
		default:
			return core.Event{Kind: core.EventToolCallDelta, ToolIndex: 0, Arguments: string(s.call.Arguments)}, nil
		}
	case s.state == s.body()+1:
		s.state++
		s.p.settle(s.entry, s.output(), true)
		return core.Event{Kind: core.EventStop, StopReason: s.stop, Usage: core.Usage{InputTokens: s.input, OutputTokens: s.output()}}, nil
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

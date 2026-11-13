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
	"github.com/TheAsianFish/switchboard/internal/cost"
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
	// ModelTokenInterval overrides TokenInterval for the models it names,
	// so one model's streams can run slower than the rest (M12's budget
	// model takes about 2 s per reply while the others keep their pace).
	ModelTokenInterval map[string]time.Duration
}

// tokenInterval is the delay between streamed deltas for model.
func (c Config) tokenInterval(model string) time.Duration {
	if d, ok := c.ModelTokenInterval[model]; ok {
		return d
	}
	return c.TokenInterval
}

type callerKey struct{}

// WithCaller returns ctx carrying the name of whoever sent the request
// (`switchboard fake-provider` sets the remote IP), which the ledger
// records so a run with several gateways can tell their requests apart.
func WithCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

func callerOf(ctx context.Context) string {
	s, _ := ctx.Value(callerKey{}).(string)
	return s
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
	// gen counts ledger resets, so a request that outlives a reset does
	// not mark an entry of the new ledger as ended.
	gen int64
	rnd *rand.Rand
	// stalled is the stall switch; transitions records every flip.
	stalled     bool
	transitions []Transition
	held        atomic.Int64
}

// Transition is one flip of the stall switch.
type Transition struct {
	Time    time.Time `json:"time"`
	Stalled bool      `json:"stalled"`
}

// SetStalled turns the stall on or off. A request that arrives while the
// provider is stalled is accepted and never answered: it is held until its
// caller gives up, even if the stall ends first, as a black-holed upstream
// holds the connections it accepted. Requests that arrive after the stall
// ends are answered as usual. Turning the switch to the state it is
// already in records nothing.
func (p *Provider) SetStalled(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stalled == on {
		return
	}
	p.stalled = on
	p.transitions = append(p.transitions, Transition{Time: time.Now(), Stalled: on})
}

// Stalled reports whether the stall switch is on.
func (p *Provider) Stalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stalled
}

// Held is the number of stalled requests whose callers have not yet given
// up.
func (p *Provider) Held() int64 { return p.held.Load() }

// hold blocks a stalled request until its caller gives up.
func (p *Provider) hold(ctx context.Context) error {
	p.held.Add(1)
	defer p.held.Add(-1)
	<-ctx.Done()
	return core.Wrap(core.KindTimeout, ctx.Err(), "request cancelled")
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
	// Ended is when the request was done with: the reply finished, or
	// the stream was closed, complete or not.
	Ended time.Time `json:"ended,omitzero"`
	// Caller is who sent the request (see WithCaller), empty if unknown.
	Caller string `json:"caller,omitempty"`
	// MaxTokens is the request's output limit, 0 if it set none.
	MaxTokens int `json:"max_tokens,omitempty"`
	// EstimatedInputTokens is the gateway's input estimate for the
	// request as received (cost.EstimateInputTokens): with MaxTokens, what
	// a gateway that forwarded it unchanged reserved against a budget.
	EstimatedInputTokens int `json:"estimated_input_tokens,omitempty"`
	// Stalled is true for a request that arrived while the stall switch
	// was on and was never answered.
	Stalled bool `json:"stalled,omitempty"`
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
	p.gen++
	p.mu.Unlock()
}

func (p *Provider) settle(i int, output int, completed bool) {
	p.mu.Lock()
	p.ledger[i].OutputTokens = output
	p.ledger[i].Completed = completed
	p.mu.Unlock()
}

// end records when request i was done with, once; a reset since it was
// recorded leaves nothing to mark.
func (p *Provider) end(i int, gen int64) {
	p.mu.Lock()
	if gen == p.gen && i < len(p.ledger) && p.ledger[i].Ended.IsZero() {
		p.ledger[i].Ended = time.Now()
	}
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

// record notes a request and returns its reply id, its ledger index, the
// ledger's generation and whether it arrived while the provider was
// stalled.
func (p *Provider) record(ctx context.Context, req *core.Request) (string, int, int64, bool) {
	p.calls.Add(1)
	c := *req
	c.Messages = append([]core.Message(nil), req.Messages...)
	p.mu.Lock()
	p.last = &c
	stalled := p.stalled
	p.ledger = append(p.ledger, Entry{Time: time.Now(), Model: req.Model, Stream: req.Stream, InputTokens: InputTokens(req), Stalled: stalled,
		Caller: callerOf(ctx), MaxTokens: req.MaxTokens, EstimatedInputTokens: cost.EstimateInputTokens(req)})
	i, gen := len(p.ledger)-1, p.gen
	p.mu.Unlock()
	return fmt.Sprintf("fake-%d", p.seq.Add(1)), i, gen, stalled
}

// Complete implements core.Provider.
func (p *Provider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	id, entry, gen, stalled := p.record(ctx, req)
	defer p.end(entry, gen)
	if stalled {
		return nil, p.hold(ctx)
	}
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
	id, entry, gen, stalled := p.record(ctx, req)
	s := &stream{
		gen:     gen,
		p:       p,
		stalled: stalled,
		entry:   entry,
		ctx:     ctx,
		cfg:     p.cfg,
		delay:   p.latency(),
		id:      id,
		model:   req.Model,
		input:   InputTokens(req),
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
	p *Provider
	// stalled: the request arrived during a stall and the stream never
	// sends anything.
	stalled bool
	entry   int
	gen     int64
	ctx     context.Context
	cfg     Config
	delay   time.Duration
	id      string
	model   string
	words   []string
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
	case s.state == 0 && s.stalled:
		return core.Event{}, s.p.hold(s.ctx)
	case s.state == 0:
		if err := sleep(s.ctx, s.delay); err != nil {
			return core.Event{}, err
		}
		s.state++
		return core.Event{Kind: core.EventStart, ID: s.id, Model: s.model, Usage: core.Usage{InputTokens: s.input}}, nil
	case s.state <= s.body():
		if s.state > 1 {
			if err := sleep(s.ctx, s.cfg.tokenInterval(s.model)); err != nil {
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
	if !s.closed {
		s.p.end(s.entry, s.gen)
	}
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

// StallHandler serves the stall switch: GET /stall returns the switch,
// the number of requests held and every transition so far; POST
// /stall/on and /stall/off flip it. `switchboard fake-provider` mounts it,
// so a benchmark can stall and recover the provider while it runs.
func (p *Provider) StallHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stall", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		body := struct {
			Stalled     bool         `json:"stalled"`
			Held        int64        `json:"held"`
			Transitions []Transition `json:"transitions"`
		}{Stalled: p.stalled, Held: p.held.Load(), Transitions: append([]Transition{}, p.transitions...)}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("POST /stall/on", func(w http.ResponseWriter, _ *http.Request) {
		p.SetStalled(true)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /stall/off", func(w http.ResponseWriter, _ *http.Request) {
		p.SetStalled(false)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

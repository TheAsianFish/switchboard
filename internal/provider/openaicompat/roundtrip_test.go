package openaicompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/api/openai"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/openaicompat"
)

// loopGateway records the request the front end parsed and answers with a
// canned reply.
type loopGateway struct {
	mu     sync.Mutex
	last   *core.Request
	events []core.Event
	resp   *core.Response
}

func (g *loopGateway) Authenticate(ctx context.Context, _ string) (context.Context, error) {
	return ctx, nil
}

func (g *loopGateway) record(req *core.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := *req
	g.last = &c
}

func (g *loopGateway) Complete(_ context.Context, req *core.Request) (*core.Response, error) {
	g.record(req)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resp != nil {
		r := *g.resp
		return &r, nil
	}
	return &core.Response{ID: "r", Model: req.Model, Text: "ok", StopReason: core.StopEndTurn}, nil
}

func (g *loopGateway) setResponse(r *core.Response) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resp = r
}

func (g *loopGateway) Stream(_ context.Context, req *core.Request) (core.Stream, error) {
	g.record(req)
	g.mu.Lock()
	defer g.mu.Unlock()
	evs := g.events
	if evs == nil {
		evs = core.ResponseEvents(&core.Response{ID: "r", Model: req.Model, StopReason: core.StopEndTurn}, []string{"ok"})
	}
	return &core.SliceStream{Events: evs}, nil
}

func (g *loopGateway) Models(context.Context) []string { return nil }

func (g *loopGateway) lastRequest() *core.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

func (g *loopGateway) setEvents(evs []core.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = evs
}

// loop wires the provider to the front end through a loopback server.
func loop(t *testing.T) (*openaicompat.Provider, *loopGateway) {
	t.Helper()
	gw := &loopGateway{}
	srv := httptest.NewServer(openai.ChatCompletions(gw))
	t.Cleanup(srv.Close)
	p, err := openaicompat.New(openaicompat.Config{BaseURL: srv.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return p, gw
}

var pieces = []string{
	"a", "Z", "0", " ", "  ", "\n", "\r\n", "\t", "\"", "\\", "/", "<", ">", "&",
	"é", "ß", "日本", "語", "😀", "👩‍💻", "\u2028", "\u00a0", "data: ", "[DONE]", "{}", "\n\n",
}

func randText(r *rand.Rand, maxPieces int) string {
	n := r.Intn(maxPieces + 1)
	var b strings.Builder
	for range n {
		b.WriteString(pieces[r.Intn(len(pieces))])
	}
	return b.String()
}

func randFloat(r *rand.Rand) *float64 {
	if r.Intn(3) == 0 {
		return nil
	}
	f := []float64{0, 1, 0.5, r.Float64() * 2}[r.Intn(4)]
	return &f
}

func randRequest(r *rand.Rand) core.Request {
	req := core.Request{
		Model:       "m-" + randText(r, 3),
		Temperature: randFloat(r),
		TopP:        randFloat(r),
	}
	if r.Intn(2) == 0 {
		req.System = randText(r, 8)
	}
	for range 1 + r.Intn(5) {
		m := core.Message{Role: core.RoleUser, Text: randText(r, 10)}
		if r.Intn(2) == 0 {
			m.Role = core.RoleAssistant
			m.ToolCalls = randCalls(r)
		} else if r.Intn(3) == 0 && !endsInResults(req.Messages) {
			// A user turn of tool results has no text in this format:
			// it is "tool" messages alone, and two in a row would be one.
			m.Text, m.ToolResults = "", randResults(r)
		}
		req.Messages = append(req.Messages, m)
	}
	req.Tools = randTools(r)
	req.ToolChoice = randChoice(r)
	if r.Intn(2) == 0 {
		req.MaxTokens = 1 + r.Intn(4096)
	}
	if r.Intn(2) == 0 {
		for range 1 + r.Intn(4) {
			req.Stop = append(req.Stop, randText(r, 3))
		}
	}
	return req
}

func endsInResults(msgs []core.Message) bool {
	return len(msgs) > 0 && len(msgs[len(msgs)-1].ToolResults) > 0
}

// randJSON is a compact JSON object holding random text, as the front end
// keeps schemas and as the provider sends them.
func randJSON(r *rand.Rand) json.RawMessage {
	obj := map[string]any{"type": "object"}
	if r.Intn(2) == 0 {
		obj["properties"] = map[string]any{randText(r, 2): map[string]any{"type": "string", "description": randText(r, 4)}}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return b
}

func randTools(r *rand.Rand) []core.Tool {
	if r.Intn(2) == 0 {
		return nil
	}
	var tools []core.Tool
	for range 1 + r.Intn(3) {
		t := core.Tool{Name: "f" + randText(r, 2), Description: randText(r, 6)}
		if r.Intn(4) > 0 {
			t.Parameters = randJSON(r)
		}
		tools = append(tools, t)
	}
	return tools
}

func randChoice(r *rand.Rand) core.ToolChoice {
	modes := []core.ToolChoiceMode{"", core.ToolChoiceAuto, core.ToolChoiceNone, core.ToolChoiceAny, core.ToolChoiceTool}
	tc := core.ToolChoice{Mode: modes[r.Intn(len(modes))], DisableParallel: r.Intn(3) == 0}
	if tc.Mode == core.ToolChoiceTool {
		tc.Name = "f" + randText(r, 2)
	}
	return tc
}

// randCalls returns tool calls whose arguments are JSON text as a client
// may write it, spacing and all: this format carries it as a string.
func randCalls(r *rand.Rand) []core.ToolCall {
	var calls []core.ToolCall
	for range r.Intn(3) {
		args := randJSON(r)
		if r.Intn(2) == 0 {
			args = json.RawMessage(`{ "a" : [1, 2.5, null, true] }`)
		}
		calls = append(calls, core.ToolCall{ID: "call_" + randText(r, 2), Name: "f" + randText(r, 2), Arguments: args})
	}
	return calls
}

func randResults(r *rand.Rand) []core.ToolResult {
	var rs []core.ToolResult
	for range 1 + r.Intn(3) {
		rs = append(rs, core.ToolResult{CallID: "call_" + randText(r, 2), Content: randText(r, 8)})
	}
	return rs
}

func TestRoundTripRequest(t *testing.T) {
	t.Parallel()
	p, gw := loop(t)
	r := rand.New(rand.NewSource(1))
	for i := range 300 {
		want := randRequest(r)
		want.Stream = i%2 == 1
		var err error
		if want.Stream {
			var s core.Stream
			if s, err = p.Stream(context.Background(), &want); err == nil {
				_, err = core.Collect(s)
			}
		} else {
			_, err = p.Complete(context.Background(), &want)
		}
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got := gw.lastRequest(); !reflect.DeepEqual(*got, want) {
			t.Fatalf("case %d:\ngot  %#v\nwant %#v", i, *got, want)
		}
	}
}

var stopReasons = []core.StopReason{core.StopEndTurn, core.StopMaxTokens, core.StopContentFilter, core.StopToolUse}

// A non-streamed reply with tool calls comes back unchanged, but for the
// id prefix and calls without arguments, which OpenAI writes as {}.
func TestRoundTripResponse(t *testing.T) {
	t.Parallel()
	p, gw := loop(t)
	r := rand.New(rand.NewSource(3))
	for i := range 200 {
		sent := &core.Response{ID: "chatcmpl-" + randText(r, 3), Model: "m" + randText(r, 2), Text: randText(r, 8), StopReason: stopReasons[r.Intn(len(stopReasons))], ToolCalls: randCalls(r)}
		if len(sent.ToolCalls) > 0 && r.Intn(4) == 0 {
			sent.ToolCalls[0].Arguments = nil
		}
		gw.setResponse(sent)
		got, err := p.Complete(context.Background(), &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "q"}}})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		want := *sent
		want.ToolCalls = append([]core.ToolCall(nil), sent.ToolCalls...)
		if len(want.ToolCalls) > 0 {
			if want.ToolCalls[0].Arguments == nil {
				want.ToolCalls[0].Arguments = json.RawMessage("{}")
			}
			if want.StopReason == core.StopEndTurn {
				want.StopReason = core.StopToolUse
			}
		}
		if !reflect.DeepEqual(got, &want) {
			t.Fatalf("case %d:\ngot  %+v\nwant %+v", i, got, &want)
		}
	}
}

// randEvents returns a well-formed stream and the events the provider should
// read back after the OpenAI wire format: empty deltas vanish, the id gains
// its prefix and the prompt usage lands on the stop event.
func randEvents(r *rand.Rand) (sent, want []core.Event) {
	id := []string{"", "chatcmpl-"}[r.Intn(2)] + "x" + randText(r, 4)
	model := "m" + randText(r, 3)
	start := core.Event{Kind: core.EventStart, ID: id, Model: model}
	if r.Intn(2) == 0 {
		start.Usage = core.Usage{InputTokens: r.Intn(1000), CacheReadTokens: r.Intn(500)}
	}
	sent = append(sent, start)
	wantID := id
	if !strings.HasPrefix(id, "chatcmpl-") {
		wantID = "chatcmpl-" + id
	}
	want = append(want, core.Event{Kind: core.EventStart, ID: wantID, Model: model})
	calls := 0
	for range r.Intn(20) {
		if r.Intn(4) == 0 {
			// A tool call, its arguments in pieces; empty pieces vanish,
			// and a call left with none reads back as "{}".
			ev := core.Event{Kind: core.EventToolCallStart, ToolIndex: calls, ToolCallID: "call_" + randText(r, 2), ToolName: "f" + randText(r, 2)}
			sent, want = append(sent, ev), append(want, ev)
			args := false
			for range r.Intn(4) {
				d := core.Event{Kind: core.EventToolCallDelta, ToolIndex: calls, Arguments: randText(r, 3)}
				sent = append(sent, d)
				if d.Arguments != "" {
					want, args = append(want, d), true
				}
			}
			if !args {
				want = append(want, core.Event{Kind: core.EventToolCallDelta, ToolIndex: calls, Arguments: "{}"})
			}
			calls++
			continue
		}
		text := randText(r, 4)
		sent = append(sent, core.Event{Kind: core.EventDelta, Text: text})
		if text != "" {
			want = append(want, core.Event{Kind: core.EventDelta, Text: text})
		}
	}
	stop := core.Event{Kind: core.EventStop, StopReason: stopReasons[r.Intn(len(stopReasons))], Usage: core.Usage{OutputTokens: r.Intn(4000)}}
	if r.Intn(2) == 0 {
		stop.Usage.InputTokens = 1 + r.Intn(1000)
		stop.Usage.CacheReadTokens = r.Intn(500)
	}
	sent = append(sent, stop)
	wantStop := stop
	if calls > 0 && stop.StopReason == core.StopEndTurn {
		// "stop" after tool calls reads as tool_use: see stopReason.
		wantStop.StopReason = core.StopToolUse
	}
	if stop.Usage.PromptTokens() == 0 {
		wantStop.Usage.InputTokens = start.Usage.InputTokens
		wantStop.Usage.CacheReadTokens = start.Usage.CacheReadTokens
	}
	want = append(want, wantStop)
	return sent, want
}

func TestRoundTripStream(t *testing.T) {
	t.Parallel()
	p, gw := loop(t)
	r := rand.New(rand.NewSource(2))
	for i := range 300 {
		sent, want := randEvents(r)
		gw.setEvents(sent)
		s, err := p.Stream(context.Background(), &core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Text: "q"}}})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		var got []core.Event
		for {
			ev, err := s.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("case %d: %v", i, err)
			}
			got = append(got, ev)
		}
		_ = s.Close()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d:\ngot  %+v\nwant %+v", i, got, want)
		}
	}
}

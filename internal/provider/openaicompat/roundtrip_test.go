package openaicompat_test

import (
	"context"
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
	return &core.Response{ID: "r", Model: req.Model, Text: "ok", StopReason: core.StopEndTurn}, nil
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
		role := core.RoleUser
		if r.Intn(2) == 0 {
			role = core.RoleAssistant
		}
		req.Messages = append(req.Messages, core.Message{Role: role, Text: randText(r, 10)})
	}
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

var stopReasons = []core.StopReason{core.StopEndTurn, core.StopMaxTokens, core.StopContentFilter}

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
	for range r.Intn(20) {
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

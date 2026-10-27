package anthropic

import (
	"context"
	"math/rand"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	anthropicapi "github.com/TheAsianFish/switchboard/internal/api/anthropic"
	"github.com/TheAsianFish/switchboard/internal/core"
)

// The round-trip tests put the provider (the SDK as a client) and the
// front end (the gateway's server side) back to back: whatever one side
// writes, the other must read back unchanged.

const roundTripCases = 300

// recordingGateway records the parsed request and replies with events.
type recordingGateway struct {
	mu     sync.Mutex
	last   *core.Request
	events []core.Event
}

func (g *recordingGateway) Authenticate(ctx context.Context, apiKey string) (context.Context, error) {
	if apiKey != secretKey {
		return nil, core.Errorf(core.KindAuthentication, "invalid x-api-key")
	}
	return ctx, nil
}

func (g *recordingGateway) Complete(_ context.Context, req *core.Request) (*core.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = req
	return &core.Response{ID: "msg_x", Model: req.Model, Text: "ok", StopReason: core.StopEndTurn}, nil
}

func (g *recordingGateway) Stream(_ context.Context, req *core.Request) (core.Stream, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = req
	evs := g.events
	if evs == nil {
		evs = []core.Event{{Kind: core.EventStart, ID: "msg_x", Model: req.Model}, {Kind: core.EventStop, StopReason: core.StopEndTurn}}
	}
	return &core.SliceStream{Events: evs}, nil
}

func (g *recordingGateway) Models(context.Context) []string { return nil }

func (g *recordingGateway) take() *core.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.last
	g.last = nil
	return r
}

func (g *recordingGateway) setEvents(evs []core.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = evs
}

// pieces are the building blocks of random text: plain words, whitespace,
// JSON metacharacters, SSE framing and multi-byte runes.
var pieces = []string{
	"a", "word", "Hello", " ", "  ", "\n", "\n\n", "\r\n", "\t", `"`, `\`, "\\n", "/", "<&>",
	"data: ", "event: x", ":", "{}", "[", "é", "漢字", "🙂", "\u2028", "\u0000", "Ω",
}

func randText(r *rand.Rand, maxPieces int) string {
	n := r.Intn(maxPieces + 1)
	var b strings.Builder
	for range n {
		b.WriteString(pieces[r.Intn(len(pieces))])
	}
	return b.String()
}

func nonEmptyText(r *rand.Rand, maxPieces int) string {
	for {
		if s := randText(r, maxPieces); s != "" {
			return s
		}
	}
}

func randRequest(r *rand.Rand) *core.Request {
	req := &core.Request{Model: nonEmptyText(r, 4)}
	if r.Intn(2) == 0 {
		req.System = randText(r, 8)
	}
	for range 1 + r.Intn(5) {
		role := core.RoleUser
		if r.Intn(2) == 0 {
			role = core.RoleAssistant
		}
		req.Messages = append(req.Messages, core.Message{Role: role, Text: randText(r, 12)})
	}
	if r.Intn(3) > 0 {
		req.MaxTokens = 1 + r.Intn(200000)
	}
	if r.Intn(2) == 0 {
		v := r.Float64()
		req.Temperature = &v
	}
	if r.Intn(2) == 0 {
		v := r.Float64()
		req.TopP = &v
	}
	if r.Intn(2) == 0 {
		for range 1 + r.Intn(3) {
			req.Stop = append(req.Stop, nonEmptyText(r, 3))
		}
	}
	return req
}

func TestRoundTripRequest(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	srv := httptest.NewServer(anthropicapi.Messages(gw))
	defer srv.Close()
	p := newProvider(t, srv, 0)
	r := rand.New(rand.NewSource(1))

	for i := range roundTripCases {
		req := randRequest(r)
		want := *req
		if want.MaxTokens == 0 {
			want.MaxTokens = defaultMaxTokens
		}
		if i%2 == 0 {
			if _, err := p.Complete(context.Background(), req); err != nil {
				t.Fatalf("case %d: complete: %v", i, err)
			}
		} else {
			want.Stream = true
			s, err := p.Stream(context.Background(), req)
			if err != nil {
				t.Fatalf("case %d: stream: %v", i, err)
			}
			if _, err := core.Collect(s); err != nil {
				t.Fatalf("case %d: collect: %v", i, err)
			}
		}
		got := gw.take()
		if got == nil {
			t.Fatalf("case %d: the front end saw no request", i)
		}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("case %d:\n got %#v\nwant %#v", i, *got, want)
		}
	}
}

var stopReasons = []core.StopReason{core.StopEndTurn, core.StopMaxTokens, core.StopSequence, core.StopContentFilter}

func randUsage(r *rand.Rand) core.Usage {
	field := func() int {
		if r.Intn(3) == 0 {
			return 0
		}
		return 1 + r.Intn(100000)
	}
	return core.Usage{InputTokens: field(), CacheReadTokens: field(), CacheWriteTokens: field()}
}

// randEvents builds a well-formed canonical stream. The stop event's input
// counts either repeat the start event's or replace them with non-zero
// values, the two shapes upstreams send.
func randEvents(r *rand.Rand) []core.Event {
	id := nonEmptyText(r, 3)
	if r.Intn(2) == 0 {
		id = "msg_" + id
	}
	start := core.Event{Kind: core.EventStart, ID: id, Model: nonEmptyText(r, 3), Usage: randUsage(r)}
	evs := []core.Event{start}
	for range r.Intn(20) {
		evs = append(evs, core.Event{Kind: core.EventDelta, Text: nonEmptyText(r, 5)})
	}
	final := start.Usage
	if r.Intn(2) == 0 {
		final = core.Usage{InputTokens: 1 + r.Intn(1000), CacheReadTokens: 1 + r.Intn(1000), CacheWriteTokens: 1 + r.Intn(1000)}
	}
	final.OutputTokens = r.Intn(100000)
	return append(evs, core.Event{Kind: core.EventStop, StopReason: stopReasons[r.Intn(len(stopReasons))], Usage: final})
}

func TestRoundTripStream(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	srv := httptest.NewServer(anthropicapi.Messages(gw))
	defer srv.Close()
	p := newProvider(t, srv, 0)
	r := rand.New(rand.NewSource(2))

	for i := range roundTripCases {
		evs := randEvents(r)
		gw.setEvents(evs)
		want := append([]core.Event(nil), evs...)
		if !strings.HasPrefix(want[0].ID, "msg_") {
			want[0].ID = "msg_" + want[0].ID
		}
		s, err := p.Stream(context.Background(), testRequest())
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		got, err := collect(t, s)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d:\n got %#v\nwant %#v", i, got, want)
		}
	}
}

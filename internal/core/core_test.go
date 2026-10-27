package core

import (
	"errors"
	"fmt"
	"testing"
)

func TestCollect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		events  []Event
		want    Response
		wantErr bool
	}{
		{
			name: "input usage from start when stop omits it",
			events: []Event{
				{Kind: EventStart, ID: "a", Model: "m", Usage: Usage{InputTokens: 5, CacheReadTokens: 2}},
				{Kind: EventDelta, Text: "he"}, {Kind: EventDelta, Text: "llo"},
				{Kind: EventStop, StopReason: StopMaxTokens, Usage: Usage{OutputTokens: 2}},
			},
			want: Response{ID: "a", Model: "m", Text: "hello", StopReason: StopMaxTokens, Usage: Usage{InputTokens: 5, OutputTokens: 2, CacheReadTokens: 2}},
		},
		{
			name: "stop usage wins when present",
			events: []Event{
				{Kind: EventStart, Usage: Usage{InputTokens: 12}},
				{Kind: EventStop, StopReason: StopEndTurn, Usage: Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
			},
			want: Response{StopReason: StopEndTurn, Usage: Usage{InputTokens: 13, CacheReadTokens: 24, OutputTokens: 6}},
		},
		{name: "no stop event", events: []Event{{Kind: EventStart}, {Kind: EventDelta, Text: "x"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Collect(&SliceStream{Events: tt.events})
			if tt.wantErr {
				if AsError(err).Kind != KindUpstream {
					t.Fatalf("err = %v, want an upstream error", err)
				}
				return
			}
			if err != nil || *got != tt.want {
				t.Fatalf("Collect = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestResponseEventsRoundTrip(t *testing.T) {
	t.Parallel()
	r := &Response{ID: "x", Model: "m", Text: "abc", StopReason: StopEndTurn, Usage: Usage{InputTokens: 3, OutputTokens: 2, CacheWriteTokens: 1}}
	got, err := Collect(&SliceStream{Events: ResponseEvents(r, []string{"a", "bc"})})
	if err != nil || *got != *r {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestAsError(t *testing.T) {
	t.Parallel()
	cause := errors.New("secret detail")
	e := AsError(fmt.Errorf("wrapped: %w", Wrap(KindRateLimited, cause, "slow down")))
	if e.Kind != KindRateLimited || e.Message != "slow down" || !errors.Is(e, cause) {
		t.Errorf("classified error = %+v", e)
	}
	plain := AsError(cause)
	if plain.Kind != KindInternal || plain.Message != "internal error" {
		t.Errorf("unclassified error = %+v, want an internal error that hides its cause", plain)
	}
	for status, want := range map[int]ErrorKind{400: KindInvalidRequest, 401: KindAuthentication, 404: KindNotFound, 429: KindRateLimited, 500: KindUpstream, 529: KindOverloaded, 504: KindTimeout} {
		if got := KindFromStatus(status); got != want {
			t.Errorf("KindFromStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

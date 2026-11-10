package cache_test

import (
	"slices"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
)

func TestDecide(t *testing.T) {
	t.Parallel()
	hot := 0.7
	tests := []struct {
		name      string
		key       cache.Mode
		directive string
		temp      *float64
		want      cache.Mode
		wantErr   bool
	}{
		{"temperature 0 gets the key's mode", cache.ModeSemantic, "", zero(), cache.ModeSemantic, false},
		{"unset temperature is not cached", cache.ModeSemantic, "", nil, cache.ModeOff, false},
		{"sampling temperature is not cached", cache.ModeSemantic, "", &hot, cache.ModeOff, false},
		{"directive overrides the temperature rule", cache.ModeSemantic, "semantic", &hot, cache.ModeSemantic, false},
		{"directive lowers the mode", cache.ModeSemantic, "exact", zero(), cache.ModeExact, false},
		{"directive turns it off", cache.ModeSemantic, "off", zero(), cache.ModeOff, false},
		{"directive never raises the key's mode", cache.ModeExact, "semantic", zero(), cache.ModeExact, false},
		{"directive cannot turn on a key that is off", cache.ModeOff, "semantic", zero(), cache.ModeOff, false},
		{"directive is case-insensitive", cache.ModeSemantic, " Exact ", nil, cache.ModeExact, false},
		{"unknown directive is a client error", cache.ModeSemantic, "always", zero(), cache.ModeOff, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &core.Request{Temperature: tt.temp, Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}
			d, err := cache.Decide(tt.key, tt.directive, r, 0)
			if tt.wantErr {
				if err == nil || core.AsError(err).Kind != core.KindInvalidRequest {
					t.Fatalf("err = %v, want an invalid-request error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Mode != tt.want {
				t.Errorf("mode = %s (%s), want %s", d.Mode, d.Reason, tt.want)
			}
			if d.Mode < tt.key && d.Reason == "" {
				t.Error("lowered without a reason")
			}
		})
	}
}

func TestDecideToolContent(t *testing.T) {
	t.Parallel()
	q := core.Message{Role: core.RoleUser, Text: "weather?"}
	call := core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "c", Name: "f"}}}
	result := core.Message{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "c", Content: "sunny"}}}
	tests := []struct {
		name string
		key  cache.Mode
		req  core.Request
		want cache.Mode
	}{
		{"tools", cache.ModeSemantic, core.Request{Messages: []core.Message{q}, Tools: []core.Tool{{Name: "f"}}}, cache.ModeExact},
		{"tool choice", cache.ModeSemantic, core.Request{Messages: []core.Message{q}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceNone}}, cache.ModeExact},
		{"tool result last", cache.ModeSemantic, core.Request{Messages: []core.Message{q, call, result}}, cache.ModeExact},
		{"tool call earlier", cache.ModeSemantic, core.Request{Messages: []core.Message{q, call, result, {Role: core.RoleAssistant, Text: "sunny"}, q}}, cache.ModeExact},
		{"exact key unchanged", cache.ModeExact, core.Request{Messages: []core.Message{q}, Tools: []core.Tool{{Name: "f"}}}, cache.ModeExact},
		{"no tools keeps semantic", cache.ModeSemantic, core.Request{Messages: []core.Message{q}}, cache.ModeSemantic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.req
			r.Temperature = zero()
			d, err := cache.Decide(tt.key, "", &r, 0)
			if err != nil {
				t.Fatal(err)
			}
			if d.Mode != tt.want {
				t.Errorf("mode = %s (%s), want %s", d.Mode, d.Reason, tt.want)
			}
			if tt.key == cache.ModeSemantic && tt.want == cache.ModeExact && d.Reason != cache.ReasonToolContent {
				t.Errorf("reason = %q, want %q", d.Reason, cache.ReasonToolContent)
			}
		})
	}
}

func TestDecideMaxTemperature(t *testing.T) {
	t.Parallel()
	low := 0.2
	r := &core.Request{Temperature: &low, Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}
	for maxT, want := range map[float64]cache.Mode{0: cache.ModeOff, 0.2: cache.ModeExact, 0.5: cache.ModeExact} {
		if d, _ := cache.Decide(cache.ModeExact, "", r, maxT); d.Mode != want {
			t.Errorf("max %g: %s, want %s", maxT, d.Mode, want)
		}
	}
}

func TestNumbers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{"no numbers here", nil},
		{"Janet has 16 eggs and eats three.", []string{"16", "3"}},
		{"It costs $1,200.50 or 1,5 kg", []string{"1200.50", "1", "5"}},
		{"A 50% rise, 007 agents, 2.0 times", []string{"50", "007", "2.0"}},
		{"Version 1.2.3 and twenty-one", []string{"1.2.3", "20", "1"}},
		{"Ends with a number 42.", []string{"42"}},
		{"-40 C, a-1 and 3-2", []string{"-40", "1", "3", "2"}},
		{"the 2nd and the second, 21st", []string{"2th", "2th", "21th"}},
		{"1,2345 and 12,000,000", []string{"1", "2345", "12000000"}},
	}
	for _, tt := range tests {
		if got := cache.Numbers(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("Numbers(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The guard errs towards "different": each pair here embeds almost alike
// but asks something else, and must not count as the same numbers.
func TestSameNumbers(t *testing.T) {
	t.Parallel()
	same := [][2]string{
		{"3 apples and two pears", "three apples and 2 pears"},
		{"It costs $1,200", "It costs $1200"},
		{"the 2nd floor", "the second floor"},
	}
	different := [][2]string{
		{"10 divided by 2", "2 divided by 10"},
		{"-40 C", "40 C"},
		{"Go 1.20", "Go 1.2"},
		{"1,5 kg", "15 kg"},
		{"007", "7"},
		{"2024-01-05", "2024-05-01"},
		{"the second runner", "the third runner"},
		{"3 apples", "3 apples and 3 pears"},
	}
	for _, p := range same {
		if !cache.SameNumbers(p[0], p[1]) {
			t.Errorf("SameNumbers(%q, %q) = false", p[0], p[1])
		}
	}
	for _, p := range different {
		if cache.SameNumbers(p[0], p[1]) {
			t.Errorf("SameNumbers(%q, %q) = true", p[0], p[1])
		}
	}
}

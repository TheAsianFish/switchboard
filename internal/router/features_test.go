package router

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
)

func feature(t *testing.T, x []float64, name string) float64 {
	t.Helper()
	i := slices.Index(CheapFeatureNames, name)
	if i < 0 {
		t.Fatalf("no feature %q", name)
	}
	return x[i]
}

func TestCheap(t *testing.T) {
	t.Parallel()
	l := math.Log1p
	tests := []struct {
		name string
		text string
		want map[string]float64
	}{
		{"empty", "", map[string]float64{
			"log_chars": 0, "log_words": 0, "numbers": 0, "number_density": 0,
			"has_code_fence": 0, "question_words": 0, "has_choices": 0, "math_ops": 0, "lines": 0,
		}},
		{"words and chars", "héllo wörld", map[string]float64{
			"log_chars": l(11), "log_words": l(2), "lines": l(1), "numbers": 0,
		}},
		{"numbers", "Tom has 3 apples and 12.5 pears, 1,000 total.", map[string]float64{
			"numbers": l(3), "number_density": 3.0 / 9, "math_ops": 0,
		}},
		{"density capped", "1+2+3", map[string]float64{
			"numbers": l(3), "number_density": 1, "math_ops": l(2),
		}},
		{"math ops", "If x = 5 and y = 3 * 2, cost $40 or 10% off; well-known a/b", map[string]float64{
			"math_ops": l(5),
		}},
		{"question words", "What is it? Why, and how? Somewhat whatever.", map[string]float64{
			"question_words": l(3),
		}},
		{"code fence", "fix this\n```go\nx := 1\n```", map[string]float64{
			"has_code_fence": 1, "lines": l(4),
		}},
		{"choices", "Which is red?\n\nA. apple\nB. sky\nC) grass\n(D) coal", map[string]float64{
			"has_choices": 1, "question_words": l(1), "lines": l(6),
		}},
		{"one choice line is not a list", "A. Lincoln was president.", map[string]float64{
			"has_choices": 0,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			x := Cheap(tt.text)
			if len(x) != len(CheapFeatureNames) {
				t.Fatalf("len %d, want %d", len(x), len(CheapFeatureNames))
			}
			for name, want := range tt.want {
				if got := feature(t, x, name); math.Abs(got-want) > 1e-12 {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
		})
	}
}

func TestCheapFiniteAndDeterministic(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"",
		"   \n\n\t ",
		strings.Repeat("What is 12 * 34 = ? ", 20_000),
		"日本語のテキスト 😀 ١٢٣ ÷ × −",
		"\xff\xfe invalid utf-8 $",
		"$", "-", "5", "```",
	}
	for _, in := range inputs {
		a, b := Cheap(in), Cheap(in)
		if !slices.Equal(a, b) {
			t.Errorf("Cheap(%.20q) not deterministic", in)
		}
		for i, v := range a {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Errorf("Cheap(%.20q)[%s] = %v", in, CheapFeatureNames[i], v)
			}
		}
	}
}

func TestVector(t *testing.T) {
	t.Parallel()
	if got := Vector([]float64{1, 2}, nil); !slices.Equal(got, []float64{1, 2}) {
		t.Errorf("cheap only: %v", got)
	}
	if got := Vector([]float64{1}, []float32{0.5, -0.25}); !slices.Equal(got, []float64{1, 0.5, -0.25}) {
		t.Errorf("with embedding: %v", got)
	}
}

func TestPromptText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  *core.Request
		want string
	}{
		{"nil", nil, ""},
		{"no messages, system only", &core.Request{System: "be nice"}, ""},
		{"last user wins", &core.Request{
			System: "sys",
			Messages: []core.Message{
				{Role: core.RoleUser, Text: "first"},
				{Role: core.RoleAssistant, Text: "reply"},
				{Role: core.RoleUser, Text: "second"},
			},
		}, "second"},
		{"assistant last is ignored", &core.Request{
			Messages: []core.Message{
				{Role: core.RoleUser, Text: "question"},
				{Role: core.RoleAssistant, Text: "prefill"},
			},
		}, "question"},
		{"tool-result turn skipped", &core.Request{
			Messages: []core.Message{
				{Role: core.RoleUser, Text: "weather in Oslo?"},
				{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "1", Name: "weather"}}},
				{Role: core.RoleUser, ToolResults: []core.ToolResult{{CallID: "1", Content: "rain"}}},
			},
		}, "weather in Oslo?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := PromptText(tt.req); got != tt.want {
				t.Errorf("PromptText = %q, want %q", got, tt.want)
			}
		})
	}
}

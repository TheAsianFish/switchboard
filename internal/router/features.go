// Package router picks a model for requests that ask for model "auto".
//
// Each candidate model has a logistic regression that predicts the chance
// it answers a prompt correctly, from a few hand-made text features and,
// optionally, the prompt's embedding. The regressions are trained offline
// by the router eval and shipped as a JSON weights file; at serving time
// the router sends a request to the cheapest candidate whose predicted
// chance clears a quality target, and to the most accurate candidate when
// none does.
package router

import (
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// CheapFeatureNames names the hand-made features Cheap returns, in order.
// A weights file records the list it was trained with and is refused when
// it differs, so changing a feature here means retraining.
//
// Counts are log1p'd: the difference between 0 and 3 numbers matters
// more than between 30 and 33, and it keeps long prompts from dominating
// a linear model.
var CheapFeatureNames = []string{
	"log_chars",      // log1p of the rune count
	"log_words",      // log1p of the whitespace-separated word count
	"numbers",        // log1p of the count of numbers (digit runs, with . or , inside)
	"number_density", // numbers per word, capped at 1
	"has_code_fence", // 1 if the text contains ``` , else 0
	"question_words", // log1p of the count of what/why/how/which/who/whom/whose/when/where
	"has_choices",    // 1 if at least two lines start like "A. ", "B) ", "(C) " (letters A to E), else 0
	"math_ops",       // log1p of the count of + - * / = % ^ × ÷ next to a digit, and $ before one
	"lines",          // log1p of the line count
}

var (
	numberRE = regexp.MustCompile(`[0-9]+(?:[.,][0-9]+)*`)
	choiceRE = regexp.MustCompile(`^\s*\(?([A-E])[.):]\s+\S`)
)

var questionWords = map[string]bool{
	"what": true, "why": true, "how": true, "which": true, "who": true,
	"whom": true, "whose": true, "when": true, "where": true,
}

// Cheap computes the hand-made features of text, in CheapFeatureNames
// order. It is deterministic and every value is finite, for any input.
func Cheap(text string) []float64 {
	words := strings.Fields(text)
	numbers := len(numberRE.FindAllStringIndex(text, -1))

	density := 0.0
	if len(words) > 0 {
		density = math.Min(1, float64(numbers)/float64(len(words)))
	}

	fence := 0.0
	if strings.Contains(text, "```") {
		fence = 1
	}

	qwords := 0
	for _, w := range words {
		w = strings.ToLower(strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) }))
		if questionWords[w] {
			qwords++
		}
	}

	lines := 0
	if text != "" {
		lines = strings.Count(text, "\n") + 1
	}

	return []float64{
		math.Log1p(float64(utf8.RuneCountInString(text))),
		math.Log1p(float64(len(words))),
		math.Log1p(float64(numbers)),
		density,
		fence,
		math.Log1p(float64(qwords)),
		choices(text),
		math.Log1p(float64(mathOps(text))),
		math.Log1p(float64(lines)),
	}
}

// choices is 1 when the text lists multiple-choice options: two or more
// lines starting with distinct option letters. One such line alone is
// more likely a sentence that starts with "A." than a list.
func choices(text string) float64 {
	var seen [5]bool
	n := 0
	for line := range strings.SplitSeq(text, "\n") {
		m := choiceRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		i := m[1][0] - 'A'
		if !seen[i] {
			seen[i] = true
			n++
		}
	}
	if n >= 2 {
		return 1
	}
	return 0
}

// mathOps counts arithmetic in the text: an operator whose nearest
// neighbour on either side (skipping up to three spaces) is a digit, and
// a $ followed by a digit. Requiring a digit keeps hyphens in words and
// slashes in paths out of the count.
func mathOps(text string) int {
	rs := []rune(text)
	n := 0
	for i, r := range rs {
		switch r {
		case '$':
			if digitNear(rs, i, 1) {
				n++
			}
		case '+', '-', '*', '/', '=', '%', '^', '×', '÷':
			if digitNear(rs, i, -1) || digitNear(rs, i, 1) {
				n++
			}
		}
	}
	return n
}

// digitNear reports whether the first non-space rune from i in direction
// dir, within three spaces, is an ASCII digit.
func digitNear(rs []rune, i, dir int) bool {
	for j, skipped := i+dir, 0; j >= 0 && j < len(rs); j += dir {
		if rs[j] == ' ' && skipped < 3 {
			skipped++
			continue
		}
		return rs[j] >= '0' && rs[j] <= '9'
	}
	return false
}

// Vector is the model input: the cheap features followed by the
// embedding. emb is nil for the cheap-only variant.
func Vector(cheap []float64, emb []float32) []float64 {
	x := make([]float64, 0, len(cheap)+len(emb))
	x = append(x, cheap...)
	for _, v := range emb {
		x = append(x, float64(v))
	}
	return x
}

// PromptText is the text the router reads from a request: the text of
// the last user turn that has any. The system prompt is left out because
// it is usually the same for every request from an application and says
// little about how hard this one is; assistant turns are the model's own
// words. A user turn that carries only tool results is skipped, so a
// tool-use loop is routed on the question that started it.
func PromptText(req *core.Request) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == core.RoleUser && strings.TrimSpace(m.Text) != "" {
			return m.Text
		}
	}
	return ""
}

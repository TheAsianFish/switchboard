package routereval

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Grade reads the answer from a reply and says whether it is right.
func Grade(q Question, output string) (extracted string, correct bool) {
	if q.Dataset == DatasetGSM8K {
		got := gsmAnswer(output)
		return got, sameNumber(got, q.Answer)
	}
	got := mmluLetter(output)
	return got, got != "" && got == q.Answer
}

var (
	boxed = regexp.MustCompile(`\\boxed\{([^{}]*)\}`)
	// answerLine finds the line's last "Answer:" (any case) and what
	// follows it.
	answerLine = regexp.MustCompile(`(?i).*answer\s*:(.*)`)
)

// gsmAnswer reads the final number of a GSM8K reply, in this order:
//
//   - the last line with "Answer:": the first number after its last "="
//     if it has one ("Answer: 9 x 2 = 18"), else its first number
//     ("Answer: $18 per day for 7 days"), else the first number on the
//     next non-empty line (a bold "**Answer:**" on a line of its own);
//   - else the last \boxed{...}: small models often box their result
//     and ignore the requested format;
//   - else the last number in the reply, the usual fallback for GSM8K.
//
// Markdown emphasis is removed first, so "**Answer**: 18" counts.
func gsmAnswer(out string) string {
	out = strings.ReplaceAll(out, "*", "")
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		m := answerLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		rest := m[1]
		if k := strings.LastIndex(rest, "="); k >= 0 {
			rest = rest[k+1:]
		}
		if ns := numbers(rest); len(ns) > 0 {
			return ns[0]
		}
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) == "" {
				continue
			}
			if ns := numbers(next); len(ns) > 0 {
				return ns[0]
			}
			break
		}
	}
	if m := boxed.FindAllStringSubmatch(out, -1); len(m) > 0 {
		if ns := numbers(m[len(m)-1][1]); len(ns) > 0 {
			return ns[len(ns)-1]
		}
	}
	if ns := numbers(out); len(ns) > 0 {
		return ns[len(ns)-1]
	}
	return ""
}

// numbers lists the numbers in s in order, thousands separators dropped,
// a leading minus kept and a trailing full stop not taken as a decimal
// point.
func numbers(s string) []string {
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if !unicode.IsDigit(rs[i]) {
			continue
		}
		var b strings.Builder
		// A minus sign, not a hyphen as in "2023-05" or "7-day".
		if i > 0 && rs[i-1] == '-' && (i < 2 || !unicode.IsLetter(rs[i-2]) && !unicode.IsDigit(rs[i-2])) {
			b.WriteRune('-')
		}
		j := i
		for j < len(rs) && (unicode.IsDigit(rs[j]) || ((rs[j] == ',' || rs[j] == '.') && j+1 < len(rs) && unicode.IsDigit(rs[j+1]))) {
			if rs[j] != ',' {
				b.WriteRune(rs[j])
			}
			j++
		}
		out = append(out, b.String())
		i = j
	}
	return out
}

// sameNumber compares two numbers by value, so "18.00" equals "18".
func sameNumber(a, b string) bool {
	x, err1 := strconv.ParseFloat(strings.ReplaceAll(a, ",", ""), 64)
	y, err2 := strconv.ParseFloat(strings.ReplaceAll(b, ",", ""), 64)
	return err1 == nil && err2 == nil && math.Abs(x-y) < 1e-9
}

var (
	// "the answer is C", "Answer: (B)": the keyword in any case, the
	// letter a capital.
	letterAnswer = regexp.MustCompile(`(?i:answer)\s*(?:(?i:is)\s*)?:?\s*\(?([ABCD])\b`)
	// "option B", "choice (D)".
	letterOption = regexp.MustCompile(`(?i:option|choice)\s*(?:(?i:is)\s*)?:?\s*\(?([ABCD])\b`)
	// A reply that starts with its letter: "B", "B.", "(B)", "B) ...",
	// "B, because", "B is correct", "B - ...", but not the article in
	// "A polynomial is ...".
	letterFirst = regexp.MustCompile(`^\s*\(?([ABCD])(?:[).:,]|\s+(?:is\b|-)|\s*$)`)
	boxedLetter = regexp.MustCompile(`\\boxed\{\s*\(?([ABCD])\)?\s*\}`)
)

// mmluLetter is the option letter a reply chose: the last letter named
// after "answer", else the first line's leading letter, else a boxed
// letter, else the last letter after "option" or "choice". Markdown
// emphasis is removed first. A reply with none of these is graded wrong.
func mmluLetter(out string) string {
	s := strings.ReplaceAll(out, "*", "")
	if m := letterAnswer.FindAllStringSubmatch(s, -1); len(m) > 0 {
		return m[len(m)-1][1]
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := letterFirst.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		break
	}
	if m := boxedLetter.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := letterOption.FindAllStringSubmatch(s, -1); len(m) > 0 {
		return m[len(m)-1][1]
	}
	return ""
}

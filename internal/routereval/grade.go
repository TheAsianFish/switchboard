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

var boxed = regexp.MustCompile(`\\boxed\{([^{}]*)\}`)

// gsmAnswer is the number on the reply's last "Answer:" line, else the
// last \boxed{...}, else the last number in the reply. Small models often
// ignore the requested format and box their result instead.
func gsmAnswer(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.ToLower(lines[i])
		if k := strings.LastIndex(l, "answer:"); k >= 0 {
			if ns := numbers(lines[i][k:]); len(ns) > 0 {
				return ns[len(ns)-1]
			}
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
		if i > 0 && rs[i-1] == '-' {
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
	// "Answer: B", "answer is (B)", "option B".
	letterAfter = regexp.MustCompile(`(?i)(?:answer|option|choice)\s*(?:is|:)?\s*:?\s*\(?\s*([ABCD])\b`)
	// A letter standing alone: "B", "B.", "(B)", "B) ...", but not the
	// article in "A polynomial is ...".
	letterAlone = regexp.MustCompile(`(?:^|[^A-Za-z])\(?([ABCD])(?:[).:]|\s*$)`)
)

// mmluLetter is the option letter a reply chose: the letter after
// "answer"/"option" if the reply names one, else the first capital A to D
// standing alone. A reply with no such letter is graded wrong.
func mmluLetter(out string) string {
	s := strings.TrimSpace(out)
	if m := letterAfter.FindStringSubmatch(s); m != nil {
		return strings.ToUpper(m[1])
	}
	if m := letterAlone.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

package cache

import (
	"slices"
	"strings"
	"unicode"
)

// numberWords maps spelled-out numbers to digits, so "three eggs" and
// "3 eggs" carry the same number. Compounds ("two hundred") come out as
// their parts ("2", "100"), which is enough to tell two prompts apart and
// errs towards treating a rewording as different, never the same.
var numberWords = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
	"six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10",
	"eleven": "11", "twelve": "12", "thirteen": "13", "fourteen": "14", "fifteen": "15",
	"sixteen": "16", "seventeen": "17", "eighteen": "18", "nineteen": "19",
	"twenty": "20", "thirty": "30", "forty": "40", "fifty": "50", "sixty": "60",
	"seventy": "70", "eighty": "80", "ninety": "90", "hundred": "100",
	"thousand": "1000", "million": "1000000", "billion": "1000000000",

	"first": "1th", "second": "2th", "third": "3th", "fourth": "4th", "fifth": "5th",
	"sixth": "6th", "seventh": "7th", "eighth": "8th", "ninth": "9th", "tenth": "10th",
	"eleventh": "11th", "twelfth": "12th", "thirteenth": "13th", "fourteenth": "14th",
	"fifteenth": "15th", "sixteenth": "16th", "seventeenth": "17th", "eighteenth": "18th",
	"nineteenth": "19th", "twentieth": "20th", "hundredth": "100th", "thousandth": "1000th",
}

// Numbers returns the numbers in text, in the order they appear: digit
// runs as written (a sign, leading zeros and decimals kept; only a comma
// before exactly three digits is dropped as a thousands separator), digit
// ordinals ("2nd") and spelled-out numbers and ordinals. The rules lean
// towards calling two numbers different: for the cache a missed hit costs
// one upstream call, a false one a wrong answer.
func Numbers(text string) []string {
	var out []string
	rs := []rune(text)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsDigit(r):
			var b strings.Builder
			if i > 0 && (rs[i-1] == '-' || rs[i-1] == '−') && (i == 1 || !isAlnum(rs[i-2])) {
				b.WriteRune('-')
			}
			j := i
		scan:
			for j < len(rs) {
				c := rs[j]
				switch {
				case unicode.IsDigit(c):
					b.WriteRune(c)
				case c == ',' && thousandsGroup(rs, j+1):
				case c == '.' && j+1 < len(rs) && unicode.IsDigit(rs[j+1]):
					b.WriteRune('.')
				default:
					break scan
				}
				j++
			}
			if k := ordinalSuffix(rs, j); k > j {
				b.WriteString("th")
				j = k
			}
			out = append(out, b.String())
			i = j
		case unicode.IsLetter(r):
			j := i
			for j < len(rs) && unicode.IsLetter(rs[j]) {
				j++
			}
			if d, ok := numberWords[strings.ToLower(string(rs[i:j]))]; ok {
				out = append(out, d)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// thousandsGroup reports whether rs[i:] starts with exactly three digits,
// as after the comma in 1,200 (and not in 1,5 or 1,2345).
func thousandsGroup(rs []rune, i int) bool {
	if i+3 > len(rs) {
		return false
	}
	for k := i; k < i+3; k++ {
		if !unicode.IsDigit(rs[k]) {
			return false
		}
	}
	return i+3 == len(rs) || !unicode.IsDigit(rs[i+3])
}

// ordinalSuffix returns the index after an "st", "nd", "rd" or "th" that
// starts at i and ends a word, or i if there is none.
func ordinalSuffix(rs []rune, i int) int {
	if i+2 > len(rs) || (i+2 < len(rs) && unicode.IsLetter(rs[i+2])) {
		return i
	}
	switch strings.ToLower(string(rs[i : i+2])) {
	case "st", "nd", "rd", "th":
		return i + 2
	}
	return i
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// SameNumbers reports whether a and b contain the same numbers in the
// same order. Order matters because "10 divided by 2" and "2 divided by
// 10" embed almost alike; a rewording that moves the numbers around costs
// a hit, which RESULTS.md (Q3) measures.
func SameNumbers(a, b string) bool {
	return slices.Equal(Numbers(a), Numbers(b))
}

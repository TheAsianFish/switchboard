package routereval

import "testing"

func TestGradeGSM8K(t *testing.T) {
	t.Parallel()
	q := Question{Dataset: DatasetGSM8K, Answer: "1200"}
	tests := []struct {
		name, out, want string
		correct         bool
	}{
		{"answer line", "3 * 400 = 1200\nAnswer: 1200", "1200", true},
		{"answer line wins over later numbers", "Answer: 1,200\n(checked with 3 and 4)", "1200", true},
		{"answer line with dollar and full stop", "So: Answer: $1,200.", "1200", true},
		{"boxed when no answer line", "We get \\boxed{1200} in total, 5 boxes", "1200", true},
		{"last number otherwise", "first 3, then 1200", "1200", true},
		{"decimal equal by value", "Answer: 1200.00", "1200.00", true},
		{"wrong", "Answer: 1100", "1100", false},
		{"no number", "I cannot tell.", "", false},
		{"negative kept", "Answer: -1200", "-1200", false},
		{"first number on the answer line", "Answer: $1200 per day for 7 days", "1200", true},
		{"after the last equals sign", "Answer: 3 x 400 = 1200", "1200", true},
		{"answer line then a working note", "Answer: 1200 (from 3 x 400)", "1200", true},
		{"bold answer heading on its own line", "**Answer:**\nJohn makes **$1200** if he works a full 7-day week", "1200", true},
		{"bold answer keyword", "**Answer**: 1200\nThen 16 - 3 = 13", "1200", true},
		{"a hyphen is not a minus", "It is the 2023-1200 season", "1200", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Grade(q, tt.out)
			if got != tt.want || ok != tt.correct {
				t.Errorf("Grade(%q) = %q, %v; want %q, %v", tt.out, got, ok, tt.want, tt.correct)
			}
		})
	}
}

func TestGradeMMLU(t *testing.T) {
	t.Parallel()
	q := Question{Dataset: DatasetMMLU, Answer: "C"}
	tests := []struct {
		name, out, want string
	}{
		{"bare letter", "C", "C"},
		{"letter and full stop", "C.", "C"},
		{"letter and option text", "C. we can refer only to how people describe", "C"},
		{"parenthesised", "(C)", "C"},
		{"answer is", "The answer is C because", "C"},
		{"answer colon", "Answer: (B)", "B"},
		{"letter then an explanation", "C\n\nExplanation: because", "C"},
		{"bold letter", "**C**", "C"},
		{"letter and comma", "C, because", "C"},
		{"letter is correct", "C is correct", "C"},
		{"boxed letter", "\\boxed{C}", "C"},
		{"the last answer wins", "Option A is wrong; the answer is C", "C"},
		{"lower-case article after answer is not a letter", "The answer is a bit unclear", ""},
		{"article is not a letter", "A polynomial is irreducible when", ""},
		{"lower case is not a letter", "c", ""},
		{"nothing", "To determine whether", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Grade(q, tt.out)
			if got != tt.want || ok != (tt.want == "C") {
				t.Errorf("Grade(%q) = %q, %v; want %q", tt.out, got, ok, tt.want)
			}
		})
	}
}

func TestPrompt(t *testing.T) {
	t.Parallel()
	sys, user := Prompt(Question{Dataset: DatasetMMLU, Question: " Which? ", Choices: []string{"w", "x", "y", "z"}})
	if sys != systemMMLU || user != "Which?\n\nA. w\nB. x\nC. y\nD. z" {
		t.Errorf("MMLU prompt = %q, %q", sys, user)
	}
	sys, user = Prompt(Question{Dataset: DatasetGSM8K, Question: "How many?"})
	if sys != systemGSM8K || user != "How many?" {
		t.Errorf("GSM8K prompt = %q, %q", sys, user)
	}
}

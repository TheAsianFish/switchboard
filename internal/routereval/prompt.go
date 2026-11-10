package routereval

import (
	"strings"
)

// The system prompts tell the model the answer format the grader reads.
// The question itself is the user turn, as a client would send it, so the
// router's features at serving time see what they saw in training.
const (
	systemGSM8K = "Solve the math word problem. Reason briefly, step by step, then give the final answer on the last line as: Answer: <number>"
	systemMMLU  = "Answer the multiple-choice question. Reply with the letter of the correct option only (A, B, C or D)."
)

// MaxTokens caps a reply per dataset: room for a short worked solution on
// GSM8K, a letter and a few words on MMLU.
func MaxTokens(dataset string) int {
	if dataset == DatasetGSM8K {
		return 512
	}
	return 16
}

// Prompt is the system prompt and user turn sent for q.
func Prompt(q Question) (system, user string) {
	if q.Dataset == DatasetGSM8K {
		return systemGSM8K, q.Question
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(q.Question))
	b.WriteString("\n")
	for i, c := range q.Choices {
		b.WriteString("\n")
		b.WriteByte(byte('A' + i))
		b.WriteString(". ")
		b.WriteString(c)
	}
	return systemMMLU, b.String()
}

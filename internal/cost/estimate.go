package cost

import (
	"unicode/utf8"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// Estimates are made before a request is sent, to admit it against token
// limits and to reserve its cost against a budget. They are corrected once
// the provider reports usage, so an estimate only has to be cheap and
// rarely low: a low estimate lets a budget overshoot by the difference.

// charsPerToken is the divisor of the input estimate. English prose runs
// at about four characters per token on current tokenizers; three makes
// the estimate high for prose and closer for code and non-Latin text.
const charsPerToken = 3

// perMessageTokens covers the role markers and separators a chat template
// adds around each message.
const perMessageTokens = 4

// EstimateInputTokens is a cheap upper-leaning guess at a request's prompt
// size, from its character count.
func EstimateInputTokens(req *core.Request) int {
	chars := utf8.RuneCountInString(req.System)
	n := 0
	if req.System != "" {
		n += perMessageTokens
	}
	for _, m := range req.Messages {
		chars += utf8.RuneCountInString(m.Text)
		n += perMessageTokens
	}
	return n + (chars+charsPerToken-1)/charsPerToken
}

// EstimateOutputTokens is the most output a request can produce: its
// max_tokens, or fallback when it has none.
func EstimateOutputTokens(req *core.Request, fallback int) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return fallback
}

// Estimate is the usage a request is admitted and reserved on before the
// provider reports the real one.
func Estimate(req *core.Request, outputFallback int) core.Usage {
	return core.Usage{InputTokens: EstimateInputTokens(req), OutputTokens: EstimateOutputTokens(req, outputFallback)}
}

// EstimateOutputFromText counts the output of a stream that ended without
// reporting usage (a client disconnect, or a server that does not send
// usage) from the text it produced.
func EstimateOutputFromText(chars int) int {
	return (chars + charsPerToken - 1) / charsPerToken
}

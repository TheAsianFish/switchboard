package openaicompat

import "github.com/TheAsianFish/switchboard/internal/core"

// chatRequest is the upstream request body.
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// buildRequest translates a canonical request into the upstream body.
func buildRequest(req *core.Request, stream bool) chatRequest {
	out := chatRequest{
		Model:       req.Model,
		Messages:    make([]chatMessage, 0, len(req.Messages)+1),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	if req.MaxTokens > 0 {
		out.MaxTokens = req.MaxTokens
	}
	if req.System != "" {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, chatMessage{Role: string(m.Role), Content: m.Text})
	}
	if stream {
		out.Stream = true
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return out
}

// completion is a non-streamed response body.
type completion struct {
	ID      string    `json:"id"`
	Model   string    `json:"model"`
	Choices []choice  `json:"choices"`
	Usage   *usage    `json:"usage"`
	Error   *apiError `json:"error"`
}

type choice struct {
	Message struct {
		Content *string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

// chunk is one streamed event.
type chunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage"`
	Error   *apiError     `json:"error"`
}

type chunkChoice struct {
	Delta struct {
		Content *string `json:"content"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// canonical converts OpenAI usage, whose prompt count includes cache reads,
// into the canonical form, whose input count does not.
func (u *usage) canonical() core.Usage {
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return core.Usage{
		InputTokens:     max(u.PromptTokens-cached, 0),
		CacheReadTokens: cached,
		OutputTokens:    u.CompletionTokens,
	}
}

type apiError struct {
	Message string `json:"message"`
}

func (e *apiError) message() string {
	if e.Message == "" {
		return "unknown error"
	}
	return e.Message
}

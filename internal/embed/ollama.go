package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama calls an Ollama server's /api/embed endpoint.
type Ollama struct {
	// BaseURL is the server's root, for example http://localhost:11434,
	// without the /v1 suffix the chat provider uses.
	BaseURL string
	// Name is the embedding model, for example all-minilm.
	Name string
	// Timeout bounds one call; zero means DefaultTimeout.
	Timeout time.Duration
	// HTTP is the client to use; nil means http.DefaultClient.
	HTTP *http.Client
}

// DefaultTimeout bounds one embedding call. One short prompt embeds in
// milliseconds on a laptop CPU; a call that takes longer than this is
// better skipped than waited for.
const DefaultTimeout = 2 * time.Second

var _ Embedder = (*Ollama)(nil)

// Model implements Embedder.
func (o *Ollama) Model() string { return o.Name }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	// Truncate is always false. Ollama's default silently cuts an input
	// to the model's context (256 tokens for all-minilm), and two long
	// prompts that differ only after the cut would embed the same.
	Truncate bool `json:"truncate"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed implements Embedder. Ollama already returns unit vectors; they are
// normalised again so a server that does not cannot skew similarities.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(embedRequest{Model: o.Name, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := o.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusBadRequest && bytes.Contains(msg, []byte("exceeds the context length")) {
			return nil, fmt.Errorf("%w (%s)", ErrTooLong, o.Name)
		}
		return nil, fmt.Errorf("embed: %s returned %d: %s", o.Name, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out embedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("%w: %d vectors for %d inputs", ErrDimensions, len(out.Embeddings), len(texts))
	}
	for i, v := range out.Embeddings {
		if len(v) == 0 || len(v) != len(out.Embeddings[0]) {
			return nil, fmt.Errorf("%w: vector %d has %d dimensions", ErrDimensions, i, len(v))
		}
		Normalise(v)
	}
	return out.Embeddings, nil
}

package routereval

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/TheAsianFish/switchboard/internal/embed"
)

// Embedding is one question's prompt embedding: a line of the committed
// embeddings file, so the eval replays without an embedding server.
type Embedding struct {
	ID     string    `json:"id"`
	Vector []float32 `json:"vector"`
}

// EmbeddingsPath is the committed embeddings file for an embedding model.
func EmbeddingsPath(dir, model string) string {
	return filepath.Join(dir, "embeddings-"+safeName(model)+".jsonl")
}

func embedMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eval router embed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", envOr("OLLAMA_URL", "http://127.0.0.1:11434"), "Ollama base URL (env OLLAMA_URL)")
	model := fs.String("model", "nomic-embed-text", "embedding model; the gateway's router must use the same")
	questions := fs.String("questions", "eval/router/data/questions.jsonl", "the eval set")
	dir := fs.String("out", "eval/router/data", "directory for the embeddings file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	qs, err := readJSONL[Question](*questions)
	if err != nil {
		return err
	}
	e := &embed.Ollama{BaseURL: *url, Name: *model, Timeout: time.Minute}
	out, err := EmbedQuestions(ctx, e, qs)
	if err != nil {
		return err
	}
	path := EmbeddingsPath(*dir, *model)
	if err := writeJSONL(path, out); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "embedded %d prompts with %s (%d dimensions) into %s\n", len(out), *model, len(out[0].Vector), path)
	return nil
}

// EmbedQuestions embeds the user turn of every question, the text the
// gateway's router embeds for a request.
func EmbedQuestions(ctx context.Context, e embed.Embedder, qs []Question) ([]Embedding, error) {
	const batch = 16
	out := make([]Embedding, 0, len(qs))
	for i := 0; i < len(qs); i += batch {
		end := min(i+batch, len(qs))
		texts := make([]string, 0, end-i)
		for _, q := range qs[i:end] {
			_, user := Prompt(q)
			texts = append(texts, user)
		}
		vs, err := e.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("embed questions %d-%d: %w", i, end-1, err)
		}
		for k, v := range vs {
			out = append(out, Embedding{ID: qs[i+k].ID, Vector: v})
		}
	}
	return out, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

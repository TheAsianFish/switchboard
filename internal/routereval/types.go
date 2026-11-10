// Package routereval answers Q5: can a router trained on an offline eval
// set beat the random-mix line between the cheapest and the best model?
// It builds the committed eval set (GSM8K and MMLU test questions), labels
// it by sending every question to every candidate through the gateway and
// grading the reply, embeds the prompts with the embedding model the
// gateway's router uses, and cross-validates the router against the four
// baselines and the line.
package routereval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Datasets in the eval set.
const (
	DatasetGSM8K = "gsm8k"
	DatasetMMLU  = "mmlu"
)

// Question is one row of the committed eval set.
type Question struct {
	// ID is stable across rebuilds: "gsm8k-<test line>" or
	// "mmlu-<subject>-<row within the subject's test split>".
	ID      string `json:"id"`
	Dataset string `json:"dataset"`
	// Subject is MMLU's subject; empty for GSM8K.
	Subject  string `json:"subject,omitempty"`
	Question string `json:"question"`
	// Choices are MMLU's four options, A to D in order.
	Choices []string `json:"choices,omitempty"`
	// Answer is GSM8K's final number or MMLU's letter.
	Answer string `json:"answer"`
}

// Label is one candidate's graded reply to one question: a line of a
// recording file. Recordings are what the eval replays at $0.
type Label struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Correct bool   `json:"correct"`
	// Extracted is the answer the grader read from the reply.
	Extracted string `json:"extracted"`
	Output    string `json:"output"`
	// StopReason is the OpenAI finish_reason ("stop", "length").
	StopReason   string `json:"stop_reason"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	// Nanodollars is what the gateway's price table charges for the
	// usage; 0 for an unpriced (local) model.
	Nanodollars int64   `json:"nanodollars"`
	TTFTMillis  float64 `json:"ttft_ms"`
	TotalMillis float64 `json:"total_ms"`
	// Error is set when the request failed; such a label is not
	// recorded, so a resumed run tries it again.
	Error string `json:"error,omitempty"`
	// At is when the request was sent, RFC 3339.
	At string `json:"at"`
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

func writeJSONL[T any](path string, rows []T) error {
	var buf bytes.Buffer
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

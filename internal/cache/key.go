package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// keyVersion changes whenever the canonical form changes, so entries
// written under an old form are never read under a new one. Fields added
// with omitempty leave the form of every request without them unchanged
// and need no new version: the tool fields are such.
const keyVersion = 2

// Keys locate a request in the cache.
type Keys struct {
	// Namespace is the tenant's hash. Every exact key and every vector
	// entry lives under it, and a search never leaves it.
	Namespace string
	// Context hashes everything but the last user turn; a semantic match
	// is only looked for among entries with the same context.
	Context string
	// Exact hashes the context and the last user turn.
	Exact string
	// Prompt is the last user turn as keyed (see KeysFor): what the
	// semantic layer embeds.
	Prompt string
}

// Upstream is where the gateway routes a request: the configured provider
// name and the provider's own model name.
type Upstream struct {
	Provider string
	Model    string
}

// canonicalContext is the hashed form of a request without its last turn.
// Field order is fixed by the struct, so equal requests give equal bytes.
type canonicalContext struct {
	V      int      `json:"v"`
	Tenant string   `json:"tenant"`
	Model  string   `json:"model"`
	Prov   string   `json:"provider"`
	Up     string   `json:"upstream_model"`
	System string   `json:"system"`
	Turns  []turn   `json:"turns"`
	Max    int      `json:"max_tokens"`
	Temp   *float64 `json:"temperature"`
	TopP   *float64 `json:"top_p"`
	Stop   []string `json:"stop"`
	// Tools and Choice are what the model was offered; two requests that
	// differ only there may get different replies.
	Tools  []keyTool  `json:"tools,omitempty"`
	Choice *keyChoice `json:"tool_choice,omitempty"`
	// LastResults are the last user turn's tool results. They belong to
	// the turn the semantic layer embeds, but that layer never serves a
	// request with tool content, so hashing them here is enough to keep
	// exact entries apart.
	LastResults []keyResult `json:"last_tool_results,omitempty"`
}

type turn struct {
	Role    core.Role   `json:"role"`
	Text    string      `json:"text"`
	Calls   []keyCall   `json:"tool_calls,omitempty"`
	Results []keyResult `json:"tool_results,omitempty"`
}

// Tool content is hashed as given: arguments and schemas are JSON text
// (kept as strings, so a client's odd but valid spacing cannot fail the
// encoding), and none of it is cleaned like prose.
type keyTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  string `json:"parameters"`
}

type keyChoice struct {
	Mode            core.ToolChoiceMode `json:"mode"`
	Name            string              `json:"name"`
	DisableParallel bool                `json:"disable_parallel"`
}

type keyCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type keyResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

func keyResults(rs []core.ToolResult) []keyResult {
	if len(rs) == 0 {
		return nil
	}
	out := make([]keyResult, len(rs))
	for i, r := range rs {
		out[i] = keyResult{CallID: r.CallID, Content: r.Content, IsError: r.IsError}
	}
	return out
}

func keyCalls(cs []core.ToolCall) []keyCall {
	if len(cs) == 0 {
		return nil
	}
	out := make([]keyCall, len(cs))
	for i, c := range cs {
		out[i] = keyCall{ID: c.ID, Name: c.Name, Arguments: string(c.Arguments)}
	}
	return out
}

// KeysFor derives the keys of req for tenant. It reports false when the
// request cannot be cached because its last turn is not the user's (a
// prefilled assistant turn asks for a continuation, not an answer).
//
// Line endings are normalised, trailing whitespace and leading blank lines
// dropped, nothing more: leading spaces, inner whitespace and case can
// carry meaning (indentation in code, an acronym against a word). Whether
// a looser match is worth it is the semantic layer's question, and Q3
// measures it.
// The client's model name is hashed and so is the route behind it (up), so
// that remapping a model to another upstream model or provider, or replicas
// that disagree during a rolling deploy, never serve one model's reply for
// another's. The stream flag is not hashed: a reply cached from a
// non-streaming call answers a streaming one.
func KeysFor(tenant string, up Upstream, req *core.Request) (Keys, bool) {
	n := len(req.Messages)
	if n == 0 || req.Messages[n-1].Role != core.RoleUser {
		return Keys{}, false
	}
	c := canonicalContext{
		V: keyVersion, Tenant: tenant, Model: req.Model, Prov: up.Provider, Up: up.Model, System: clean(req.System),
		Turns: make([]turn, 0, n-1), Max: req.MaxTokens, Temp: req.Temperature, TopP: req.TopP, Stop: req.Stop,
	}
	for _, m := range req.Messages[:n-1] {
		c.Turns = append(c.Turns, turn{Role: m.Role, Text: clean(m.Text), Calls: keyCalls(m.ToolCalls), Results: keyResults(m.ToolResults)})
	}
	for _, t := range req.Tools {
		c.Tools = append(c.Tools, keyTool{Name: t.Name, Description: t.Description, Parameters: string(t.Parameters)})
	}
	if tc := req.ToolChoice; tc != (core.ToolChoice{}) {
		c.Choice = &keyChoice{Mode: tc.Mode, Name: tc.Name, DisableParallel: tc.DisableParallel}
	}
	c.LastResults = keyResults(req.Messages[n-1].ToolResults)
	ctxBytes, err := json.Marshal(c)
	if err != nil {
		// Only strings, ints, bools and float pointers: Marshal cannot
		// fail except on NaN or Inf, which the front ends refuse.
		return Keys{}, false
	}
	prompt := clean(req.Messages[n-1].Text)
	ctxSum := sha256.Sum256(ctxBytes)
	h := sha256.New()
	h.Write(ctxSum[:])
	h.Write([]byte(prompt))
	ns := sha256.Sum256([]byte("tenant\x00" + tenant))
	return Keys{
		Namespace: hex.EncodeToString(ns[:16]),
		Context:   hex.EncodeToString(ctxSum[:16]),
		Exact:     hex.EncodeToString(h.Sum(nil)),
		Prompt:    prompt,
	}, true
}

func clean(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimLeft(strings.TrimRight(s, " \t\n"), "\n")
}

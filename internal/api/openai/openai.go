// Package openai is the OpenAI chat completions front end. It parses
// POST /v1/chat/completions into a canonical core.Request, hands it to a
// core.Gateway and renders the reply as an OpenAI JSON body or SSE stream.
// Text content and function tools are supported: tools, tool_choice,
// parallel_tool_calls, assistant tool_calls and "tool" messages in, and
// message.tool_calls or streamed delta.tool_calls with finish_reason
// "tool_calls" out. Multimodal parts, non-function tools and the legacy
// functions API are refused with an invalid-request error.
package openai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// maxBodyBytes bounds a request body.
const maxBodyBytes = 10 << 20

// idPrefix starts every chat completion id.
const idPrefix = "chatcmpl-"

// ChatCompletions serves POST /v1/chat/completions over gw.
func ChatCompletions(gw core.Gateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeStatusError(w, http.StatusMethodNotAllowed, typeInvalidRequest, "", "method not allowed: use POST")
			return
		}
		ctx, ok := authenticate(w, r, gw)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeStatusError(w, http.StatusRequestEntityTooLarge, typeInvalidRequest, "request_too_large", "request body is larger than 10 MiB")
				return
			}
			writeError(w, ctx, core.Wrap(core.KindInvalidRequest, err, "could not read the request body"))
			return
		}
		req, opts, err := parseRequest(body)
		if err != nil {
			writeError(w, ctx, err)
			return
		}
		if req.Stream {
			serveStream(ctx, w, gw, req, opts)
			return
		}
		serveComplete(ctx, w, gw, req)
	})
}

// ListModels serves GET /v1/models with the models the caller may use.
func ListModels(gw core.Gateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeStatusError(w, http.StatusMethodNotAllowed, typeInvalidRequest, "", "method not allowed: use GET")
			return
		}
		ctx, ok := authenticate(w, r, gw)
		if !ok {
			return
		}
		names := gw.Models(ctx)
		list := modelList{Object: "list", Data: make([]model, 0, len(names))}
		for _, n := range names {
			list.Data = append(list.Data, model{ID: n, Object: "model", Created: 0, OwnedBy: "switchboard"})
		}
		writeJSON(w, http.StatusOK, list)
	})
}

type modelList struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// authenticate resolves the caller's key and writes the error response when
// it fails. The returned context must be used for the rest of the request.
func authenticate(w http.ResponseWriter, r *http.Request, gw core.Gateway) (context.Context, bool) {
	key := apiKey(r)
	// An empty key still goes to the gateway: whether one is required is
	// its decision (the fake provider, like Ollama, accepts none).
	ctx, err := gw.Authenticate(r.Context(), key)
	if err != nil {
		if key == "" {
			err = core.Wrap(core.KindAuthentication, err, "missing API key: send it as \"Authorization: Bearer <key>\"")
		}
		writeError(w, r.Context(), err)
		return nil, false
	}
	return ctx, true
}

// apiKey reads the bearer token, falling back to the x-api-key header.
func apiKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, found := strings.Cut(strings.TrimSpace(h), " ")
		if found && strings.EqualFold(scheme, "Bearer") {
			if t := strings.TrimSpace(token); t != "" {
				return t
			}
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// randomHex returns 24 random hex digits for made-up ids.
func randomHex() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// completionID returns the client-facing id for a provider id.
func completionID(providerID string) string {
	switch {
	case providerID == "":
		return idPrefix + randomHex()
	case strings.HasPrefix(providerID, idPrefix):
		return providerID
	default:
		return idPrefix + providerID
	}
}

// Package anthropic is the Anthropic Messages front end. It parses
// POST /v1/messages into a canonical request, hands it to a core.Gateway and
// renders the reply as a Messages response or as Anthropic's server-sent
// event sequence. GET /v1/models lists the caller's models in Anthropic's
// list shape.
//
// Text, client tools, tool_choice, tool_use and tool_result blocks
// (tool_result content as a string or text blocks) are supported; replies
// carry tool_use blocks, streamed as content_block_start, input_json_delta
// and content_block_stop, with stop_reason tool_use. Images, documents,
// thinking and server tools are refused with an invalid-request error.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// MaxBodyBytes bounds a request body; larger bodies get 413.
const MaxBodyBytes = 10 << 20

// Messages serves POST /v1/messages.
func Messages(gw core.Gateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErrorType(r.Context(), w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed: use POST")
			return
		}
		ctx, ok := authenticate(w, r, gw)
		if !ok {
			return
		}
		req, err := parseRequest(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeErrorType(ctx, w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the 10 MiB limit")
				return
			}
			writeError(ctx, w, err)
			return
		}
		if req.Stream {
			serveStream(ctx, w, gw, req)
			return
		}
		resp, err := gw.Complete(ctx, req)
		if err != nil {
			writeError(ctx, w, err)
			return
		}
		msg, err := renderMessage(resp)
		if err != nil {
			writeError(ctx, w, err)
			return
		}
		writeJSON(ctx, w, http.StatusOK, msg)
	})
}

// modelInfo is one entry of the models list.
type modelInfo struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type modelList struct {
	Data    []modelInfo `json:"data"`
	HasMore bool        `json:"has_more"`
	FirstID *string     `json:"first_id"`
	LastID  *string     `json:"last_id"`
}

// modelsCreatedAt is reported as every model's creation time: the gateway
// does not know when an upstream model was released, and the field is
// required by the list shape.
var modelsCreatedAt = time.Unix(0, 0).UTC().Format(time.RFC3339)

// ListModels serves GET /v1/models with the models the caller may use.
func ListModels(gw core.Gateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			writeErrorType(r.Context(), w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed: use GET")
			return
		}
		ctx, ok := authenticate(w, r, gw)
		if !ok {
			return
		}
		names := gw.Models(ctx)
		list := modelList{Data: make([]modelInfo, 0, len(names))}
		for _, n := range names {
			list.Data = append(list.Data, modelInfo{Type: "model", ID: n, DisplayName: n, CreatedAt: modelsCreatedAt})
		}
		if len(names) > 0 {
			list.FirstID, list.LastID = &names[0], &names[len(names)-1]
		}
		writeJSON(ctx, w, http.StatusOK, list)
	})
}

// authenticate resolves the client key from x-api-key, falling back to an
// Authorization bearer token. On failure it writes the error response.
func authenticate(w http.ResponseWriter, r *http.Request, gw core.Gateway) (context.Context, bool) {
	key := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	if key == "" {
		if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			key = strings.TrimSpace(token)
		}
	}
	// An empty key still goes to the gateway: whether one is required is
	// its decision (the fake provider, like Ollama, accepts none).
	ctx, err := gw.Authenticate(r.Context(), key)
	if err != nil {
		if key == "" {
			err = core.Wrap(core.KindAuthentication, err, "missing API key: set the x-api-key header")
		}
		writeError(r.Context(), w, err)
		return nil, false
	}
	return ctx, true
}

// errorStatus maps an error kind to the HTTP status and Anthropic error type.
func errorStatus(k core.ErrorKind) (int, string) {
	switch k {
	case core.KindInvalidRequest:
		return http.StatusBadRequest, "invalid_request_error"
	case core.KindAuthentication:
		return http.StatusUnauthorized, "authentication_error"
	case core.KindPermission:
		return http.StatusForbidden, "permission_error"
	case core.KindNotFound:
		return http.StatusNotFound, "not_found_error"
	case core.KindRateLimited:
		return http.StatusTooManyRequests, "rate_limit_error"
	case core.KindBudgetExceeded:
		// Anthropic's own billing failure; the SDKs do not retry a 402.
		return http.StatusPaymentRequired, "billing_error"
	case core.KindOverloaded:
		return 529, "overloaded_error"
	case core.KindTimeout:
		return http.StatusGatewayTimeout, "timeout_error"
	case core.KindUpstream:
		return http.StatusBadGateway, "api_error"
	default:
		return http.StatusInternalServerError, "api_error"
	}
}

// internalMessage replaces the message of internal errors, whose cause must
// not reach the client.
const internalMessage = "internal server error"

type errorBody struct {
	Type  string      `json:"type"`
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// clientError classifies err into the status, type and client-safe message
// of an Anthropic error, logging server-side failures with their cause.
func clientError(ctx context.Context, err error) (*core.Error, int, errorDetail) {
	e := core.AsError(err)
	status, typ := errorStatus(e.Kind)
	msg := e.Message
	if e.Kind == core.KindInternal {
		msg = internalMessage
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if status >= 500 {
		telemetry.Logger(ctx).Error("request failed", "status", status, "kind", e.Kind.String(), "error", err.Error())
	}
	return e, status, errorDetail{Type: typ, Message: msg}
}

// writeError writes err as an Anthropic error response.
func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	e, status, detail := clientError(ctx, err)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	writeJSON(ctx, w, status, errorBody{Type: "error", Error: detail})
}

// writeErrorType writes an error the front end raises itself.
func writeErrorType(ctx context.Context, w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(ctx, w, status, errorBody{Type: "error", Error: errorDetail{Type: typ, Message: msg}})
}

func writeJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	body, err := marshal(v)
	if err != nil {
		telemetry.Logger(ctx).Error("encode response", "error", err.Error())
		status = http.StatusInternalServerError
		body = []byte(`{"type":"error","error":{"type":"api_error","message":"internal server error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		telemetry.Logger(ctx).Debug("write response", "error", err.Error())
	}
}

// marshal encodes v without HTML escaping, so model text reaches the client
// as it was generated.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode json: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

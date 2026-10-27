package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// OpenAI error types.
const (
	typeInvalidRequest = "invalid_request_error"
	typePermission     = "permission_error"
	typeRateLimit      = "rate_limit_error"
	typeServer         = "server_error"
)

// errorBody is the OpenAI error envelope.
type errorBody struct {
	Error errorObject `json:"error"`
}

type errorObject struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// classify maps a classified error to its HTTP status and OpenAI error
// object. Internal errors get a generic message so their cause never reaches
// the client.
func classify(e *core.Error) (int, errorObject) {
	obj := errorObject{Message: e.Message}
	var status int
	var code string
	switch e.Kind {
	case core.KindInvalidRequest:
		status, obj.Type = http.StatusBadRequest, typeInvalidRequest
	case core.KindAuthentication:
		status, obj.Type, code = http.StatusUnauthorized, typeInvalidRequest, "invalid_api_key"
	case core.KindPermission:
		status, obj.Type = http.StatusForbidden, typePermission
	case core.KindNotFound:
		status, obj.Type = http.StatusNotFound, typeInvalidRequest
		if strings.Contains(strings.ToLower(e.Message), "model") {
			code = "model_not_found"
		}
	case core.KindRateLimited:
		status, obj.Type, code = http.StatusTooManyRequests, typeRateLimit, "rate_limit_exceeded"
	case core.KindOverloaded:
		status, obj.Type, code = http.StatusServiceUnavailable, typeServer, "overloaded"
	case core.KindTimeout:
		status, obj.Type, code = http.StatusGatewayTimeout, typeServer, "timeout"
	case core.KindUpstream:
		status, obj.Type, code = http.StatusBadGateway, typeServer, "upstream_error"
	default:
		status, obj.Type = http.StatusInternalServerError, typeServer
		obj.Message = "internal error"
	}
	if obj.Message == "" {
		obj.Message = http.StatusText(status)
	}
	if code != "" {
		obj.Code = &code
	}
	return status, obj
}

// writeError renders err as an OpenAI error response. 5xx errors are logged
// with their cause.
func writeError(w http.ResponseWriter, ctx context.Context, err error) {
	e := core.AsError(err)
	status, obj := classify(e)
	logError(ctx, "request failed", status, e)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	writeJSON(w, status, errorBody{Error: obj})
}

// writeStatusError renders an error that has no core.ErrorKind, such as 405
// and 413.
func writeStatusError(w http.ResponseWriter, status int, typ, code, message string) {
	obj := errorObject{Message: message, Type: typ}
	if code != "" {
		obj.Code = &code
	}
	writeJSON(w, status, errorBody{Error: obj})
}

// logError logs server-side failures; client errors are not worth a line.
func logError(ctx context.Context, msg string, status int, e *core.Error) {
	if status < 500 {
		return
	}
	level := slog.LevelWarn
	if e.Kind == core.KindInternal {
		level = slog.LevelError
	}
	telemetry.Logger(ctx).Log(ctx, level, msg, "status", status, "kind", e.Kind.String(), "error", e.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// classify turns an SDK or transport error into a classified core error.
// ctx is the call's context, consulted to tell deadlines from other
// transport failures. The API key is never part of the message: the SDK's
// errors carry the URL and the response body, not the request headers.
func classify(ctx context.Context, err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return classifyAPI(apiErr)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return core.Wrap(core.KindTimeout, err, "upstream timed out")
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return core.Wrap(core.KindTimeout, err, "request cancelled")
	default:
		return core.Wrap(core.KindUpstream, err, "upstream request failed")
	}
}

// classifyAPI classifies an error the API answered with: an HTTP error
// status, or an error event inside a stream that started with 200.
func classifyAPI(e *sdk.Error) error {
	kind := core.KindFromStatus(e.StatusCode)
	if e.StatusCode < 400 {
		kind = kindFromType(e.Type())
	}
	msg := apiMessage(e.RawJSON())
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
		if e.StatusCode < 400 || msg == "" {
			msg = "stream error"
			if t := string(e.Type()); t != "" {
				msg = t
			}
		}
	}
	out := core.Wrap(kind, e, "upstream: "+msg)
	if e.Response != nil {
		out.RetryAfter = retryAfter(e.Response.Header.Get("Retry-After"), time.Now())
	}
	return out
}

// kindFromType classifies the error type of an error body or event.
func kindFromType(t sdk.ErrorType) core.ErrorKind {
	switch t {
	case sdk.ErrorTypeInvalidRequestError:
		return core.KindInvalidRequest
	case sdk.ErrorTypeAuthenticationError:
		return core.KindAuthentication
	case sdk.ErrorTypePermissionError, sdk.ErrorTypeBillingError:
		return core.KindPermission
	case sdk.ErrorTypeNotFoundError:
		return core.KindNotFound
	case sdk.ErrorTypeRateLimitError:
		return core.KindRateLimited
	case sdk.ErrorTypeOverloadedError:
		return core.KindOverloaded
	case sdk.ErrorTypeTimeoutError:
		return core.KindTimeout
	default:
		return core.KindUpstream
	}
}

// apiMessage extracts error.message from an Anthropic error body.
func apiMessage(body string) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	return env.Error.Message
}

// retryAfter parses a Retry-After header, in seconds or as an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

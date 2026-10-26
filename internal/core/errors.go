package core

import (
	"errors"
	"fmt"
	"time"
)

// ErrorKind classifies a failure so each front end can map it to its own
// status code and error type. Providers classify upstream errors; nothing
// below the front ends knows about HTTP status codes of the client API.
type ErrorKind int

const (
	KindInternal ErrorKind = iota
	KindInvalidRequest
	KindAuthentication
	KindPermission
	KindNotFound
	KindRateLimited
	KindOverloaded
	KindTimeout
	KindUpstream
)

func (k ErrorKind) String() string {
	switch k {
	case KindInvalidRequest:
		return "invalid_request"
	case KindAuthentication:
		return "authentication"
	case KindPermission:
		return "permission"
	case KindNotFound:
		return "not_found"
	case KindRateLimited:
		return "rate_limited"
	case KindOverloaded:
		return "overloaded"
	case KindTimeout:
		return "timeout"
	case KindUpstream:
		return "upstream"
	default:
		return "internal"
	}
}

// Error is a classified failure. Message is safe to show to the client.
type Error struct {
	Kind    ErrorKind
	Message string
	// RetryAfter is set when the failing party said when to retry.
	RetryAfter time.Duration
	// Err is the underlying cause, kept for logs and errors.Is; it is never
	// shown to the client.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds a classified error with a client-safe message.
func Errorf(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Wrap classifies err with a client-safe message. The cause stays available
// to errors.Is and errors.As.
func Wrap(kind ErrorKind, err error, message string) *Error {
	return &Error{Kind: kind, Message: message, Err: err}
}

// AsError returns err's classification, treating unclassified errors as
// internal so their text never reaches a client.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Kind: KindInternal, Message: "internal error", Err: err}
}

// KindFromStatus classifies an upstream HTTP status code.
func KindFromStatus(status int) ErrorKind {
	switch {
	case status == 400 || status == 413 || status == 422:
		return KindInvalidRequest
	case status == 401:
		return KindAuthentication
	case status == 403:
		return KindPermission
	case status == 404:
		return KindNotFound
	case status == 408 || status == 504:
		return KindTimeout
	case status == 429:
		return KindRateLimited
	case status == 503 || status == 529:
		return KindOverloaded
	default:
		return KindUpstream
	}
}

// Package telemetry holds the process logger and the per-request log
// context, the Prometheus metrics and the OpenTelemetry tracing setup.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

// NewLogger builds the process logger. format is "json" or "text"; level is
// one of debug, info, warn, error.
func NewLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(format) {
	case "json", "":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("log format %q: want json or text", format)
	}
}

// WithRequest returns a context carrying the request id and a logger that
// stamps it on every line.
func WithRequest(ctx context.Context, base *slog.Logger, requestID string) context.Context {
	ctx = context.WithValue(ctx, requestIDKey, requestID)
	return context.WithValue(ctx, loggerKey, base.With("request_id", requestID))
}

// WithAttrs returns a context whose logger carries extra attributes.
func WithAttrs(ctx context.Context, args ...any) context.Context {
	return context.WithValue(ctx, loggerKey, Logger(ctx).With(args...))
}

// Logger returns the request's logger, or the default logger outside a
// request.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// RequestID returns the request id carried by ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// NewRequestID returns a random request id.
func NewRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

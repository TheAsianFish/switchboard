// Package server mounts the front ends on one HTTP handler and runs it with
// request ids, access logs, panic recovery and graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	anthropicapi "github.com/TheAsianFish/switchboard/internal/api/anthropic"
	openaiapi "github.com/TheAsianFish/switchboard/internal/api/openai"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Handler serves both wire formats over gw.
func Handler(gw core.Gateway, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	// The API routes take every method so the front ends answer a wrong one
	// with 405 in their own error format instead of the catch-all's 404.
	mux.Handle("/v1/chat/completions", openaiapi.ChatCompletions(gw))
	mux.Handle("/v1/messages", anthropicapi.Messages(gw))
	openaiModels, anthropicModels := openaiapi.ListModels(gw), anthropicapi.ListModels(gw)
	// Both APIs list models at the same path in different shapes. Anthropic
	// clients always send anthropic-version, OpenAI clients never do.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") != "" {
			anthropicModels.ServeHTTP(w, r)
			return
		}
		openaiModels.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":{"type":"not_found_error","message":"no route for %s %s"}}`+"\n", r.Method, jsonSafe(r.URL.Path))
	})
	return withRequestContext(log, mux)
}

// jsonSafe keeps only characters that need no escaping in a JSON string.
func jsonSafe(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < 200; i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7f && c != '"' && c != '\\' {
			b = append(b, c)
		}
	}
	return string(b)
}

// withRequestContext gives every request an id (the caller's X-Request-Id
// when it is sane, otherwise a fresh one), a logger that stamps it, an
// access log line and panic recovery.
func withRequestContext(base *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = telemetry.NewRequestID()
		}
		// x-request-id is the common convention; request-id is the header
		// the Anthropic SDKs surface on errors.
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("Request-Id", id)
		ctx := telemetry.WithRequest(r.Context(), base, id)
		rec := &recorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			log := telemetry.Logger(ctx)
			abort := false
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				log.Error("panic serving request", slog.Any("panic", p))
				if rec.wroteHeader {
					// Returning would end the response cleanly and a cut-off
					// reply would read as complete; abort the connection so
					// the client sees a transport error instead.
					abort = true
				} else {
					rec.Header().Set("Content-Type", "application/json")
					rec.WriteHeader(http.StatusInternalServerError)
					_, _ = rec.Write([]byte(`{"error":{"type":"api_error","message":"internal error"}}` + "\n"))
				}
			}
			log.Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.statusCode()),
				slog.Int64("bytes", rec.bytes),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			)
			if abort {
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(rec, r.WithContext(ctx))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// recorder captures the status and size for the access log. It keeps
// streaming working by forwarding Flush, and exposes the underlying writer
// to http.ResponseController through Unwrap.
type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = http.StatusOK, true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		if !r.wroteHeader {
			r.status, r.wroteHeader = http.StatusOK, true
		}
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) statusCode() int {
	if !r.wroteHeader {
		return http.StatusOK
	}
	return r.status
}

// ShutdownGrace is how long in-flight requests, streams included, get to
// finish after a shutdown signal.
const ShutdownGrace = 30 * time.Second

// Run serves h on addr until ctx is cancelled, then drains. ready, when not
// nil, receives the bound address once the listener is open.
func Run(ctx context.Context, addr string, h http.Handler, log *slog.Logger, ready func(net.Addr)) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a stream may legitimately run for minutes. The
		// provider timeout bounds each upstream call instead.
		IdleTimeout: 120 * time.Second,
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	if ready != nil {
		ready(ln.Addr())
	}
	log.Info("listening", slog.String("addr", ln.Addr().String()))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down", slog.Duration("grace", ShutdownGrace))
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

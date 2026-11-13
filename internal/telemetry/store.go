package telemetry

import (
	"context"
	"time"
)

// SlowStoreCall is how long a call to the shared store may wait before it
// is logged. A store call on a healthy network takes well under a
// millisecond, so a wait this long is the store, or the path to it,
// failing; logging every such wait with its component is how an outage's
// slow requests are traced to their cause.
const SlowStoreCall = 50 * time.Millisecond

// Components named on a slow store call's log line.
const (
	StoreLimiter     = "limiter"
	StoreBudget      = "budget"
	StoreCacheLookup = "cache_lookup"
	StoreCacheFill   = "cache_fill"
)

// SlowStoreMessage is the message of the line StoreWaited logs.
const SlowStoreMessage = "store call slow"

// StoreWaited logs, at warn level and with the request's id, a store call
// that waited SlowStoreCall or longer: the component that made it, the
// operation, the wait in milliseconds and the error, if the call failed.
// Faster calls log nothing.
func StoreWaited(ctx context.Context, component, op string, waited time.Duration, err error) {
	if waited < SlowStoreCall {
		return
	}
	args := []any{"component", component, "op", op, "wait_ms", Millis(waited)}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	Logger(ctx).Warn(SlowStoreMessage, args...)
}

// Millis is d in milliseconds, to the microsecond.
func Millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

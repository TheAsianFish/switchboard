package cache

import (
	"fmt"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// Mode is how much caching a request gets. Modes are ordered: each one
// includes the ones below it.
type Mode int

// Modes.
const (
	// ModeOff neither reads nor writes the cache.
	ModeOff Mode = iota
	// ModeExact reads and writes exact entries only.
	ModeExact
	// ModeSemantic also matches reworded last turns.
	ModeSemantic
)

func (m Mode) String() string {
	switch m {
	case ModeExact:
		return "exact"
	case ModeSemantic:
		return "semantic"
	default:
		return "off"
	}
}

// ParseMode reads a configured mode.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off":
		return ModeOff, nil
	case "exact":
		return ModeExact, nil
	case "semantic":
		return ModeSemantic, nil
	default:
		return ModeOff, fmt.Errorf("cache mode %q is not one of off, exact, semantic", s)
	}
}

// DirectiveHeader is the request header a client sets to choose the cache
// mode for one request: off, exact or semantic.
const DirectiveHeader = "X-Switchboard-Cache"

// Decision is the mode a request gets and why.
type Decision struct {
	Mode Mode
	// Reason says why the mode is lower than the key's, for logs.
	Reason string
}

// Decide applies the cacheability rules:
//
//   - The key's mode is the ceiling; a client's directive can lower it but
//     never raise it, so an operator who turned caching off for a key
//     keeps it off.
//   - Without a directive, a request is cached only when it asks for
//     deterministic output: a temperature set and at most maxTemperature.
//     A client sampling at temperature 1 asks for a different reply each
//     time, and a cache would quietly take that away. A client that sets
//     the directive has said it wants caching, whatever its temperature.
//   - A request whose last turn is not the user's is never cached.
//
// A directive that is not a mode is a client error.
func Decide(keyMode Mode, directive string, req *core.Request, maxTemperature float64) (Decision, error) {
	mode, reason := keyMode, ""
	if keyMode == ModeOff {
		reason = "off for this key"
	}
	if d := strings.TrimSpace(directive); d != "" {
		want, err := ParseMode(d)
		if err != nil {
			return Decision{}, core.Errorf(core.KindInvalidRequest, "header %s: %q is not one of off, exact, semantic", DirectiveHeader, d)
		}
		if want < mode {
			mode, reason = want, "lowered by "+DirectiveHeader
		}
	} else if mode != ModeOff && (req.Temperature == nil || *req.Temperature > maxTemperature) {
		mode, reason = ModeOff, fmt.Sprintf("temperature not set at or below %g", maxTemperature)
	}
	if n := len(req.Messages); mode != ModeOff && (n == 0 || req.Messages[n-1].Role != core.RoleUser) {
		mode, reason = ModeOff, "last turn is not the user's"
	}
	return Decision{Mode: mode, Reason: reason}, nil
}

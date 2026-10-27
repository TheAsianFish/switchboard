package cacheeval

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

// Main runs `switchboard eval cache-data|cache|cache-index`; args[0] is the
// subcommand.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var err error
	switch args[0] {
	case "cache-data":
		err = dataMain(ctx, args[1:], stdout, stderr)
	case "cache":
		err = cacheMain(ctx, args[1:], stdout, stderr)
	case "cache-index":
		err = indexMain(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown eval %q\n", args[0])
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "switchboard eval %s: %v\n", args[0], err)
		return 1
	}
}

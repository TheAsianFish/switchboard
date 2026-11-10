package routereval

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

// Main runs `switchboard eval router <data|label|embed|run>`.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: switchboard eval router data|label|embed|run [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "data":
		err = dataMain(ctx, args[1:], stdout, stderr)
	case "label":
		err = labelMain(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown eval router command %q\n", args[0])
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "switchboard eval router %s: %v\n", args[0], err)
		return 1
	}
}

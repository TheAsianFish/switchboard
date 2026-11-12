package stalleval

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Main is `switchboard eval stall`: it analyses one bench-stall run and
// writes its Summary to stdout as indented JSON. args excludes the
// subcommand names. It returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "eval/results/m10-stall", "directory eval/stall/run.sh wrote")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "stall: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	s, err := Analyse(*dir)
	if err == nil {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(s)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

package chaos

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// Main implements `switchboard chaos`; args start with the subcommand:
// report (Q6 and M8's runs), orphan (M12's) or outage (M13's analysis
// of a run's slow requests, written to outage.json).
func Main(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "report" && args[0] != "orphan" && args[0] != "outage" {
		_, _ = fmt.Fprintln(stderr, "usage: switchboard chaos report|orphan|outage [--dir eval/results/q6-chaos] [--out file]")
		return 2
	}
	name := "chaos " + args[0]
	defaultDir := "eval/results/q6-chaos"
	switch args[0] {
	case "orphan":
		defaultDir = "eval/results/m12-orphan/after"
	case "outage":
		defaultDir = "eval/results/m13-outage/after"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", defaultDir, "the run's raw files")
	out := fs.String("out", "", `write the summary here (default <dir>/summary.json, "-" for stdout)`)
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var summary any
	var text string
	file := SummaryFile
	switch args[0] {
	case "outage":
		in, err := Load(*dir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		}
		s, err := AnalyseOutage(*in)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		}
		summary, text, file = s, DescribeOutage(s), OutageFile
	case "orphan":
		in, err := LoadOrphan(*dir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		}
		s, err := AnalyseOrphan(*in)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		}
		summary, text = s, DescribeOrphan(s)
	default:
		in, err := Load(*dir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		}
		s, err := Analyse(*in, DefaultRule)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		}
		summary, text = s, Describe(s)
	}
	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	b = append(b, '\n')
	path := *out
	if path == "" {
		path = filepath.Join(*dir, file)
	}
	if path == "-" {
		_, err = stdout.Write(b)
	} else {
		err = os.WriteFile(path, b, 0o644)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	_, _ = fmt.Fprint(stderr, text)
	return 0
}

// Load reads a run's raw files from dir. The limited and budget streams
// are optional.
func Load(dir string) (*Input, error) {
	in := &Input{Streams: map[string]*Stream{}}
	if err := readJSON(filepath.Join(dir, MetaFile), &in.Meta); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, EventsFile), &in.Events); err != nil {
		return nil, err
	}
	for _, name := range []string{StreamMain, StreamLimited, StreamBudget} {
		st, err := loadStream(dir, name)
		if errors.Is(err, fs.ErrNotExist) && name != StreamMain {
			continue
		}
		if err != nil {
			return nil, err
		}
		in.Streams[name] = st
	}
	var ledger struct {
		Entries []fake.Entry `json:"entries"`
	}
	if err := readJSON(filepath.Join(dir, LedgerFile), &ledger); err != nil {
		return nil, err
	}
	in.Ledger = ledger.Entries
	err := readJSON(filepath.Join(dir, SnapshotsFile), &in.Snapshots)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if in.Logs, err = loadLogs(dir); err != nil {
		return nil, err
	}
	return in, nil
}

func loadStream(dir, name string) (*Stream, error) {
	st := &Stream{}
	if err := readJSON(filepath.Join(dir, name+".json"), &st.Result); err != nil {
		return nil, err
	}
	r, closeFn, err := open(filepath.Join(dir, name+".jsonl.gz"))
	if err != nil {
		return nil, err
	}
	defer closeFn()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var rec bench.Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("%s log: %w", name, err)
		}
		st.Records = append(st.Records, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s log: %w", name, err)
	}
	return st, nil
}

// open opens path, through gzip if its name ends in .gz.
func open(path string) (io.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, func() { _ = f.Close() }, nil
	}
	z, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return z, func() { _ = z.Close(); _ = f.Close() }, nil
}

func readJSON(path string, v any) error {
	r, closeFn, err := open(path)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Describe is a short plain-text table of s for the terminal.
func Describe(s *Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-26s %7s %8s %6s %9s %9s %6s %6s %10s\n", "phase", "from s", "requests", "err %", "p50 ms", "p99 ms", "hit %", "over", "overshoot")
	for _, p := range s.Phases {
		fmt.Fprintf(&b, "%-26s %7.1f %8d %6.2f %9.2f %9.2f %6.1f %6d %10s\n", p.Name, p.FromSeconds, p.Main.Requests,
			100*p.Main.ErrorRate, p.Main.Latency.P50, p.Main.Latency.P99, 100*p.Main.CacheHitRate,
			p.Limited.OverAdmissions, p.Budget.Overshoot)
	}
	for _, e := range s.Events {
		r := "not recovered"
		if e.RecoverySeconds != nil {
			r = fmt.Sprintf("%.3f s", *e.RecoverySeconds)
		}
		fmt.Fprintf(&b, "%-24s at %6.2f s: %s, %d errors before\n", e.Name, e.AtSeconds, r, e.ErrorsBeforeRecovery)
	}
	if st := s.Store; st != nil {
		for _, o := range st.Outages {
			fmt.Fprintf(&b, "%s at %.1f s, store errors logged:", o.Event, o.AtSeconds)
			for _, c := range o.Classes {
				fmt.Fprintf(&b, " %s %d (%.1f-%.1f s)", c.Class, c.Lines, c.FirstSeconds, c.LastSeconds)
			}
			fmt.Fprintln(&b)
		}
		for _, r := range st.Restarts {
			fmt.Fprintf(&b, "redis back at %.1f s:", r.AtSeconds)
			if r.LoadedSeconds != nil {
				fmt.Fprintf(&b, " AOF loaded in %.3f s,", *r.LoadedSeconds)
			}
			if r.ReadySeconds != nil {
				fmt.Fprintf(&b, " ready after %.3f s,", *r.ReadySeconds)
			}
			fmt.Fprintf(&b, " %d LOADING errors,", r.LoadingErrors)
			if r.DegradedSeconds != nil {
				fmt.Fprintf(&b, " replicas degraded for %.3f s\n", *r.DegradedSeconds)
			} else {
				fmt.Fprintf(&b, " not every replica recovered\n")
			}
		}
	}
	if f := s.Flap; f != nil {
		for _, o := range f.Outages {
			fmt.Fprintf(&b, "outage %d %6.1f-%6.1f s: %3d requests, %s spent, overshoot after %s\n",
				o.N, o.FromSeconds, o.ToSeconds, o.Requests, o.Spent, o.OvershootAfter)
		}
		fmt.Fprintf(&b, "overshoot %s over T = %.1f s, %d processes; slope %s per outage (R² %.3f); per-outage bound %s, refill bound %s\n",
			f.Overshoot, f.TSeconds, f.Processes, f.Slope, f.R2, f.PerOutageBound, f.RefillBound)
		if k := f.Kill; k != nil {
			fmt.Fprintf(&b, "%s killed %.1f s into an outage: %s never reached Redis (bound %s)\n", k.Target, k.IntoOutageSeconds, k.Lost, k.Bound)
		}
	}
	return b.String()
}

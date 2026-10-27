// Package results renders RESULTS.md from the raw result files under
// eval/results/. Every number in RESULTS.md comes from those files; the
// renderer only selects, subtracts and formats, and its output is
// deterministic so re-rendering unchanged inputs is byte-stable.
package results

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/TheAsianFish/switchboard/internal/bench"
)

// Q1Dir is the subdirectory of the results directory written by
// eval/bench/q1.sh.
const Q1Dir = "q1-overhead"

// Meta describes a Q1 run as a whole; eval/bench/q1.sh writes it as
// meta.json next to the per-run files.
type Meta struct {
	// Commit is the short git commit the binary was built from, with a
	// "-dirty" suffix if the work tree had uncommitted changes.
	Commit string `json:"commit"`
	// Date is when the run started, RFC 3339 in UTC.
	Date         string       `json:"date"`
	FakeProvider FakeSettings `json:"fake_provider"`
	// Command is the command line that produced the run.
	Command string `json:"command"`
}

// FakeSettings are the fake provider's flags for the run.
type FakeSettings struct {
	Latency       string `json:"latency"`
	Tokens        int    `json:"tokens"`
	TokenInterval string `json:"token_interval"`
}

// Data is what the RESULTS.md template sees. A nil section has not been
// measured yet.
type Data struct {
	Q1  *Q1
	SDK *SDK
}

// Q1 is the gateway overhead section.
type Q1 struct {
	Meta Meta
	// FakeLatency and FakeTokenInterval are the fake provider's settings
	// in milliseconds for prose, e.g. "20 ms".
	FakeLatency, FakeTokenInterval string
	Rows                           []Q1Row
	// Hardware lists the distinct machines the runs reported; normally one.
	Hardware []string
	// Windows lists the distinct measurement windows, e.g. "60 s after 5 s
	// warm-up"; normally one.
	Windows []string
	// MaxSendDelayP99 is the worst p99 send delay of any run, in ms: how far
	// the generator itself fell behind its schedule.
	MaxSendDelayP99 string
}

// Q1Row compares the direct and gateway runs for one mode and rate. Latency
// values are milliseconds formatted with two decimals; a side that was not
// run, or had no successful request, shows "n/a".
type Q1Row struct {
	Mode   string // "non-streaming" or "streaming"
	Metric string // "total" or "TTFT"
	Rate   string // requests per second

	DirectP50, DirectP99   string
	GatewayP50, GatewayP99 string
	// AddedP50 and AddedP99 are gateway minus direct.
	AddedP50, AddedP99 string
	// Requests and Errors are "direct / gateway" over the measured window.
	Requests, Errors string
}

// Render loads the raw results under resultsDir and executes the template
// at templatePath into w. Nothing is written to w if anything fails.
func Render(resultsDir, templatePath string, w io.Writer) error {
	q1, err := loadQ1(filepath.Join(resultsDir, Q1Dir))
	if err != nil {
		return err
	}
	sdk, err := loadSDK(filepath.Join(resultsDir, SDKDir))
	if err != nil {
		return err
	}
	tmpl, err := template.New(filepath.Base(templatePath)).Option("missingkey=error").ParseFiles(templatePath)
	if err != nil {
		return fmt.Errorf("results: parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, Data{Q1: q1, SDK: sdk}); err != nil {
		return fmt.Errorf("results: render template: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("results: write: %w", err)
	}
	return nil
}

// run is one parsed result file.
type run struct {
	target string // "direct" or "gateway"
	stream bool
	rate   string
	res    bench.Result
}

// loadQ1 reads dir. A missing directory, or one without result files, gives
// a nil section.
func loadQ1(dir string) (*Q1, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	var runs []run
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || name == "meta.json" {
			continue
		}
		r, err := loadRun(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	if len(runs) == 0 {
		return nil, nil
	}
	var meta Meta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return nil, err
	}
	return buildQ1(meta, runs), nil
}

// loadRun reads one <target>-<mode>-<rate>.json file and checks that its
// name agrees with the configuration recorded inside.
func loadRun(path string) (run, error) {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	parts := strings.Split(base, "-")
	if len(parts) != 3 || (parts[0] != "direct" && parts[0] != "gateway") || (parts[1] != "nonstream" && parts[1] != "stream") {
		return run{}, fmt.Errorf("results: %s: name is not <direct|gateway>-<nonstream|stream>-<rate>.json", path)
	}
	rate, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return run{}, fmt.Errorf("results: %s: bad rate in name: %w", path, err)
	}
	r := run{target: parts[0], stream: parts[1] == "stream", rate: parts[2]}
	if err := readJSON(path, &r.res); err != nil {
		return run{}, err
	}
	if r.res.Config.Stream != r.stream || r.res.Config.Rate != rate {
		return run{}, fmt.Errorf("results: %s: name says stream=%v rate=%v, file says stream=%v rate=%v",
			path, r.stream, rate, r.res.Config.Stream, r.res.Config.Rate)
	}
	return r, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("results: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("results: %s: %w", path, err)
	}
	return nil
}

type rowKey struct {
	stream bool
	rate   float64
	label  string
}

// buildQ1 pairs direct and gateway runs into rows, non-streaming first, then
// by rate.
func buildQ1(meta Meta, runs []run) *Q1 {
	pairs := map[rowKey][2]*bench.Result{}
	hardware := map[string]bool{}
	windows := map[string]bool{}
	var maxDelay float64
	for i := range runs {
		r := &runs[i]
		rate, _ := strconv.ParseFloat(r.rate, 64)
		k := rowKey{stream: r.stream, rate: rate, label: r.rate}
		p := pairs[k]
		if r.target == "direct" {
			p[0] = &r.res
		} else {
			p[1] = &r.res
		}
		pairs[k] = p
		hardware[describeHardware(r.res.Hardware)] = true
		windows[fmt.Sprintf("%s s after %s s warm-up", num(r.res.Config.DurationSec), num(r.res.Config.WarmupSec))] = true
		maxDelay = max(maxDelay, r.res.SendDelay.P99)
	}
	keys := make([]rowKey, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b rowKey) int {
		if a.stream != b.stream {
			if a.stream {
				return 1
			}
			return -1
		}
		if a.rate != b.rate {
			if a.rate < b.rate {
				return -1
			}
			return 1
		}
		return strings.Compare(a.label, b.label)
	})
	q := &Q1{
		Meta:              meta,
		FakeLatency:       prose(meta.FakeProvider.Latency),
		FakeTokenInterval: prose(meta.FakeProvider.TokenInterval),
		Hardware:          sortedKeys(hardware),
		Windows:           sortedKeys(windows),
		MaxSendDelayP99:   fmt.Sprintf("%.3f", maxDelay),
	}
	for _, k := range keys {
		q.Rows = append(q.Rows, buildRow(k, pairs[k][0], pairs[k][1]))
	}
	return q
}

func buildRow(k rowKey, direct, gateway *bench.Result) Q1Row {
	row := Q1Row{Mode: "non-streaming", Metric: "total", Rate: k.label}
	pick := func(r *bench.Result) bench.Stats { return r.Latency }
	if k.stream {
		row.Mode, row.Metric = "streaming", "TTFT"
		pick = func(r *bench.Result) bench.Stats { return r.TTFT }
	}
	const na = "n/a"
	row.DirectP50, row.DirectP99, row.GatewayP50, row.GatewayP99 = na, na, na, na
	row.AddedP50, row.AddedP99 = na, na
	// A side without successful requests has no latency to show.
	measured := func(r *bench.Result) bool { return r != nil && pick(r).Count > 0 }
	if measured(direct) {
		s := pick(direct)
		row.DirectP50, row.DirectP99 = msf(s.P50), msf(s.P99)
	}
	if measured(gateway) {
		s := pick(gateway)
		row.GatewayP50, row.GatewayP99 = msf(s.P50), msf(s.P99)
	}
	if measured(direct) && measured(gateway) {
		d, g := pick(direct), pick(gateway)
		row.AddedP50, row.AddedP99 = added(g.P50-d.P50), added(g.P99-d.P99)
	}
	count := func(r *bench.Result, f func(*bench.Result) int) string {
		if r == nil {
			return na
		}
		return strconv.Itoa(f(r))
	}
	requests := func(r *bench.Result) int { return r.Requests }
	errs := func(r *bench.Result) int { return r.Errors() }
	row.Requests = count(direct, requests) + " / " + count(gateway, requests)
	row.Errors = count(direct, errs) + " / " + count(gateway, errs)
	return row
}

func describeHardware(h bench.Hardware) string {
	var parts []string
	if h.CPUModel != "" {
		parts = append(parts, h.CPUModel)
	}
	parts = append(parts, fmt.Sprintf("%d logical CPUs (GOMAXPROCS %d)", h.NumCPU, h.GOMAXPROCS))
	parts = append(parts, h.GOOS+"/"+h.GOARCH)
	if h.Kernel != "" {
		parts = append(parts, "kernel "+h.Kernel)
	}
	parts = append(parts, h.GoVersion)
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func msf(v float64) string { return fmt.Sprintf("%.2f", v) }

// added formats a difference with an explicit sign; a difference that rounds
// to zero is "+0.00", never "-0.00".
func added(v float64) string {
	v = math.Round(v*100) / 100
	if v == 0 {
		v = 0
	}
	return fmt.Sprintf("%+.2f", v)
}

// prose renders a Go duration string such as "20ms" as "20 ms"; anything
// that does not parse is shown as recorded.
func prose(d string) string {
	v, err := time.ParseDuration(d)
	if err != nil {
		return d
	}
	return num(float64(v)/float64(time.Millisecond)) + " ms"
}

// num formats a number of seconds without trailing zeros.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Main implements `switchboard eval results`. args excludes the subcommand
// names. It returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("results", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("results", "eval/results", "directory of raw result files")
	tmpl := fs.String("template", "eval/RESULTS.md.tmpl", "template to render")
	out := fs.String("out", "RESULTS.md", `file to write ("-" for stdout)`)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "results: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	var buf bytes.Buffer
	if err := Render(*dir, *tmpl, &buf); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *out == "-" {
		if _, err := stdout.Write(buf.Bytes()); err != nil {
			fmt.Fprintf(stderr, "results: %v\n", err)
			return 1
		}
		return 0
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintf(stderr, "results: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "wrote %s\n", *out)
	return 0
}

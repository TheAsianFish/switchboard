package stalleval

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
)

// breakerLogMsg is the gateway's warn line on every breaker transition.
const breakerLogMsg = "provider breaker state changed"

// breakerProvider is the provider the runs drive.
const breakerProvider = "fake"

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return wrap(path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return wrap(path, err)
	}
	return nil
}

// open opens path, through gzip if its name ends in .gz.
func open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, wrap(path, err)
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	z, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, wrap(path, err)
	}
	return struct {
		io.Reader
		io.Closer
	}{z, f}, nil
}

// eachLine calls fn with every line of path.
func eachLine(path string, fn func([]byte) error) error {
	r, err := open(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return wrap(path, err)
		}
	}
	if err := sc.Err(); err != nil {
		return wrap(path, err)
	}
	return nil
}

// readFirstTokenTimeout reads the fake provider's first_token_timeout
// from a gateway config; 0 when it is not set.
func readFirstTokenTimeout(path string) (time.Duration, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, wrap(path, err)
	}
	var cfg struct {
		Providers []struct {
			Name       string `yaml:"name"`
			Resilience struct {
				FirstTokenTimeout time.Duration `yaml:"first_token_timeout"`
			} `yaml:"resilience"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return 0, wrap(path, err)
	}
	for _, p := range cfg.Providers {
		if p.Name == breakerProvider {
			return p.Resilience.FirstTokenTimeout, nil
		}
	}
	return 0, wrap(path, errors.New("no provider named "+breakerProvider))
}

// readStall returns the first stall-on transition and the first stall-off
// after it.
func readStall(path string) (on, off time.Time, err error) {
	var s struct {
		Transitions []fake.Transition `json:"transitions"`
	}
	if err := readJSON(path, &s); err != nil {
		return on, off, err
	}
	for _, t := range s.Transitions {
		switch {
		case t.Stalled && on.IsZero():
			on = t.Time
		case !t.Stalled && !on.IsZero():
			return on, t.Time, nil
		}
	}
	return on, off, wrap(path, errNoStall)
}

func readRecords(path string) ([]bench.Record, error) {
	var out []bench.Record
	err := eachLine(path, func(line []byte) error {
		var r bench.Record
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func readLedger(path string) ([]fake.Entry, error) {
	r, err := open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	var body struct {
		Entries []fake.Entry `json:"entries"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, wrap(path, err)
	}
	return body.Entries, nil
}

// readLogTransitions reads the gateway's breaker warn lines, in log
// order (the gateway writes them under the breaker's lock, so in the
// order they happened). A gateway that does not log transitions gives
// none.
func readLogTransitions(path string, offset func(time.Time) float64) ([]Transition, error) {
	var out []Transition
	err := eachLine(path, func(line []byte) error {
		if !bytes.Contains(line, []byte(breakerLogMsg)) {
			return nil
		}
		var l struct {
			Time     time.Time `json:"time"`
			Msg      string    `json:"msg"`
			Provider string    `json:"provider"`
			State    string    `json:"state"`
		}
		if err := json.Unmarshal(line, &l); err != nil {
			// A line that is not JSON (a panic, a start-up message) is
			// not a transition.
			return nil
		}
		if l.Msg == breakerLogMsg && l.Provider == breakerProvider {
			out = append(out, Transition{AtSeconds: round6(offset(l.Time)), State: l.State})
		}
		return nil
	})
	return out, err
}

// readPollTransitions reads the /providers polls: a transition at every
// poll whose state differs from the previous poll's (the first poll's
// state counts as a transition only if it is not closed). It also
// returns the gaps between consecutive polls and their count.
func readPollTransitions(path string, start time.Time) ([]Transition, bench.Stats, int, error) {
	var (
		out   []Transition
		gaps  []time.Duration
		polls int
		prevT float64
		prev  = StateClosed
	)
	err := eachLine(path, func(line []byte) error {
		var p struct {
			T    json.Number `json:"t"`
			Body struct {
				Providers []struct {
					Provider string `json:"provider"`
					Breaker  string `json:"breaker"`
				} `json:"providers"`
			} `json:"body"`
		}
		if err := json.Unmarshal(line, &p); err != nil {
			return err
		}
		t, err := sinceStart(p.T, start)
		if err != nil {
			return err
		}
		for _, pr := range p.Body.Providers {
			if pr.Provider != breakerProvider {
				continue
			}
			if polls > 0 {
				gaps = append(gaps, time.Duration(math.Round((t-prevT)*1e9)))
			}
			polls++
			prevT = t
			if pr.Breaker != prev {
				out = append(out, Transition{AtSeconds: round6(t), State: pr.Breaker})
				prev = pr.Breaker
			}
		}
		return nil
	})
	return out, bench.Summarise(gaps), polls, err
}

// sinceStart turns a poll's Unix time in seconds, "1794476893.785922182",
// into seconds after start, subtracting the whole seconds before
// converting so the nanoseconds survive.
func sinceStart(n json.Number, start time.Time) (float64, error) {
	s := n.String()
	whole, frac, _ := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, err
	}
	f := 0.0
	if frac != "" {
		if f, err = strconv.ParseFloat("0."+frac, 64); err != nil {
			return 0, err
		}
	}
	return float64(w-start.Unix()) + f - float64(start.Nanosecond())/1e9, nil
}

// readMetrics reads the provider counters the run scraped.
func readMetrics(path string) (Metrics, error) {
	m := Metrics{Retries: map[string]int{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, wrap(path, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, err := parseSample(line)
		if err != nil {
			return m, wrap(path, err)
		}
		if labels["provider"] != breakerProvider {
			continue
		}
		switch name {
		case "switchboard_provider_first_token_timeouts_total":
			m.FirstTokenTimeouts += int(value)
			m.FirstTokenTimeoutsSeries = true
		case "switchboard_provider_retries_total":
			m.Retries[labels["kind"]] += int(value)
		case "switchboard_provider_breaker_state":
			m.BreakerState = ptr(value)
		}
	}
	if err := sc.Err(); err != nil {
		return m, wrap(path, err)
	}
	return m, nil
}

// parseSample parses one Prometheus text-format sample line, without a
// timestamp: name{k="v",...} value.
func parseSample(line string) (string, map[string]string, float64, error) {
	labels := map[string]string{}
	var name, rest string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", nil, 0, errors.New("bad sample " + strconv.Quote(line))
		}
		name, rest = line[:i], line[j+1:]
		for _, kv := range strings.Split(line[i+1:j], ",") {
			if kv == "" {
				continue
			}
			k, v, _ := strings.Cut(kv, "=")
			labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	} else {
		name, rest, _ = strings.Cut(line, " ")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		return "", nil, 0, errors.New("bad value in " + strconv.Quote(line))
	}
	return name, labels, v, nil
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

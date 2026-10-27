package cacheeval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/TheAsianFish/switchboard/internal/bench"
	"github.com/TheAsianFish/switchboard/internal/cache"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
)

// latencyWarmups is how many single-text calls are made, and not timed,
// before the embedding latency is measured.
const latencyWarmups = 10

// cacheConfig is the flags of `switchboard eval cache`.
type cacheConfig struct {
	data, out, ollama string
	models            []string
	target            float64
	fromSims          bool
	limit             int
	latencyCalls      int
	batch             int
}

// cacheMain runs the threshold sweep: `switchboard eval cache`.
//
// It embeds both sides of every pair through the gateway's own Ollama
// client, writes one sims-<model>-<set>.json per model and pair set, then
// computes summary.json from those files alone. With --from-sims it skips
// Ollama and recomputes summary.json from the sims files already in --out.
// The summary's RunInfo is the embedding run's (copied from the sims
// files) and its latency comes from latency.json written by that run, so
// re-running with --from-sims on unchanged files is byte-identical; the
// sweep is a pure function of the raw files.
func cacheMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("cache", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var c cacheConfig
	var models string
	flags.StringVar(&c.data, "data", "eval/cache/data", "directory of the committed pair sets")
	flags.StringVar(&c.out, "out", "eval/results/q3-cache", "directory for the raw similarity files and summary.json")
	flags.StringVar(&c.ollama, "ollama", envOr("OLLAMA_URL", "http://localhost:11434"), "Ollama base URL")
	flags.StringVar(&models, "models", "all-minilm,nomic-embed-text", "comma-separated embedding models")
	flags.Float64Var(&c.target, "target", 0.01, "false-hit rate the threshold must meet")
	flags.BoolVar(&c.fromSims, "from-sims", false, "skip Ollama and recompute summary.json from the sims files in --out")
	flags.IntVar(&c.limit, "limit", 0, "pairs per set, 0 for all (for smoke runs)")
	flags.IntVar(&c.latencyCalls, "latency-calls", 200, "single-text embedding calls timed per model")
	flags.IntVar(&c.batch, "batch", 64, "texts per embedding call when embedding the pair sets")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	for _, m := range strings.Split(models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			c.models = append(c.models, m)
		}
	}
	if len(c.models) == 0 {
		return errors.New("no models")
	}
	if c.batch < 1 || c.limit < 0 || c.latencyCalls < 0 || c.target <= 0 || c.target >= 1 {
		return errors.New("--batch must be positive, --limit and --latency-calls not negative, --target in (0, 1)")
	}
	sets := []string{SetPAWS, SetGSM}

	var data DataMeta
	if err := readJSONFile(filepath.Join(c.data, GSMMetaFile), &data); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	var (
		sims    []Sims
		latency []Latency
		info    RunInfo
	)
	if c.fromSims {
		var err error
		if sims, err = loadSims(c.out, c.models, sets); err != nil {
			return err
		}
		info = sims[0].RunInfo
		var lf latencyFile
		if err := readJSONFile(filepath.Join(c.out, LatencyFile), &lf); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		latency = lf.Latency
	} else {
		info = RunInfo{
			Commit:   gitCommit(ctx),
			Date:     time.Now().UTC().Format(time.RFC3339),
			Command:  "switchboard eval cache " + strings.Join(args, " "),
			Hardware: bench.DetectHardware(),
		}
		var err error
		if sims, latency, err = embedAll(ctx, c, sets, info, stdout); err != nil {
			return err
		}
	}

	s, err := Summarise(sims, c.models, sets, c.target, latency, data, info)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(c.out, SummaryFile), s); err != nil {
		return err
	}
	printSummary(stdout, s)
	_, _ = fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(c.out, SummaryFile))
	return nil
}

// LatencyFile holds the embedding latency of the run that wrote the sims
// files, so --from-sims can carry it into the summary.
const LatencyFile = "latency.json"

type latencyFile struct {
	RunInfo
	Latency []Latency `json:"embed_latency"`
}

// buildPairs reads the committed pair sets in file order, at most limit
// pairs each (0 for all).
func buildPairs(dataDir string, limit int) (map[string][]Pair, []GSMRow, error) {
	paws, err := readJSONL[PAWSRow](filepath.Join(dataDir, PAWSFile))
	if err != nil {
		return nil, nil, err
	}
	gsm, err := readJSONL[GSMRow](filepath.Join(dataDir, GSMFile))
	if err != nil {
		return nil, nil, err
	}
	out := map[string][]Pair{}
	for _, r := range paws {
		out[SetPAWS] = append(out[SetPAWS], Pair{ID: fmt.Sprintf("paws-%d", r.ID), A: r.Sentence1, B: r.Sentence2, Same: r.Label == 1})
	}
	for _, r := range gsm {
		out[SetGSM] = append(out[SetGSM], Pair{ID: fmt.Sprintf("gsm-%d-twin", r.Index), A: r.Question, B: r.Twin})
		if r.Positive() {
			out[SetGSM] = append(out[SetGSM], Pair{ID: fmt.Sprintf("gsm-%d-para", r.Index), A: r.Question, B: r.Paraphrase, Same: true})
		}
	}
	if limit > 0 {
		for k, v := range out {
			if len(v) > limit {
				out[k] = v[:limit]
			}
		}
	}
	return out, gsm, nil
}

// embedAll embeds every set with every model, writes the sims files and
// latency.json, and measures single-prompt embedding latency.
func embedAll(ctx context.Context, c cacheConfig, sets []string, info RunInfo, stdout io.Writer) ([]Sims, []Latency, error) {
	pairSets, gsm, err := buildPairs(c.data, c.limit)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return nil, nil, fmt.Errorf("cache: %w", err)
	}
	var prompts []string
	for _, r := range gsm {
		prompts = append(prompts, r.Question)
	}
	for _, r := range gsm {
		prompts = append(prompts, r.Twin)
	}
	var sims []Sims
	var latency []Latency
	for _, m := range c.models {
		client := &embed.Ollama{BaseURL: c.ollama, Name: m, Timeout: 2 * time.Minute}
		dims := 0
		for _, set := range sets {
			s, err := embedSet(ctx, client, set, pairSets[set], c.batch)
			if err != nil {
				return nil, nil, err
			}
			s.RunInfo = info
			dims = s.Dims
			if err := writeSims(filepath.Join(c.out, simsFileName(m, set)), s); err != nil {
				return nil, nil, err
			}
			_, _ = fmt.Fprintf(stdout, "%s %s: %d pairs, %d texts, %d dims, %.1f s\n", m, set, len(s.Pairs), s.Texts, s.Dims, s.EmbedSeconds)
			sims = append(sims, s)
		}
		if c.latencyCalls > 0 {
			l, err := measureLatency(ctx, client, prompts, c.latencyCalls)
			if err != nil {
				return nil, nil, err
			}
			l.Dims = dims
			latency = append(latency, l)
		}
	}
	if err := writeJSON(filepath.Join(c.out, LatencyFile), latencyFile{RunInfo: info, Latency: latency}); err != nil {
		return nil, nil, err
	}
	return sims, latency, nil
}

// embedSet embeds each distinct text of the set once and scores every pair.
func embedSet(ctx context.Context, e embed.Embedder, set string, pairs []Pair, batch int) (Sims, error) {
	index := map[string]int{}
	var texts []string
	for _, p := range pairs {
		for _, t := range []string{p.A, p.B} {
			if _, ok := index[t]; !ok {
				index[t] = len(texts)
				texts = append(texts, t)
			}
		}
	}
	start := time.Now()
	vecs := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += batch {
		v, err := e.Embed(ctx, texts[i:min(i+batch, len(texts))])
		if err != nil {
			return Sims{}, fmt.Errorf("cache: embed %s with %s: %w", set, e.Model(), err)
		}
		vecs = append(vecs, v...)
	}
	s := Sims{Model: e.Model(), Set: set, Texts: len(texts), EmbedSeconds: round3(time.Since(start).Seconds())}
	if len(vecs) > 0 {
		s.Dims = len(vecs[0])
	}
	for _, p := range pairs {
		s.Pairs = append(s.Pairs, SimPair{
			ID: p.ID, Same: p.Same,
			Cos:          round6(embed.Cosine(vecs[index[p.A]], vecs[index[p.B]])),
			Exact:        exactKey(p.A) == exactKey(p.B),
			Norm:         normalise(p.A) == normalise(p.B),
			NumbersEqual: cache.SameNumbers(p.A, p.B),
		})
	}
	return s, nil
}

// measureLatency times single-text calls, as a semantic lookup makes,
// after latencyWarmups untimed ones. Prompts are used in order and reused
// from the start only if there are fewer than the calls need.
func measureLatency(ctx context.Context, e embed.Embedder, prompts []string, calls int) (Latency, error) {
	if len(prompts) == 0 {
		return Latency{}, errors.New("cache: no prompts to time embedding latency with")
	}
	samples := make([]time.Duration, 0, calls)
	for i := range latencyWarmups + calls {
		text := prompts[i%len(prompts)]
		start := time.Now()
		if _, err := e.Embed(ctx, []string{text}); err != nil {
			return Latency{}, fmt.Errorf("cache: latency with %s: %w", e.Model(), err)
		}
		if i >= latencyWarmups {
			samples = append(samples, time.Since(start))
		}
	}
	st := bench.Summarise(samples)
	return Latency{Model: e.Model(), Calls: st.Count, P50Ms: st.P50, P99Ms: st.P99, MeanMs: st.Mean, Warmups: latencyWarmups}, nil
}

// simsFileName is sims-<model slug>-<set>.json.
func simsFileName(model, set string) string {
	slug := strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return '-'
	}, model)
	return "sims-" + slug + "-" + set + ".json"
}

// writeSims writes s indented, with one pair per line so the file stays
// small and diffs line by line.
func writeSims(path string, s Sims) error {
	pairs := s.Pairs
	s.Pairs = []SimPair{}
	head, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	const tail = "\"pairs\": []\n}"
	if !bytes.HasSuffix(head, []byte(tail)) {
		return fmt.Errorf("write %s: unexpected encoding", path)
	}
	var buf bytes.Buffer
	buf.Write(head[:len(head)-len(tail)])
	buf.WriteString("\"pairs\": [\n")
	for i, p := range pairs {
		b, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		buf.WriteString("    ")
		buf.Write(b)
		if i < len(pairs)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("  ]\n}\n")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// loadSims reads the sims file of every model and set.
func loadSims(dir string, models, sets []string) ([]Sims, error) {
	var out []Sims
	for _, m := range models {
		for _, set := range sets {
			var s Sims
			if err := readJSONFile(filepath.Join(dir, simsFileName(m, set)), &s); err != nil {
				return nil, err
			}
			if s.Model != m || s.Set != set {
				return nil, fmt.Errorf("cache: %s holds %s on %s", simsFileName(m, set), s.Model, s.Set)
			}
			out = append(out, s)
		}
	}
	return out, nil
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// gitCommit is the short commit of the work tree, with "-dirty" if it has
// uncommitted changes, or "unknown" outside a git checkout.
func gitCommit(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rev, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	commit := strings.TrimSpace(string(rev))
	if st, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output(); err == nil && len(bytes.TrimSpace(st)) > 0 {
		commit += "-dirty"
	}
	return commit
}

// printSummary writes the operating points and the chosen default.
func printSummary(w io.Writer, s Summary) {
	pct := func(r Rate) string { return fmt.Sprintf("%5.1f%% [%4.1f, %4.1f]", 100*r.P, 100*r.Low, 100*r.High) }
	op := func(o Operating) string {
		if !o.Found {
			return fmt.Sprintf("%-6s  %-22s  %-22s", "none", "", "")
		}
		return fmt.Sprintf("%.4f  %-22s  %-22s", o.Threshold, pct(o.HitRate), pct(o.FalseHitRate))
	}
	_, _ = fmt.Fprintf(w, "%-18s %-14s %-5s %5s %5s | %-6s  %-22s  %-22s | %-6s  %-22s  %-22s\n",
		"model", "set", "guard", "pos", "neg", "point", "hit rate", "false-hit rate", "upper", "hit rate", "false-hit rate")
	for _, c := range s.Curves {
		_, _ = fmt.Fprintf(w, "%-18s %-14s %-5v %5d %5d | %s | %s\n", c.Model, c.Set, c.Guard, c.Positives, c.Negatives, op(c.AtPoint), op(c.AtUpper))
	}
	for _, b := range s.Baselines {
		_, _ = fmt.Fprintf(w, "exact baseline %-14s hit %s false %s; normalised hit %s false %s\n",
			b.Set, pct(b.ExactHitRate), pct(b.ExactFalseHitRate), pct(b.NormalisedHitRate), pct(b.NormalisedFalseHitRate))
	}
	for _, j := range s.Joints {
		if j.Found {
			_, _ = fmt.Fprintf(w, "joint %-18s guard=%-5v threshold %.4f mean hit rate %.1f%%\n", j.Model, j.Guard, j.Threshold, 100*j.MeanHitRate)
		} else {
			_, _ = fmt.Fprintf(w, "joint %-18s guard=%-5v none\n", j.Model, j.Guard)
		}
	}
	for _, l := range s.Latency {
		_, _ = fmt.Fprintf(w, "embed latency %-18s %d dims: p50 %.2f ms, p99 %.2f ms, mean %.2f ms over %d calls\n", l.Model, l.Dims, l.P50Ms, l.P99Ms, l.MeanMs, l.Calls)
	}
	if s.Chosen.Model == "" {
		_, _ = fmt.Fprintln(w, "chosen: none, semantic matching off")
	} else {
		_, _ = fmt.Fprintf(w, "chosen: %s at %.4f with the number guard\n", s.Chosen.Model, s.Chosen.Threshold)
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// exactKey is the gateway's exact cache key for a one-turn prompt, so the
// baseline is exactly what the exact layer would match.
func exactKey(prompt string) string {
	k, _ := cache.KeysFor("eval", &core.Request{Messages: []core.Message{{Role: core.RoleUser, Text: prompt}}})
	return k.Exact
}

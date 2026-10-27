// Package cacheeval answers Q3: at what similarity threshold does the
// semantic cache's false-hit rate fall under 1%, and how much hit rate
// survives there. It builds the two labelled pair sets (PAWS-Wiki and
// GSM8K number perturbations), embeds them through the same embedding
// client the gateway uses, sweeps the threshold, and measures FLAT against
// HNSW query latency on a real Redis.
package cacheeval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Pair is one labelled pair: Same is true when a cache serving A's answer
// for B would be right.
type Pair struct {
	ID   string `json:"id"`
	A    string `json:"a"`
	B    string `json:"b"`
	Same bool   `json:"same"`
}

// PAWSRow is one row of the committed PAWS-Wiki file, as published.
type PAWSRow struct {
	ID        int    `json:"id"`
	Sentence1 string `json:"sentence1"`
	Sentence2 string `json:"sentence2"`
	Label     int    `json:"label"`
}

// GSMRow is one GSM8K question with its perturbed twin and its
// paraphrase, and the checks the paraphrase went through.
type GSMRow struct {
	// Index is the question's line number in GSM8K's test.jsonl, from 0.
	Index    int    `json:"index"`
	Question string `json:"question"`
	// Gold is the final answer from the dataset ("#### 18").
	Gold string `json:"gold"`
	// Twin is Question with one number changed; From and To say which.
	Twin string `json:"twin"`
	From string `json:"from"`
	To   string `json:"to"`
	// Paraphrase is the local model's rewording, empty if every attempt
	// failed the number check.
	Paraphrase string `json:"paraphrase"`
	Attempts   int    `json:"attempts"`
	NumbersOK  bool   `json:"numbers_ok"`
	// Solved is the local model's final answer to the paraphrase, and
	// SolveOutput its whole reply.
	Solved      string `json:"solved"`
	SolveOutput string `json:"solve_output"`
	AnswerOK    bool   `json:"answer_ok"`
}

// Positive reports whether the paraphrase passed both checks.
func (r GSMRow) Positive() bool { return r.NumbersOK && r.AnswerOK }

// DataMeta records how the pair sets were built.
type DataMeta struct {
	Date        string   `json:"date"`
	Command     string   `json:"command"`
	Model       string   `json:"model"`
	Seed        uint64   `json:"seed"`
	GSMEligible int      `json:"gsm8k_eligible"`
	GSMSampled  int      `json:"gsm8k_sampled"`
	NumbersOK   int      `json:"paraphrase_numbers_ok"`
	AnswerOK    int      `json:"paraphrase_answer_ok"`
	Positives   int      `json:"positives"`
	Negatives   int      `json:"negatives"`
	PAWSRows    int      `json:"paws_rows"`
	Sources     []string `json:"sources"`
}

const (
	pawsRowsURL = "https://datasets-server.huggingface.co/rows?dataset=google-research-datasets/paws&config=labeled_final&split=test"
	gsmTestURL  = "https://raw.githubusercontent.com/openai/grade-school-math/master/grade_school_math/data/test.jsonl"

	// PAWSFile is the committed PAWS-Wiki test split under the data
	// directory.
	PAWSFile = "paws-wiki-test.jsonl"
	// GSMFile holds the GSM8K questions with their twins and paraphrases.
	GSMFile = "gsm8k-pairs.jsonl"
	// GSMMetaFile records how GSMFile was built.
	GSMMetaFile = "gsm8k-pairs.meta.json"
)

const paraphraseSystem = "You rewrite math word problems. Reword the problem so that it asks exactly the same question with the same answer: keep every number exactly as written (digits stay digits, words stay words), keep every quantity, name and unit, and keep the final question. Change the wording and the sentence structure. Reply with the rewritten problem only."

const solveSystem = "Solve the math word problem step by step. End your reply with one last line of the form \"Answer: <number>\" with the final number only, no units."

// dataMain builds the pair sets: `switchboard eval cache-data`.
func dataMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cache-data", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "eval/cache/data", "directory for the committed pair sets")
	ollama := fs.String("ollama", envOr("OLLAMA_URL", "http://localhost:11434"), "Ollama base URL")
	model := fs.String("model", "qwen2.5:3b", "local model that paraphrases and solves GSM8K questions")
	n := fs.Int("n", 500, "GSM8K questions to sample")
	seed := fs.Uint64("seed", 1, "sampling seed")
	attempts := fs.Int("attempts", 3, "paraphrase attempts per question before giving up")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fmt.Errorf("cache-data: %w", err)
	}
	hc := &http.Client{Timeout: 5 * time.Minute}
	pawsPath := filepath.Join(*out, PAWSFile)
	if _, err := os.Stat(pawsPath); errors.Is(err, os.ErrNotExist) {
		rows, err := fetchPAWS(ctx, hc)
		if err != nil {
			return err
		}
		if err := writeJSONL(pawsPath, rows); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "wrote %s (%d rows)\n", pawsPath, len(rows))
	}
	pawsRows, err := readJSONL[PAWSRow](pawsPath)
	if err != nil {
		return err
	}

	qs, err := fetchGSM8K(ctx, hc)
	if err != nil {
		return err
	}
	rng := rand.New(rand.NewPCG(*seed, 0x5eed))
	var eligible []GSMRow
	for i, q := range qs {
		twin, from, to, ok := perturb(q.Question, q.Answer, rng)
		if !ok {
			continue
		}
		eligible = append(eligible, GSMRow{Index: i, Question: q.Question, Gold: gold(q.Answer), Twin: twin, From: from, To: to})
	}
	sample := sampleRows(eligible, *n, rng)

	// Resume: rows already in the output keep their paraphrase.
	gsmPath := filepath.Join(*out, GSMFile)
	done := map[int]GSMRow{}
	if prev, err := readJSONL[GSMRow](gsmPath); err == nil {
		for _, r := range prev {
			done[r.Index] = r
		}
	}
	f, err := os.OpenFile(gsmPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("cache-data: %w", err)
	}
	defer func() { _ = f.Close() }()
	llm := chatClient{base: strings.TrimRight(*ollama, "/"), model: *model, hc: hc}
	for k, row := range sample {
		if _, ok := done[row.Index]; ok {
			continue
		}
		start := time.Now()
		if err := buildPositive(ctx, llm, &row, *attempts); err != nil {
			return err
		}
		b, _ := json.Marshal(row)
		if _, err := f.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("cache-data: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "%d/%d q%d numbers=%v answer=%v (%s)\n", k+1, len(sample), row.Index, row.NumbersOK, row.AnswerOK, time.Since(start).Round(time.Millisecond))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cache-data: %w", err)
	}

	rows, err := readJSONL[GSMRow](gsmPath)
	if err != nil {
		return err
	}
	meta := DataMeta{
		Date: time.Now().UTC().Format(time.RFC3339), Command: commandLine("cache-data", args),
		Model: *model, Seed: *seed, GSMEligible: len(eligible), GSMSampled: len(rows), PAWSRows: len(pawsRows),
		Sources: []string{pawsRowsURL, gsmTestURL},
	}
	for _, r := range rows {
		if r.NumbersOK {
			meta.NumbersOK++
		}
		if r.AnswerOK {
			meta.AnswerOK++
		}
		if r.Positive() {
			meta.Positives++
		}
		meta.Negatives++
	}
	if err := writeJSON(filepath.Join(*out, GSMMetaFile), meta); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "gsm8k: %d sampled, %d positives, %d negatives\n", len(rows), meta.Positives, meta.Negatives)
	return nil
}

// buildPositive paraphrases row.Question until the numbers survive, then
// asks the model to solve the paraphrase and checks its answer against
// the gold one. A paraphrase that fails either check is kept in the file
// but not used as a positive.
func buildPositive(ctx context.Context, llm chatClient, row *GSMRow, attempts int) error {
	for a := 1; a <= attempts; a++ {
		// The first attempt is greedy; retries sample.
		temp := 0.0
		if a > 1 {
			temp = 0.7
		}
		p, err := llm.chat(ctx, paraphraseSystem, row.Question, temp, uint64(a), 400)
		if err != nil {
			return err
		}
		p = strings.TrimSpace(p)
		row.Paraphrase, row.Attempts = p, a
		if keepsNumbers(p, row.Question) && normalise(p) != normalise(row.Question) {
			row.NumbersOK = true
			break
		}
	}
	if !row.NumbersOK {
		return nil
	}
	out, err := llm.chat(ctx, solveSystem, row.Paraphrase, 0, 1, 768)
	if err != nil {
		return err
	}
	row.SolveOutput = out
	row.Solved = finalAnswer(out)
	row.AnswerOK = sameValue(row.Solved, row.Gold)
	return nil
}

// perturb changes one integer of question that the worked solution uses,
// so the twin asks a different question with (almost always) a different
// answer. Years are left alone.
func perturb(question, solution string, rng *rand.Rand) (twin, from, to string, ok bool) {
	used := map[string]bool{}
	for _, n := range numberSet(solution) {
		used[n] = true
	}
	type span struct{ start, end int }
	var cands []span
	rs := []rune(question)
	for i := 0; i < len(rs); {
		if !unicode.IsDigit(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && unicode.IsDigit(rs[j]) {
			j++
		}
		// Only plain integers: not part of 1,200 or 2.5.
		before := i > 0 && (rs[i-1] == '.' || rs[i-1] == ',') && i > 1 && unicode.IsDigit(rs[i-2])
		after := j < len(rs)-1 && (rs[j] == '.' || rs[j] == ',') && unicode.IsDigit(rs[j+1])
		s := string(rs[i:j])
		v, err := strconv.Atoi(s)
		if !before && !after && err == nil && v > 0 && (v < 1900 || v > 2100) && used[strconv.Itoa(v)] {
			cands = append(cands, span{i, j})
		}
		i = j
	}
	if len(cands) == 0 {
		return "", "", "", false
	}
	c := cands[rng.IntN(len(cands))]
	v, _ := strconv.Atoi(string(rs[c.start:c.end]))
	nv := v + 1
	if v >= 10 {
		nv = v + max(1, v/5)
	}
	from, to = strconv.Itoa(v), strconv.Itoa(nv)
	twin = string(rs[:c.start]) + to + string(rs[c.end:])
	return twin, from, to, true
}

func sampleRows(rows []GSMRow, n int, rng *rand.Rand) []GSMRow {
	if n >= len(rows) {
		return rows
	}
	idx := rng.Perm(len(rows))[:n]
	keep := make([]bool, len(rows))
	for _, i := range idx {
		keep[i] = true
	}
	out := make([]GSMRow, 0, n)
	for i, r := range rows {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}

// gold extracts GSM8K's final answer after "####".
func gold(solution string) string {
	_, a, ok := strings.Cut(solution, "####")
	if !ok {
		return ""
	}
	return strings.TrimSpace(a)
}

// finalAnswer takes the number on the reply's "Answer:" line, or else the
// last number in the reply.
func finalAnswer(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.ToLower(lines[i])
		if k := strings.LastIndex(l, "answer:"); k >= 0 {
			if ns := digitNumbers(lines[i][k:]); len(ns) > 0 {
				return ns[len(ns)-1]
			}
		}
	}
	if ns := digitNumbers(out); len(ns) > 0 {
		return ns[len(ns)-1]
	}
	return ""
}

// digitNumbers lists the digit numbers in s in order of appearance,
// thousands separators dropped and a leading minus kept.
func digitNumbers(s string) []string {
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if !unicode.IsDigit(rs[i]) {
			continue
		}
		var b strings.Builder
		if i > 0 && rs[i-1] == '-' {
			b.WriteRune('-')
		}
		j := i
		for j < len(rs) && (unicode.IsDigit(rs[j]) || ((rs[j] == ',' || rs[j] == '.') && j+1 < len(rs) && unicode.IsDigit(rs[j+1]))) {
			if rs[j] != ',' {
				b.WriteRune(rs[j])
			}
			j++
		}
		out = append(out, b.String())
		i = j
	}
	return out
}

func sameValue(a, b string) bool {
	x, err1 := strconv.ParseFloat(strings.ReplaceAll(a, ",", ""), 64)
	y, err2 := strconv.ParseFloat(strings.ReplaceAll(b, ",", ""), 64)
	return err1 == nil && err2 == nil && x == y
}

// normalise is the exact-match baseline's normalisation: whitespace runs
// collapsed and case folded.
func normalise(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

type gsmQuestion struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

func fetchGSM8K(ctx context.Context, hc *http.Client) ([]gsmQuestion, error) {
	body, err := get(ctx, hc, gsmTestURL)
	if err != nil {
		return nil, err
	}
	var qs []gsmQuestion
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var q gsmQuestion
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			return nil, fmt.Errorf("gsm8k: %w", err)
		}
		qs = append(qs, q)
	}
	return qs, sc.Err()
}

func fetchPAWS(ctx context.Context, hc *http.Client) ([]PAWSRow, error) {
	var rows []PAWSRow
	for offset := 0; ; offset += 100 {
		body, err := get(ctx, hc, fmt.Sprintf("%s&offset=%d&length=100", pawsRowsURL, offset))
		if err != nil {
			return nil, err
		}
		var page struct {
			Rows []struct {
				Row PAWSRow `json:"row"`
			} `json:"rows"`
			Total int `json:"num_rows_total"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("paws: %w", err)
		}
		for _, r := range page.Rows {
			rows = append(rows, r.Row)
		}
		if len(page.Rows) == 0 || len(rows) >= page.Total {
			return rows, nil
		}
	}
}

func get(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// chatClient calls Ollama's native chat API for the data build. It is eval
// tooling, not a gateway provider.
type chatClient struct {
	base, model string
	hc          *http.Client
}

func (c chatClient) chat(ctx context.Context, system, user string, temperature float64, seed uint64, maxTokens int) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": c.model, "stream": false,
		"options":  map[string]any{"temperature": temperature, "seed": seed, "num_predict": maxTokens},
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama chat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ollama chat: status %d: %s", resp.StatusCode, b)
	}
	var r struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("ollama chat: %w", err)
	}
	return r.Message.Content, nil
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

func writeJSONL[T any](path string, rows []T) error {
	var buf bytes.Buffer
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// The pair sets were built with the number check below, which is looser
// than the gateway's guard (cache.SameNumbers) on purpose: a paraphrase
// may move the numbers around and still ask the same question, so the
// builder compares them as a multiset. It is kept apart from the guard so
// that a change to the guard cannot change what the committed data means,
// and the eval measures what the stricter guard costs on these positives.

// keepsNumbers reports whether a and b contain the same numbers, each the
// same number of times, in any order.
func keepsNumbers(a, b string) bool {
	return slices.Equal(numberSet(a), numberSet(b))
}

// builderNumberWords are the spelled-out numbers the builder reads.
var builderNumberWords = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
	"six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10",
	"eleven": "11", "twelve": "12", "thirteen": "13", "fourteen": "14", "fifteen": "15",
	"sixteen": "16", "seventeen": "17", "eighteen": "18", "nineteen": "19",
	"twenty": "20", "thirty": "30", "forty": "40", "fifty": "50", "sixty": "60",
	"seventy": "70", "eighty": "80", "ninety": "90", "hundred": "100",
	"thousand": "1000", "million": "1000000", "billion": "1000000000",
}

// numberSet lists the numbers in text, sorted: digit runs with commas
// between digits dropped and decimals trimmed ("1,200.50" is "1200.5"),
// plus spelled-out numbers.
func numberSet(text string) []string {
	var out []string
	rs := []rune(text)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsDigit(r):
			j := i
			var b strings.Builder
		scan:
			for j < len(rs) {
				c := rs[j]
				switch {
				case unicode.IsDigit(c):
					b.WriteRune(c)
				case (c == ',' || c == '.') && j+1 < len(rs) && unicode.IsDigit(rs[j+1]):
					if c == '.' {
						b.WriteRune('.')
					}
				default:
					break scan
				}
				j++
			}
			out = append(out, canonicalNumber(b.String()))
			i = j
		case unicode.IsLetter(r):
			j := i
			for j < len(rs) && unicode.IsLetter(rs[j]) {
				j++
			}
			if d, ok := builderNumberWords[strings.ToLower(string(rs[i:j]))]; ok {
				out = append(out, d)
			}
			i = j
		default:
			i++
		}
	}
	slices.Sort(out)
	return out
}

// canonicalNumber drops leading zeros and trailing decimal zeros. More
// than one decimal point (a version string, a date) is kept as written.
func canonicalNumber(s string) string {
	if strings.Count(s, ".") > 1 {
		return s
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && len(s) <= 15 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return s
}

package routereval

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	gsmTestURL = "https://raw.githubusercontent.com/openai/grade-school-math/master/grade_school_math/data/test.jsonl"
	mmluURL    = "https://people.eecs.berkeley.edu/~hendrycks/data.tar"
	// dataSeed fixes which questions are drawn; changing it changes the
	// eval set, so it is part of the set's identity.
	dataSeed = 5
)

// DataMeta records where the eval set came from, written beside it.
type DataMeta struct {
	Sources map[string]string `json:"sources"`
	Seed    uint64            `json:"seed"`
	GSM8K   int               `json:"gsm8k"`
	MMLU    int               `json:"mmlu"`
	// Subjects is how many MMLU subjects are represented.
	Subjects int    `json:"subjects"`
	Built    string `json:"built"`
	Command  string `json:"command"`
	Licences string `json:"licences"`
}

func dataMain(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eval router data", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "eval/router/data", "directory for questions.jsonl")
	nGSM := fs.Int("gsm8k", 300, "GSM8K test questions to draw")
	nMMLU := fs.Int("mmlu", 300, "MMLU test questions to draw, stratified over subjects")
	if err := fs.Parse(args); err != nil {
		return err
	}
	hc := &http.Client{Timeout: 10 * time.Minute}
	gsm, err := fetchGSM8K(ctx, hc)
	if err != nil {
		return err
	}
	mmlu, err := fetchMMLU(ctx, hc)
	if err != nil {
		return err
	}
	qs := append(SampleGSM8K(gsm, *nGSM), SampleMMLU(mmlu, *nMMLU)...)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(*out, "questions.jsonl"), qs); err != nil {
		return err
	}
	subjects := map[string]bool{}
	for _, q := range qs {
		if q.Subject != "" {
			subjects[q.Subject] = true
		}
	}
	meta := DataMeta{
		Sources: map[string]string{DatasetGSM8K: gsmTestURL, DatasetMMLU: mmluURL},
		Seed:    dataSeed, GSM8K: *nGSM, MMLU: *nMMLU, Subjects: len(subjects),
		Built:    time.Now().UTC().Format(time.RFC3339),
		Command:  "switchboard eval router data",
		Licences: "GSM8K (openai/grade-school-math) MIT; MMLU (hendrycks/test) MIT",
	}
	if err := writeJSON(filepath.Join(*out, "questions.meta.json"), meta); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "wrote %d questions (%d GSM8K, %d MMLU over %d subjects) to %s\n",
		len(qs), *nGSM, *nMMLU, len(subjects), *out)
	return nil
}

type gsmRaw struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

func fetchGSM8K(ctx context.Context, hc *http.Client) ([]gsmRaw, error) {
	body, err := get(ctx, hc, gsmTestURL)
	if err != nil {
		return nil, err
	}
	var qs []gsmRaw
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var q gsmRaw
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			return nil, fmt.Errorf("gsm8k: %w", err)
		}
		qs = append(qs, q)
	}
	return qs, sc.Err()
}

// SampleGSM8K draws n questions with a fixed seed and keeps them in test
// file order.
func SampleGSM8K(rows []gsmRaw, n int) []Question {
	rng := rand.New(rand.NewPCG(dataSeed, 1))
	idx := rng.Perm(len(rows))[:min(n, len(rows))]
	slices.Sort(idx)
	out := make([]Question, 0, len(idx))
	for _, i := range idx {
		_, ans, _ := strings.Cut(rows[i].Answer, "####")
		out = append(out, Question{
			ID: fmt.Sprintf("gsm8k-%04d", i), Dataset: DatasetGSM8K,
			Question: strings.TrimSpace(rows[i].Question),
			Answer:   strings.ReplaceAll(strings.TrimSpace(ans), ",", ""),
		})
	}
	return out
}

type mmluRaw struct {
	Question string   `json:"question"`
	Subject  string   `json:"subject"`
	Choices  []string `json:"choices"`
	Answer   int      `json:"answer"`
}

// fetchMMLU reads every test row from the original release's tarball
// (data/test/<subject>_test.csv: question, four options, answer letter).
func fetchMMLU(ctx context.Context, hc *http.Client) ([]mmluRaw, error) {
	body, err := get(ctx, hc, mmluURL)
	if err != nil {
		return nil, err
	}
	var rows []mmluRaw
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("mmlu: %w", err)
		}
		dir, name := path.Split(h.Name)
		if path.Base(path.Clean(dir)) != "test" || !strings.HasSuffix(name, "_test.csv") {
			continue
		}
		subject := strings.TrimSuffix(name, "_test.csv")
		recs, err := csv.NewReader(tr).ReadAll()
		if err != nil {
			return nil, fmt.Errorf("mmlu %s: %w", name, err)
		}
		for _, r := range recs {
			if len(r) != 6 || len(r[5]) != 1 || r[5][0] < 'A' || r[5][0] > 'D' {
				return nil, fmt.Errorf("mmlu %s: malformed row %q", name, r)
			}
			rows = append(rows, mmluRaw{Question: r[0], Subject: subject, Choices: r[1:5], Answer: int(r[5][0] - 'A')})
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("mmlu: no test rows in the archive")
	}
	return rows, nil
}

// SampleMMLU draws n questions stratified over subjects: an equal share
// from every subject, and the remainder one each from subjects drawn with
// the fixed seed. Rows keep the order they have in the test split.
func SampleMMLU(rows []mmluRaw, n int) []Question {
	bySubject := map[string][]int{}
	var subjects []string
	for i, r := range rows {
		if _, ok := bySubject[r.Subject]; !ok {
			subjects = append(subjects, r.Subject)
		}
		bySubject[r.Subject] = append(bySubject[r.Subject], i)
	}
	slices.Sort(subjects)
	if len(subjects) == 0 {
		return nil
	}
	rng := rand.New(rand.NewPCG(dataSeed, 2))
	share := map[string]int{}
	for _, s := range subjects {
		share[s] = n / len(subjects)
	}
	for _, k := range rng.Perm(len(subjects))[:n%len(subjects)] {
		share[subjects[k]]++
	}
	var out []Question
	for _, s := range subjects {
		idx := bySubject[s]
		pick := rng.Perm(len(idx))[:min(share[s], len(idx))]
		slices.Sort(pick)
		for _, k := range pick {
			r := rows[idx[k]]
			if len(r.Choices) != 4 || r.Answer < 0 || r.Answer > 3 {
				continue
			}
			out = append(out, Question{
				ID: fmt.Sprintf("mmlu-%s-%03d", s, k), Dataset: DatasetMMLU, Subject: s,
				Question: strings.TrimSpace(r.Question), Choices: r.Choices,
				Answer: string(rune('A' + r.Answer)),
			})
		}
	}
	return out
}

func get(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			last = fmt.Errorf("get %s: %w", url, err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			last = fmt.Errorf("get %s: %w", url, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("get %s: status %d", url, resp.StatusCode)
			continue
		}
		return body, nil
	}
	return nil, last
}

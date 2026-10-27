package cacheeval

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/embed"
)

func TestSimsFileName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"all-minilm":              "sims-all-minilm-paws-wiki.json",
		"nomic-embed-text:latest": "sims-nomic-embed-text-latest-paws-wiki.json",
		"org/model.v2":            "sims-org-model-v2-paws-wiki.json",
	}
	for model, want := range tests {
		if got := simsFileName(model, SetPAWS); got != want {
			t.Errorf("simsFileName(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestEmbedSet(t *testing.T) {
	t.Parallel()
	f := &embed.Fake{Dims: 32}
	pairs := []Pair{
		{ID: "a", A: "How many eggs?", B: "how  many EGGS?", Same: true},
		{ID: "b", A: "Tom has 3 apples", B: "Tom has 4 apples"},
		{ID: "c", A: "Tom has 3 apples", B: "Tom has 3 apples \n"},
	}
	s, err := embedSet(context.Background(), f, SetGSM, pairs, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Five distinct texts in batches of two: three calls.
	if calls, texts := f.Calls(); calls != 3 || texts != 5 || s.Texts != 5 || s.Dims != 32 {
		t.Errorf("calls %d texts %d, sims %d texts %d dims", calls, texts, s.Texts, s.Dims)
	}
	want := []SimPair{
		{ID: "a", Same: true, Cos: 1, Norm: true, NumbersEqual: true},
		{ID: "b", Cos: s.Pairs[1].Cos},
		{ID: "c", Cos: 1, Exact: true, Norm: true, NumbersEqual: true},
	}
	if !reflect.DeepEqual(s.Pairs, want) {
		t.Errorf("pairs = %+v\nwant %+v", s.Pairs, want)
	}
	if c := s.Pairs[1].Cos; c <= 0.5 || c >= 1 {
		t.Errorf("cosine of the perturbed pair = %v", c)
	}
}

func TestMeasureLatency(t *testing.T) {
	t.Parallel()
	f := &embed.Fake{}
	l, err := measureLatency(context.Background(), f, []string{"a", "b"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := f.Calls(); calls != latencyWarmups+5 || l.Calls != 5 || l.Warmups != latencyWarmups || l.Model != embed.FakeModel {
		t.Errorf("latency = %+v after %d calls", l, calls)
	}
	if _, err := measureLatency(context.Background(), f, nil, 5); err == nil {
		t.Error("no prompts accepted")
	}
}

// The summary is a function of the sims files: --from-sims twice gives the
// same bytes, and the sims files survive the round trip.
func TestFromSimsIsDeterministic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	info := RunInfo{Commit: "abc", Date: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC).Format(time.RFC3339), Command: "switchboard eval cache"}
	var written []Sims
	for _, m := range []string{"m1", "m2"} {
		for _, set := range []string{SetPAWS, SetGSM} {
			s := Sims{RunInfo: info, Model: m, Set: set, Dims: 8, Texts: 4,
				Pairs: concat(pairs(3, true, 0.9, true), pairs(400, false, 0.75, true), pairs(1, false, 0.95, false))}
			if err := writeSims(filepath.Join(dir, simsFileName(m, set)), s); err != nil {
				t.Fatal(err)
			}
			written = append(written, s)
		}
	}
	read, err := loadSims(dir, []string{"m1", "m2"}, []string{SetPAWS, SetGSM})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, written) {
		t.Fatal("sims files do not round-trip")
	}
	args := []string{"--from-sims", "--out", dir, "--data", t.TempDir(), "--models", "m1,m2"}
	var out bytes.Buffer
	render := func() []byte {
		t.Helper()
		if err := cacheMain(context.Background(), args, &out, &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		b, err := os.ReadFile(filepath.Join(dir, SummaryFile))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first, second := render(), render()
	if !bytes.Equal(first, second) {
		t.Error("two --from-sims runs differ")
	}
	if !bytes.Contains(first, []byte(`"commit": "abc"`)) || !bytes.Contains(out.Bytes(), []byte("chosen: m1 at 0.7525")) {
		t.Errorf("summary or table unexpected:\n%s", out.String())
	}
	if err := cacheMain(context.Background(), []string{"--from-sims", "--out", t.TempDir()}, &out, &out); err == nil {
		t.Error("--from-sims without sims files succeeded")
	}
}

package cacheeval

import (
	"fmt"

	"github.com/TheAsianFish/switchboard/internal/bench"
)

// Result file names under the Q3 results directory.
const (
	// SummaryFile holds the threshold sweep for every model and pair set
	// and the threshold the gateway ships with.
	SummaryFile = "summary.json"
)

// IndexFileName is the file holding the FLAT against HNSW check for one
// vector length.
func IndexFileName(dims int) string { return fmt.Sprintf("index-%d.json", dims) }

// Pair set names.
const (
	SetPAWS = "paws-wiki"
	SetGSM  = "gsm8k-perturb"
)

// RunInfo identifies a run.
type RunInfo struct {
	Commit   string         `json:"commit"`
	Date     string         `json:"date"`
	Command  string         `json:"command"`
	Hardware bench.Hardware `json:"hardware"`
}

// SimPair is one pair's similarity under one embedding model. It is the
// raw output the sweep is computed from, so the sweep can be re-run
// without Ollama.
type SimPair struct {
	ID   string `json:"id"`
	Same bool   `json:"same"`
	// Cos is the cosine similarity of the two sides' embeddings.
	Cos float64 `json:"cos"`
	// Exact: the two sides have the same exact key in the gateway
	// (cache.KeysFor). Norm: equal after collapsing whitespace and folding case (the
	// plan's exact-cache baseline). NumbersEqual: the same numbers occur
	// in both (the number guard passes).
	Exact        bool `json:"exact"`
	Norm         bool `json:"norm"`
	NumbersEqual bool `json:"numbers_equal"`
}

// Sims is the raw file for one model and one pair set.
type Sims struct {
	RunInfo
	Model string `json:"model"`
	Set   string `json:"set"`
	Dims  int    `json:"dims"`
	// EmbedSeconds is the wall time spent embedding the set's texts.
	EmbedSeconds float64   `json:"embed_seconds"`
	Texts        int       `json:"texts"`
	Pairs        []SimPair `json:"pairs"`
}

// Rate is a proportion with its Wilson 95% interval.
type Rate struct {
	K    int     `json:"k"`
	N    int     `json:"n"`
	P    float64 `json:"p"`
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// SweepPoint is one threshold of the sweep: a pair hits when its cosine
// is at least Threshold (and, with the guard, its numbers are equal).
type SweepPoint struct {
	Threshold float64 `json:"threshold"`
	// HitRate is over the positives, FalseHitRate over the negatives.
	HitRate      Rate `json:"hit_rate"`
	FalseHitRate Rate `json:"false_hit_rate"`
}

// Operating is the threshold a selection rule picked and what it gives.
// Found is false when no threshold in the sweep meets the rule.
type Operating struct {
	Found bool `json:"found"`
	SweepPoint
}

// Curve is the sweep for one model, pair set and guard setting.
type Curve struct {
	Model string `json:"model"`
	Set   string `json:"set"`
	// Guard is true when a hit also needs equal numbers.
	Guard     bool `json:"guard"`
	Positives int  `json:"positives"`
	Negatives int  `json:"negatives"`
	// AtPoint is the lowest threshold whose observed false-hit rate is at
	// most the target; AtUpper the lowest whose Wilson upper bound is.
	AtPoint Operating    `json:"at_point"`
	AtUpper Operating    `json:"at_upper"`
	Points  []SweepPoint `json:"points"`
}

// Baseline is an exact-match cache on one pair set: its hit rate on the
// positives is the floor a semantic cache must beat.
type Baseline struct {
	Set string `json:"set"`
	// Exact compares trimmed text (what the gateway's exact layer keys
	// on); Normalised also collapses whitespace and folds case.
	ExactHitRate           Rate `json:"exact_hit_rate"`
	ExactFalseHitRate      Rate `json:"exact_false_hit_rate"`
	NormalisedHitRate      Rate `json:"normalised_hit_rate"`
	NormalisedFalseHitRate Rate `json:"normalised_false_hit_rate"`
}

// Joint is one threshold for all pair sets: the highest of the per-set
// AtUpper thresholds, so the false-hit target holds on every set, with
// the rates each set gets there.
type Joint struct {
	Model     string       `json:"model"`
	Guard     bool         `json:"guard"`
	Found     bool         `json:"found"`
	Threshold float64      `json:"threshold"`
	PerSet    []SweepPoint `json:"per_set"`
	// MeanHitRate is the unweighted mean of the per-set hit rates.
	MeanHitRate float64 `json:"mean_hit_rate"`
}

// Latency is the time to embed one short prompt, as the gateway does on a
// semantic lookup.
type Latency struct {
	Model   string  `json:"model"`
	Calls   int     `json:"calls"`
	P50Ms   float64 `json:"p50_ms"`
	P99Ms   float64 `json:"p99_ms"`
	MeanMs  float64 `json:"mean_ms"`
	Dims    int     `json:"dims"`
	Warmups int     `json:"warmups"`
}

// Chosen is the gateway's shipped default: the model and threshold the
// rule picked, and the rule in words.
type Chosen struct {
	Model     string  `json:"model"`
	Threshold float64 `json:"threshold"`
	Guard     bool    `json:"guard"`
	Rule      string  `json:"rule"`
}

// Summary is the Q3 sweep result.
type Summary struct {
	RunInfo
	// Target is the false-hit target, 0.01.
	Target float64 `json:"target"`
	// Step is the grid's step up to FineFrom, FineStep its step above.
	Step      float64    `json:"step"`
	FineFrom  float64    `json:"fine_from"`
	FineStep  float64    `json:"fine_step"`
	Models    []string   `json:"models"`
	Sets      []string   `json:"sets"`
	Baselines []Baseline `json:"baselines"`
	Curves    []Curve    `json:"curves"`
	Joints    []Joint    `json:"joints"`
	Latency   []Latency  `json:"embed_latency"`
	Chosen    Chosen     `json:"chosen"`
	// DataMeta is how the GSM8K pairs were built.
	Data DataMeta `json:"data"`
}

// IndexLatency is one index configuration's query latency.
type IndexLatency struct {
	P50Ms  float64 `json:"p50_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MeanMs float64 `json:"mean_ms"`
}

// IndexRun is one index algorithm on the same data.
type IndexRun struct {
	// Algorithm is FLAT or HNSW.
	Algorithm string `json:"algorithm"`
	// Params are the HNSW parameters (M, EF_CONSTRUCTION, EF_RUNTIME);
	// empty for FLAT.
	Params       map[string]int `json:"params,omitempty"`
	BuildSeconds float64        `json:"build_seconds"`
	// Unfiltered is a KNN over every entry; Filtered restricts it to one
	// context's entries, the shape of a gateway lookup.
	Unfiltered IndexLatency `json:"unfiltered"`
	Filtered   IndexLatency `json:"filtered"`
	// RecallAt1 is the fraction of unfiltered queries whose top result is
	// the exact nearest neighbour (computed by brute force in Go), and
	// FilteredRecallAt1 the same within the query's context.
	RecallAt1         float64 `json:"recall_at_1"`
	FilteredRecallAt1 float64 `json:"filtered_recall_at_1"`
	IndexSizeMB       float64 `json:"index_size_mb"`
}

// IndexResult is the FLAT against HNSW check.
type IndexResult struct {
	RunInfo
	RedisVersion string `json:"redis_version"`
	Entries      int    `json:"entries"`
	Dims         int    `json:"dims"`
	Queries      int    `json:"queries"`
	// Contexts is how many distinct context tags the entries are spread
	// over; a filtered query sees Entries / Contexts of them.
	Contexts int `json:"contexts"`
	// Vectors says what the vectors are, for example "random unit".
	Vectors string     `json:"vectors"`
	Runs    []IndexRun `json:"runs"`
}

package routereval

import "github.com/TheAsianFish/switchboard/internal/bench"

// ResultsDir is the subdirectory of eval/results written by `switchboard
// eval router run`.
const ResultsDir = "q5-router"

// Cost units. A policy's cost is the sum over its requests of the chosen
// candidate's cost on that request.
const (
	// CostUSD is the gateway's price table applied to each reply's usage.
	CostUSD = "usd"
	// CostGFLOPs is 2 × parameters × (input + output tokens): the forward
	// pass's multiply-adds, the standard approximation for a dense
	// decoder. It is the cost axis when the candidates are local models,
	// which have no price.
	CostGFLOPs = "gflops"
)

// Point is one policy's result over the whole eval set.
type Point struct {
	Accuracy float64 `json:"accuracy"`
	// AccuracyCI is the Wilson 95% interval of Accuracy.
	AccuracyCI [2]float64 `json:"accuracy_ci"`
	// CostPer1K is the policy's cost for 1,000 requests drawn like the
	// eval set, in the summary's cost unit.
	CostPer1K float64 `json:"cost_per_1k"`
}

// CandidateResult is one candidate model answering every question.
type CandidateResult struct {
	Model string `json:"model"`
	Point
	// ByDataset is the accuracy per dataset.
	ByDataset map[string]float64 `json:"by_dataset"`
	// LatencyP50Ms and LatencyP95Ms are total latency through the gateway.
	LatencyP50Ms float64 `json:"latency_p50_ms"`
	LatencyP95Ms float64 `json:"latency_p95_ms"`
	// MeanInputTokens and MeanOutputTokens are the mean prompt and reply
	// lengths in the candidate's own tokens.
	MeanInputTokens  float64 `json:"mean_input_tokens"`
	MeanOutputTokens float64 `json:"mean_output_tokens"`
	// Truncated counts replies cut off by the output cap.
	Truncated int `json:"truncated"`
	// Nanodollars is what labelling this candidate cost.
	Nanodollars int64 `json:"nanodollars"`
}

// FrontierPoint is a router variant at one quality target, on held-out
// folds.
type FrontierPoint struct {
	Target float64 `json:"target"`
	Point
	// LineAccuracy is the random-mix line's accuracy at this point's cost
	// (the cheapest and the best candidate mixed to cost the same).
	LineAccuracy float64 `json:"line_accuracy"`
	// Gain is Accuracy − LineAccuracy.
	Gain float64 `json:"gain"`
	// HullAccuracy is the candidates' hull at this point's cost: the best
	// random mix of any two candidates. HullGain is Accuracy minus it.
	HullAccuracy float64 `json:"hull_accuracy"`
	HullGain     float64 `json:"hull_gain"`
	// Share is the fraction of requests sent to each candidate.
	Share map[string]float64 `json:"share"`
}

// Variant is one router configuration's held-out frontier.
type Variant struct {
	// Name is "router" (embedding plus cheap features), "router-cheap"
	// (cheap features only) or "router-cost" (router-cheap's predictions
	// routed by a per-question cost estimate; eval only, not served).
	Name        string `json:"name"`
	Description string `json:"description"`
	// Knob names what FrontierPoint.Target holds: "target" (the quality
	// target) or "cost weight" (the cost-aware rule's weight).
	Knob     string          `json:"knob"`
	Frontier []FrontierPoint `json:"frontier"`
	// AUCGain is the mean gain over the line across the line's cost
	// range, as a fraction like Gain; positive means above the line on
	// average.
	AUCGain float64 `json:"auc_gain"`
	// AUCHullGain is the same mean gain over the candidates' hull.
	AUCHullGain float64 `json:"auc_hull_gain"`
	// AUCGainCI and AUCHullGainCI are bootstrap 95% intervals of the two
	// means, resampling questions with the routing decisions held fixed.
	AUCGainCI     [2]float64 `json:"auc_gain_ci"`
	AUCHullGainCI [2]float64 `json:"auc_hull_gain_ci"`
	// MeanBrier is the mean Brier score of the per-candidate correctness
	// predictions on held-out folds, per candidate.
	Brier map[string]float64 `json:"brier"`
	// AUROC is the held-out area under the ROC curve of each candidate's
	// correctness prediction.
	AUROC map[string]float64 `json:"auroc"`
}

// OperatingPoint is the quality target the gateway ships, chosen by the
// rule fixed in PLAN.md: the lowest target whose held-out accuracy is at
// least 95% of the best candidate's.
type OperatingPoint struct {
	Variant string `json:"variant"`
	FrontierPoint
	// PctBestAccuracy and PctBestCost compare it with always sending to
	// the best candidate, in percent.
	PctBestAccuracy float64 `json:"pct_best_accuracy"`
	PctBestCost     float64 `json:"pct_best_cost"`
	// GainCI is a bootstrap 95% interval of Gain, resampling questions
	// and re-applying the selection rule in every resample.
	GainCI [2]float64 `json:"gain_ci"`
	// HullGainCI is the same interval for HullGain.
	HullGainCI [2]float64 `json:"hull_gain_ci"`
	// ShareByDataset is the routing share per dataset.
	ShareByDataset map[string]map[string]float64 `json:"share_by_dataset"`
	// Met is false when no target reached the rule's accuracy; the
	// operating point is then the most accurate frontier point.
	Met bool `json:"met"`
}

// DatasetRule is the reference that knows which dataset a question came
// from (which the router does not see): one fixed candidate for GSM8K and
// one for MMLU.
type DatasetRule struct {
	GSM8K string `json:"gsm8k"`
	MMLU  string `json:"mmlu"`
	Point
}

// Summary is everything the RESULTS.md section renders, written as
// summary.json.
type Summary struct {
	CostUnit string `json:"cost_unit"`
	// Questions is the number of eval questions every candidate answered.
	Questions int            `json:"questions"`
	ByDataset map[string]int `json:"by_dataset"`
	Folds     int            `json:"folds"`
	Seed      uint64         `json:"seed"`
	// Bootstrap is the number of bootstrap resamples behind GainCI.
	Bootstrap int `json:"bootstrap"`
	// AccuracyRule is the operating-point rule's share of the best
	// candidate's accuracy (0.95).
	AccuracyRule float64 `json:"accuracy_rule"`
	// L2 is the regularisation strength each fold chose by inner
	// cross-validation, per variant and candidate (mode over folds).
	L2         map[string]map[string]float64 `json:"l2"`
	Candidates []CandidateResult             `json:"candidates"`
	// Cheapest and Best name the line's two ends.
	Cheapest string `json:"cheapest"`
	Best     string `json:"best"`
	// Hull names the candidates on the hull, cheapest first. A candidate
	// between the ends that is on it sits above the line by itself.
	Hull []string `json:"hull"`
	// Oracle sends each question to the cheapest candidate that got it
	// right (the cheapest candidate when none did).
	Oracle         Point          `json:"oracle"`
	DatasetRules   []DatasetRule  `json:"dataset_rules"`
	Variants       []Variant      `json:"variants"`
	OperatingPoint OperatingPoint `json:"operating_point"`
}

// Meta describes the run, written as meta.json.
type Meta struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Command string `json:"command"`
	// Embedding is the embedding model the features used and its
	// dimension.
	Embedding  string `json:"embedding"`
	Dimensions int    `json:"dimensions"`
	// Hardware is the machine the labels were produced on.
	Hardware bench.Hardware `json:"hardware"`
	// LabelledFrom and LabelledTo bound the labels' timestamps.
	LabelledFrom string `json:"labelled_from"`
	LabelledTo   string `json:"labelled_to"`
	// LabelSeconds is the summed wall time of every labelled request.
	LabelSeconds float64 `json:"label_seconds"`
	// Nanodollars is the summed cost of every labelled request.
	Nanodollars int64 `json:"nanodollars"`
	// Params are the candidates' parameter counts in billions, for the
	// GFLOPs cost unit.
	Params map[string]float64 `json:"params,omitempty"`
	// Ollama is the Ollama version that served the local candidates.
	Ollama string `json:"ollama,omitempty"`
}

package results

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SDKDir is the subdirectory written by `make sdk-smoke`.
const SDKDir = "sdk-smoke"

// SDK is the client compatibility section: unmodified SDKs streaming
// through the gateway.
type SDK struct {
	Rows []SDKRow
	// Dates lists the distinct recording days.
	Dates []string
}

// SDKRow is one (SDK, model) case recorded by eval/sdk/smoke.py.
type SDKRow struct {
	SDK, Model, Backend string
	Passed              string // "yes" or "no"
	Chunks              int
	StopReason          string
	OutputTokens        int
	TTFT                string // ms as measured by the client, one sample
	Text                string
	Error               string
}

type sdkCase struct {
	SDK        string `json:"sdk"`
	SDKVersion string `json:"sdk_version"`
	Model      string `json:"model"`
	Backend    string `json:"backend"`
	Passed     bool   `json:"passed"`
	Error      string `json:"error"`
	RecordedAt string `json:"recorded_at"`
	Stream     *struct {
		Text       string         `json:"text"`
		Chunks     int            `json:"chunks"`
		StopReason string         `json:"stop_reason"`
		TTFTms     *float64       `json:"ttft_ms"`
		Usage      map[string]int `json:"usage"`
	} `json:"stream"`
}

// loadSDK reads every case file; a missing directory means not measured.
func loadSDK(dir string) (*SDK, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	if len(paths) == 0 {
		if _, err := os.Stat(dir); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("results: %w", err)
		}
		return nil, nil
	}
	sort.Strings(paths)
	out := &SDK{}
	dates := map[string]bool{}
	for _, p := range paths {
		var c sdkCase
		if err := readJSON(p, &c); err != nil {
			return nil, err
		}
		if c.SDK == "" || c.Model == "" {
			return nil, fmt.Errorf("results: %s: not an sdk-smoke case", p)
		}
		row := SDKRow{SDK: c.SDK + " " + c.SDKVersion, Model: c.Model, Backend: c.Backend, Passed: "no", Error: c.Error}
		if c.Passed {
			row.Passed = "yes"
		}
		if s := c.Stream; s != nil {
			row.Chunks, row.StopReason = s.Chunks, s.StopReason
			row.OutputTokens = s.Usage["completion_tokens"] + s.Usage["output_tokens"]
			row.Text = strings.ReplaceAll(s.Text, "|", `\|`)
			if s.TTFTms != nil {
				row.TTFT = fmt.Sprintf("%.0f", *s.TTFTms)
			}
		}
		if d, _, ok := strings.Cut(c.RecordedAt, "T"); ok {
			dates[d] = true
		}
		out.Rows = append(out.Rows, row)
	}
	out.Dates = sortedKeys(dates)
	return out, nil
}

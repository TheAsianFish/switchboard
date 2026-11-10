package routereval

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

// plotSummary is a made-up summary for plot tests only.
func plotSummary() Summary {
	pt := func(acc, cost float64) Point { return Point{Accuracy: acc, CostPer1K: cost} }
	fp := func(target, acc, cost float64) FrontierPoint {
		return FrontierPoint{Target: target, Point: pt(acc, cost), LineAccuracy: acc - 0.02, Gain: 0.02}
	}
	return Summary{
		CostUnit: CostGFLOPs, Questions: 600, Folds: 5,
		Candidates: []CandidateResult{
			{Model: "qwen2.5:0.5b", Point: pt(0.31, 1200)},
			{Model: "qwen2.5:1.5b", Point: pt(0.52, 3500)},
			{Model: "qwen2.5:3b & <co>", Point: pt(0.64, 7100)},
		},
		Cheapest: "qwen2.5:0.5b", Best: "qwen2.5:3b & <co>",
		Oracle:       pt(0.74, 2100),
		DatasetRules: []DatasetRule{{GSM8K: "qwen2.5:3b & <co>", MMLU: "qwen2.5:1.5b", Point: pt(0.6, 5300)}},
		Variants: []Variant{
			{Name: "router-cheap", Frontier: []FrontierPoint{fp(0.5, 0.45, 2500), fp(0.3, 0.40, 1900)}},
			{Name: "router", Frontier: []FrontierPoint{fp(0.3, 0.42, 1800), fp(0.5, 0.55, 3000), fp(0.7, 0.62, 5000)}},
		},
		OperatingPoint: OperatingPoint{Variant: "router", FrontierPoint: fp(0.7, 0.62, 5000)},
	}
}

// walk decodes svg as XML and counts its elements by name.
func walk(t *testing.T, svg []byte) map[string]int {
	t.Helper()
	counts := map[string]int{}
	dec := xml.NewDecoder(bytes.NewReader(svg))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("not XML: %v\n%s", err, svg)
		}
		if se, ok := tok.(xml.StartElement); ok {
			counts[se.Name.Local]++
		}
	}
	if counts["svg"] != 1 {
		t.Fatalf("want one root svg, got %d", counts["svg"])
	}
	return counts
}

func TestPlotFrontier(t *testing.T) {
	t.Parallel()
	small := plotSummary()
	small.Variants = small.Variants[1:]
	small.Variants[0].Frontier = small.Variants[0].Frontier[:1]
	small.DatasetRules = nil

	zero := plotSummary()
	for i := range zero.Candidates {
		zero.Candidates[i].CostPer1K = 0
	}
	zero.Oracle.CostPer1K = 0
	zero.DatasetRules[0].CostPer1K = 0
	for i := range zero.Variants {
		for j := range zero.Variants[i].Frontier {
			zero.Variants[i].Frontier[j].CostPer1K = 0
		}
	}
	zero.OperatingPoint.CostPer1K = 0

	wide := plotSummary()
	wide.CostUnit = CostUSD
	wide.Candidates[2].CostPer1K = 1200 * 50

	tests := map[string]struct {
		s        Summary
		variants int
		log      bool
	}{
		"two variants": {plotSummary(), 2, false},
		"one point":    {small, 1, false},
		"zero costs":   {zero, 2, false},
		"wide range":   {wide, 2, true},
		"empty":        {Summary{}, 0, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svg := PlotFrontier(tc.s)
			if !bytes.Equal(svg, PlotFrontier(tc.s)) {
				t.Error("two plots of the same summary differ")
			}
			counts := walk(t, svg)
			if got := bytes.Count(svg, []byte(`class="variant"`)); got != tc.variants {
				t.Errorf("variant groups = %d, want %d", got, tc.variants)
			}
			// One polyline per variant, plus the random-mix line when
			// both of its ends are known.
			line := 0
			if tc.s.Cheapest != "" {
				line = 1
			}
			if counts["polyline"] != tc.variants+line {
				t.Errorf("polylines = %d, want %d", counts["polyline"], tc.variants+line)
			}
			if got := strings.Contains(string(svg), "(log scale)"); got != tc.log {
				t.Errorf("log scale = %v, want %v", got, tc.log)
			}
			for _, bad := range []string{"NaN", "Inf", "<script", "http://", "@import"} {
				if bad == "http://" {
					// The namespace is the only URL allowed.
					if strings.Count(string(svg), bad) != 1 {
						t.Errorf("svg references a URL other than its namespace")
					}
					continue
				}
				if strings.Contains(string(svg), bad) {
					t.Errorf("svg contains %q", bad)
				}
			}
		})
	}
}

func TestPlotFrontierOrder(t *testing.T) {
	t.Parallel()
	svg := string(PlotFrontier(plotSummary()))
	// "router" is drawn last so it sits on top; the legend keeps the
	// summary's order.
	if strings.Index(svg, `data-name="router-cheap"`) > strings.Index(svg, `data-name="router"`) {
		t.Error("router drawn before router-cheap")
	}
	if strings.Index(svg, ">router-cheap (held out)<") > strings.Index(svg, ">router (held out)<") {
		t.Error("legend not in summary order")
	}
	for _, want := range []string{"knows the dataset", "GFLOPs per 1,000 requests", "qwen2.5:3b &amp; &lt;co&gt;", `class="operating"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("svg lacks %q", want)
		}
	}
}

func TestSVGNiceStep(t *testing.T) {
	t.Parallel()
	for in, want := range map[float64]float64{0.7: 1, 1: 1, 1.2: 2, 3: 5, 7: 10, 140: 200, 0: 1} {
		if got := svgNiceStep(in); got != want {
			t.Errorf("svgNiceStep(%v) = %v, want %v", in, got, want)
		}
	}
	for _, c := range []struct {
		v, step float64
		want    string
	}{{0, 1000, "0"}, {2000, 1000, "2,000"}, {10000, 2000, "10K"}, {2500000, 500000, "2.5M"}, {0.25, 0.05, "0.25"}, {0.001, 0.001, "0.001"}} {
		if got := svgTick(c.v, c.step); got != c.want {
			t.Errorf("svgTick(%v, %v) = %q, want %q", c.v, c.step, got, c.want)
		}
	}
}

package routereval

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Plot geometry and palette. The colours are the first categorical slots
// of a palette validated for colour-blind separation; everything that is
// not a router variant is drawn in ink so that the variants are the only
// colour on the chart.
const (
	svgW, svgH       = 720, 440
	svgPadL, svgPadR = 72, 24
	svgPadB          = 56

	svgSurface = "#fcfcfb"
	svgInk     = "#0b0b0b"
	svgInk2    = "#52514e"
	svgMuted   = "#898781"
	svgGrid    = "#e1e0d9"
	svgAxis    = "#c3c2b7"
	svgFont    = "system-ui, -apple-system, 'Segoe UI', sans-serif"

	// svgLogSpan is the cost ratio above which the x axis is logarithmic.
	svgLogSpan = 30
)

// svgVariantColours is the categorical order: "router" takes the first
// slot, "router-cheap" the second, any other variant the next free one.
var svgVariantColours = []string{"#2a78d6", "#eb6834", "#1baf7a", "#6250d6", "#e87ba4", "#008300", "#eda100", "#e34948"}

// PlotFrontier draws the held-out frontiers of s against the random-mix
// line, the candidates, the oracle and the dataset rules: accuracy in
// percent against cost per 1,000 requests. The SVG is standalone (no
// scripts, no external fonts) and the same summary always gives the same
// bytes.
func PlotFrontier(s Summary) []byte {
	p := newFrontierPlot(s)
	var b bytes.Buffer
	p.header(&b)
	p.axes(&b)
	p.line(&b)
	p.hull(&b)
	p.datasetRules(&b)
	p.variants(&b)
	p.candidates(&b)
	p.oracle(&b)
	p.operating(&b)
	p.legend(&b)
	b.WriteString("</svg>\n")
	return b.Bytes()
}

type frontierPlot struct {
	s        Summary
	log      bool
	x0, x1   float64 // cost domain (log10 of it when log)
	y0, y1   float64 // accuracy domain in percent
	xTicks   []float64
	yTicks   []float64
	yStep    float64
	xStep    float64
	colours  map[string]string
	top      float64  // the plot area's top edge, below the legend
	order    []string // variants drawn, in Summary order
	cheapest *CandidateResult
	best     *CandidateResult
}

func newFrontierPlot(s Summary) *frontierPlot {
	p := &frontierPlot{s: s, colours: map[string]string{}}
	for i := range s.Candidates {
		c := &s.Candidates[i]
		if c.Model == s.Cheapest {
			p.cheapest = c
		}
		if c.Model == s.Best {
			p.best = c
		}
	}
	// Colours by name first, so a variant keeps its colour whatever else
	// is in the summary.
	used := map[int]bool{}
	fixed := map[string]int{"router": 0, "router-cheap": 1}
	for _, v := range s.Variants {
		if i, ok := fixed[v.Name]; ok {
			p.colours[v.Name] = svgVariantColours[i]
			used[i] = true
		}
	}
	next := 0
	for _, v := range s.Variants {
		p.order = append(p.order, v.Name)
		if _, ok := p.colours[v.Name]; ok {
			continue
		}
		for used[next] && next < len(svgVariantColours)-1 {
			next++
		}
		p.colours[v.Name] = svgVariantColours[next]
		used[next] = true
	}

	_, legendBottom := p.layoutLegend()
	p.top = legendBottom + 14

	var costs, accs []float64
	add := func(pt Point) {
		costs = append(costs, pt.CostPer1K)
		accs = append(accs, 100*pt.Accuracy)
	}
	for _, c := range s.Candidates {
		add(c.Point)
	}
	add(s.Oracle)
	for _, r := range s.DatasetRules {
		add(r.Point)
	}
	for _, v := range s.Variants {
		for _, f := range v.Frontier {
			add(f.Point)
		}
	}
	if len(s.Variants) > 0 || s.OperatingPoint.Variant != "" {
		add(s.OperatingPoint.Point)
	}

	minPos, maxCost := math.Inf(1), 0.0
	for _, c := range costs {
		if c > 0 {
			minPos = min(minPos, c)
		}
		maxCost = max(maxCost, c)
	}
	if maxCost > 0 && maxCost/minPos > svgLogSpan {
		p.log = true
		p.x0, p.x1 = math.Floor(math.Log10(minPos)), math.Ceil(math.Log10(maxCost))
		if p.x1 == p.x0 {
			p.x1++
		}
		for e := p.x0; e <= p.x1; e++ {
			p.xTicks = append(p.xTicks, math.Pow(10, e))
		}
	} else {
		if maxCost <= 0 {
			maxCost = 1
		}
		p.xStep = svgNiceStep(maxCost / 5)
		p.x0, p.x1 = 0, math.Ceil(maxCost/p.xStep-1e-9)*p.xStep
		for i := 0; float64(i)*p.xStep <= p.x1+p.xStep/2; i++ {
			p.xTicks = append(p.xTicks, float64(i)*p.xStep)
		}
	}

	lo, hi := 100.0, 0.0
	for _, a := range accs {
		lo, hi = min(lo, a), max(hi, a)
	}
	if lo > hi {
		lo, hi = 0, 100
	}
	if hi-lo < 5 {
		lo, hi = lo-2.5, hi+2.5
	}
	p.yStep = svgNiceStep((hi - lo) / 5)
	p.y0 = math.Max(0, math.Floor(lo/p.yStep)*p.yStep)
	p.y1 = math.Min(100, math.Ceil(hi/p.yStep-1e-9)*p.yStep)
	if p.y1 <= p.y0 {
		p.y1 = p.y0 + p.yStep
	}
	for i := 0; p.y0+float64(i)*p.yStep <= p.y1+p.yStep/2; i++ {
		p.yTicks = append(p.yTicks, p.y0+float64(i)*p.yStep)
	}
	return p
}

// niceStep rounds v up to 1, 2 or 5 times a power of ten.
func svgNiceStep(v float64) float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*e >= v*(1-1e-9) {
			return m * e
		}
	}
	return 10 * e
}

func (p *frontierPlot) px(cost float64) float64 {
	left, right := float64(svgPadL), float64(svgW-svgPadR)
	var t float64
	if p.log {
		if cost <= 0 {
			return left
		}
		t = (math.Log10(cost) - p.x0) / (p.x1 - p.x0)
	} else {
		t = (cost - p.x0) / (p.x1 - p.x0)
	}
	return left + t*(right-left)
}

func (p *frontierPlot) py(acc float64) float64 {
	top, bottom := p.top, float64(svgH-svgPadB)
	return bottom - (100*acc-p.y0)/(p.y1-p.y0)*(bottom-top)
}

func (p *frontierPlot) xy(pt Point) (string, string) {
	return svgNum(p.px(pt.CostPer1K)), svgNum(p.py(pt.Accuracy))
}

func svgNum(v float64) string {
	v = math.Round(v*10) / 10
	if v == 0 {
		v = 0
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// esc escapes text for XML character data and attribute values.
var svgEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace

func svgPct(acc float64) string { return strconv.FormatFloat(100*acc, 'f', 1, 64) + "%" }

func (p *frontierPlot) costLabel() string {
	if p.s.CostUnit == CostUSD {
		return "$ per 1,000 requests"
	}
	return "GFLOPs per 1,000 requests"
}

func (p *frontierPlot) costText(v float64) string {
	if p.s.CostUnit == CostUSD {
		return "$" + strconv.FormatFloat(v, 'f', 4, 64)
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " GFLOPs"
}

// tick formats an axis value: in full below 10,000, compact above.
func svgTick(v, step float64) string {
	for _, u := range []struct {
		div    float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if math.Abs(v) >= 10000 && math.Abs(v) >= u.div {
			return svgTrimZeros(strconv.FormatFloat(v/u.div, 'f', 2, 64)) + u.suffix
		}
	}
	dec := 0
	if step > 0 && step < 1 {
		dec = int(math.Ceil(-math.Log10(step) - 1e-9))
	}
	s := strconv.FormatFloat(v, 'f', dec, 64)
	if dec == 0 && v >= 1000 {
		s = s[:len(s)-3] + "," + s[len(s)-3:]
	}
	return s
}

func svgTrimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

func (p *frontierPlot) header(b *bytes.Buffer) {
	title := "Held-out router frontier against the random-mix line"
	desc := fmt.Sprintf("Accuracy against %s for %d candidates, the random mix of %s and %s, the oracle and %d router variants on held-out folds.",
		p.costLabel(), len(p.s.Candidates), p.s.Cheapest, p.s.Best, len(p.s.Variants))
	fmt.Fprintf(b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="q5-title q5-desc" font-family="%s">`+"\n",
		svgW, svgH, svgW, svgH, svgFont)
	fmt.Fprintf(b, `<title id="q5-title">%s</title>`+"\n", svgEsc(title))
	fmt.Fprintf(b, `<desc id="q5-desc">%s</desc>`+"\n", svgEsc(desc))
	fmt.Fprintf(b, `<rect width="%d" height="%d" fill="%s"/>`+"\n", svgW, svgH, svgSurface)
	fmt.Fprintf(b, `<text x="%d" y="24" font-size="15" font-weight="600" fill="%s">%s</text>`+"\n", svgPadL, svgInk, svgEsc(title))
	sub := fmt.Sprintf("%d questions, %d-fold cross-validation by question; higher and further left is better", p.s.Questions, p.s.Folds)
	fmt.Fprintf(b, `<text x="%d" y="42" font-size="12" fill="%s">%s</text>`+"\n", svgPadL, svgInk2, svgEsc(sub))
}

func (p *frontierPlot) axes(b *bytes.Buffer) {
	left, right := float64(svgPadL), float64(svgW-svgPadR)
	top, bottom := p.top, float64(svgH-svgPadB)
	b.WriteString(`<g font-size="11" fill="` + svgMuted + `" style="font-variant-numeric: tabular-nums">` + "\n")
	for _, t := range p.yTicks {
		y := svgNum(bottom - (t-p.y0)/(p.y1-p.y0)*(bottom-top))
		fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1"/>`+"\n", svgNum(left), y, svgNum(right), y, svgGrid)
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="end" dominant-baseline="middle">%s</text>`+"\n", svgNum(left-8), y, svgTick(t, p.yStep))
	}
	for _, t := range p.xTicks {
		x := svgNum(p.px(t))
		fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1"/>`+"\n", x, svgNum(top), x, svgNum(bottom), svgGrid)
		label := svgTick(t, p.xStep)
		if p.log {
			label = svgTick(t, t)
		}
		fmt.Fprintf(b, `<text x="%s" y="%s" text-anchor="middle">%s</text>`+"\n", x, svgNum(bottom+18), label)
	}
	fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1"/>`+"\n", svgNum(left), svgNum(bottom), svgNum(right), svgNum(bottom), svgAxis)
	b.WriteString("</g>\n")
	xl := p.costLabel()
	if p.log {
		xl += " (log scale)"
	}
	fmt.Fprintf(b, `<text x="%s" y="%d" font-size="12" fill="%s" text-anchor="middle">%s</text>`+"\n", svgNum((left+right)/2), svgH-14, svgInk2, svgEsc(xl))
	fmt.Fprintf(b, `<text x="16" y="%s" font-size="12" fill="%s" text-anchor="middle" transform="rotate(-90 16 %s)">Accuracy (%%)</text>`+"\n", svgNum((top+bottom)/2), svgInk2, svgNum((top+bottom)/2))
}

// line is the random mix of the cheapest and the best candidate.
func (p *frontierPlot) line(b *bytes.Buffer) {
	if p.cheapest == nil || p.best == nil {
		return
	}
	// On a log axis the mix is not straight: sample it.
	n := 1
	if p.log {
		n = 64
	}
	pts := make([]string, 0, n+1)
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		c := p.cheapest.CostPer1K + t*(p.best.CostPer1K-p.cheapest.CostPer1K)
		a := p.cheapest.Accuracy + t*(p.best.Accuracy-p.cheapest.Accuracy)
		pts = append(pts, svgNum(p.px(c))+","+svgNum(p.py(a)))
	}
	fmt.Fprintf(b, `<polyline class="line" points="%s" fill="none" stroke="%s" stroke-width="2" stroke-dasharray="6 4" stroke-linecap="round"><title>random mix of %s and %s</title></polyline>`+"\n",
		strings.Join(pts, " "), svgMuted, svgEsc(p.s.Cheapest), svgEsc(p.s.Best))
}

// hull is the candidates' upper hull, drawn only when a middle candidate
// lifts it above the line.
func (p *frontierPlot) hull(b *bytes.Buffer) {
	if len(p.s.Hull) < 3 {
		return
	}
	byModel := map[string]Point{}
	for _, c := range p.s.Candidates {
		byModel[c.Model] = c.Point
	}
	n := 1
	if p.log {
		n = 64
	}
	var pts []string
	for i := 1; i < len(p.s.Hull); i++ {
		a, z := byModel[p.s.Hull[i-1]], byModel[p.s.Hull[i]]
		for k := 0; k <= n; k++ {
			if k == 0 && i > 1 {
				continue
			}
			t := float64(k) / float64(n)
			c := a.CostPer1K + t*(z.CostPer1K-a.CostPer1K)
			acc := a.Accuracy + t*(z.Accuracy-a.Accuracy)
			pts = append(pts, svgNum(p.px(c))+","+svgNum(p.py(acc)))
		}
	}
	fmt.Fprintf(b, `<polyline class="hull" points="%s" fill="none" stroke="%s" stroke-width="1.5" stroke-dasharray="2 3" stroke-linecap="round"><title>best random mix of two candidates</title></polyline>`+"\n",
		strings.Join(pts, " "), svgInk2)
}

func (p *frontierPlot) datasetRules(b *bytes.Buffer) {
	for _, r := range p.s.DatasetRules {
		x, y := p.xy(r.Point)
		fmt.Fprintf(b, `<circle class="rule" cx="%s" cy="%s" r="3.5" fill="%s" stroke="%s" stroke-width="1.5"><title>GSM8K to %s, MMLU to %s: %s at %s</title></circle>`+"\n",
			x, y, svgSurface, svgInk2, svgEsc(r.GSM8K), svgEsc(r.MMLU), svgPct(r.Accuracy), svgEsc(p.costText(r.CostPer1K)))
	}
}

func (p *frontierPlot) variants(b *bytes.Buffer) {
	// Secondary variants first, so "router" is drawn on top.
	idx := make([]int, len(p.s.Variants))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(i, j int) int {
		ri, rj := p.s.Variants[i].Name == "router", p.s.Variants[j].Name == "router"
		switch {
		case ri == rj:
			return 0
		case ri:
			return 1
		default:
			return -1
		}
	})
	for _, i := range idx {
		v := p.s.Variants[i]
		col := p.colours[v.Name]
		width, r, opacity := "2", "4", "1"
		if v.Name != "router" {
			width, r, opacity = "1.5", "3", "0.85"
		}
		pts := slices.Clone(v.Frontier)
		slices.SortStableFunc(pts, func(a, c FrontierPoint) int {
			switch {
			case a.CostPer1K < c.CostPer1K:
				return -1
			case a.CostPer1K > c.CostPer1K:
				return 1
			}
			return 0
		})
		coords := make([]string, len(pts))
		for k, f := range pts {
			x, y := p.xy(f.Point)
			coords[k] = x + "," + y
		}
		fmt.Fprintf(b, `<g class="variant" data-name="%s" opacity="%s">`+"\n", svgEsc(v.Name), opacity)
		fmt.Fprintf(b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="%s" stroke-linejoin="round" stroke-linecap="round"/>`+"\n", strings.Join(coords, " "), col, width)
		for _, f := range pts {
			x, y := p.xy(f.Point)
			fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="%s" fill="%s" stroke="%s" stroke-width="2"><title>%s, target %s: %s at %s, %s points over the line</title></circle>`+"\n",
				x, y, r, col, svgSurface, svgEsc(v.Name), strconv.FormatFloat(f.Target, 'f', 2, 64), svgPct(f.Accuracy), svgEsc(p.costText(f.CostPer1K)), svgSigned(100*f.Gain))
		}
		b.WriteString("</g>\n")
	}
}

func svgSigned(v float64) string {
	v = math.Round(v*10) / 10
	if v == 0 {
		v = 0
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func (p *frontierPlot) candidates(b *bytes.Buffer) {
	for _, c := range p.s.Candidates {
		x, y := p.xy(c.Point)
		fmt.Fprintf(b, `<circle class="candidate" cx="%s" cy="%s" r="5" fill="%s" stroke="%s" stroke-width="2"><title>%s: %s at %s</title></circle>`+"\n",
			x, y, svgInk, svgSurface, svgEsc(c.Model), svgPct(c.Accuracy), svgEsc(p.costText(c.CostPer1K)))
		p.label(b, p.px(c.CostPer1K), p.py(c.Accuracy), c.Model, svgInk)
	}
}

// label writes text beside a mark: to the right, or to the left near the
// right edge.
func (p *frontierPlot) label(b *bytes.Buffer, x, y float64, text, col string) {
	anchor, dx := "start", 9.0
	if x > float64(svgW-svgPadR)-140 {
		anchor, dx = "end", -9
	}
	fmt.Fprintf(b, `<text x="%s" y="%s" font-size="11" fill="%s" text-anchor="%s">%s</text>`+"\n", svgNum(x+dx), svgNum(y-7), col, anchor, svgEsc(text))
}

func (p *frontierPlot) oracle(b *bytes.Buffer) {
	x, y := p.px(p.s.Oracle.CostPer1K), p.py(p.s.Oracle.Accuracy)
	fmt.Fprintf(b, `<path class="oracle" d="%s" fill="%s" stroke="%s" stroke-width="2"><title>oracle: %s at %s</title></path>`+"\n",
		svgDiamond(x, y, 6), svgInk2, svgSurface, svgPct(p.s.Oracle.Accuracy), svgEsc(p.costText(p.s.Oracle.CostPer1K)))
	p.label(b, x, y, "oracle", svgInk2)
}

func svgDiamond(x, y, r float64) string {
	return "M" + svgNum(x) + "," + svgNum(y-r) + " L" + svgNum(x+r) + "," + svgNum(y) + " L" + svgNum(x) + "," + svgNum(y+r) + " L" + svgNum(x-r) + "," + svgNum(y) + " Z"
}

func (p *frontierPlot) operating(b *bytes.Buffer) {
	op := p.s.OperatingPoint
	if op.Variant == "" {
		return
	}
	col := p.colours[op.Variant]
	if col == "" {
		col = svgInk
	}
	x, y := p.px(op.CostPer1K), p.py(op.Accuracy)
	fmt.Fprintf(b, `<circle class="operating" cx="%s" cy="%s" r="9" fill="none" stroke="%s" stroke-width="2"><title>operating point (%s, target %s): %s at %s</title></circle>`+"\n",
		svgNum(x), svgNum(y), col, svgEsc(op.Variant), strconv.FormatFloat(op.Target, 'f', 2, 64), svgPct(op.Accuracy), svgEsc(p.costText(op.CostPer1K)))
	text := "operating point, target " + strconv.FormatFloat(op.Target, 'f', 2, 64)
	// Below and to the left: candidate labels sit above their points, and
	// near the dear end the operating point is close to the best one.
	anchor, dx := "end", -12.0
	if x < float64(svgPadL)+200 {
		anchor, dx = "start", 12
	}
	fmt.Fprintf(b, `<text x="%s" y="%s" font-size="11" font-weight="600" fill="%s" text-anchor="%s">%s</text>`+"\n", svgNum(x+dx), svgNum(y+20), svgInk, anchor, svgEsc(text))
}

// legendEntry is one key in the legend band.
type legendEntry struct {
	kind, colour, text string
}

func (p *frontierPlot) legendEntries() []legendEntry {
	var es []legendEntry
	for _, name := range p.order {
		es = append(es, legendEntry{"variant", p.colours[name], name + " (held out)"})
	}
	if p.cheapest != nil && p.best != nil {
		es = append(es, legendEntry{"line", svgMuted, "random mix of " + p.s.Cheapest + " and " + p.s.Best})
	}
	if len(p.s.Hull) >= 3 {
		es = append(es, legendEntry{"hull", svgInk2, "best mix of two candidates"})
	}
	es = append(es, legendEntry{"candidate", svgInk, "candidate model"}, legendEntry{"oracle", svgInk2, "oracle"})
	if len(p.s.DatasetRules) > 0 {
		es = append(es, legendEntry{"rule", svgInk2, "knows the dataset"})
	}
	if p.s.OperatingPoint.Variant != "" {
		col := p.colours[p.s.OperatingPoint.Variant]
		if col == "" {
			col = svgInk
		}
		es = append(es, legendEntry{"operating", col, "operating point"})
	}
	return es
}

// Legend band geometry: it sits between the subtitle and the plot, so it
// never covers data.
const (
	svgLegendTop  = 56.0
	svgLegendRowH = 18.0
	svgCharW      = 6.2 // approximate advance of an 11px sans character
)

// layoutLegend places the entries left to right, wrapping to the plot's
// width, and returns each entry's position and the band's bottom.
func (p *frontierPlot) layoutLegend() ([][2]float64, float64) {
	left, right := float64(svgPadL), float64(svgW-svgPadR)
	x, row := left, 0
	var pos [][2]float64
	for _, e := range p.legendEntries() {
		w := 30 + svgCharW*float64(len([]rune(e.text))) + 16
		if x > left && x+w > right {
			x, row = left, row+1
		}
		pos = append(pos, [2]float64{x, svgLegendTop + svgLegendRowH*(float64(row)+0.5)})
		x += w
	}
	return pos, svgLegendTop + svgLegendRowH*float64(row+1)
}

func (p *frontierPlot) legend(b *bytes.Buffer) {
	pos, _ := p.layoutLegend()
	fmt.Fprintf(b, `<g class="legend" font-size="11" fill="%s">`+"\n", svgInk2)
	for i, e := range p.legendEntries() {
		kx, cy := pos[i][0]+10, pos[i][1]
		switch e.kind {
		case "variant":
			fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="2"/><circle cx="%s" cy="%s" r="3.5" fill="%s"/>`+"\n", svgNum(kx-10), svgNum(cy), svgNum(kx+10), svgNum(cy), e.colour, svgNum(kx), svgNum(cy), e.colour)
		case "line":
			fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="2" stroke-dasharray="6 4"/>`+"\n", svgNum(kx-10), svgNum(cy), svgNum(kx+10), svgNum(cy), e.colour)
		case "hull":
			fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1.5" stroke-dasharray="2 3"/>`+"\n", svgNum(kx-10), svgNum(cy), svgNum(kx+10), svgNum(cy), e.colour)
		case "candidate":
			fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="4.5" fill="%s"/>`+"\n", svgNum(kx), svgNum(cy), e.colour)
		case "oracle":
			fmt.Fprintf(b, `<path d="%s" fill="%s"/>`+"\n", svgDiamond(kx, cy, 5), e.colour)
		case "rule":
			fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="3.5" fill="%s" stroke="%s" stroke-width="1.5"/>`+"\n", svgNum(kx), svgNum(cy), svgSurface, e.colour)
		case "operating":
			fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="6" fill="none" stroke="%s" stroke-width="2"/>`+"\n", svgNum(kx), svgNum(cy), e.colour)
		}
		fmt.Fprintf(b, `<text x="%s" y="%s" dominant-baseline="middle">%s</text>`+"\n", svgNum(pos[i][0]+28), svgNum(cy), svgEsc(e.text))
	}
	b.WriteString("</g>\n")
}

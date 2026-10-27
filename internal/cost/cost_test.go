package cost

import (
	"testing"

	"github.com/TheAsianFish/switchboard/internal/core"
)

func TestParseUSD(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want Nanos
		err  bool
	}{
		{"5", 5 * Dollar, false},
		{"0.10", 10 * Cent, false},
		{"$12.5", 12*Dollar + 50*Cent, false},
		{".25", 25 * Cent, false},
		{"0.000000001", 1, false},
		{"0.0000000001", 0, true},
		{"-1", 0, true},
		{"1e3", 0, true},
		{"", 0, true},
		{"99999999999", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseUSD(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("ParseUSD(%q) = %d, %v; want %d, err %v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestParsePerMTok(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want PerToken
		err  bool
	}{
		{"4", 4000, false},
		{"0.10", 100, false},
		{"0.01", 10, false},
		{"0.125", 125, false},
		{"0.0005", 0, true},
	}
	for _, tt := range tests {
		got, err := ParsePerMTok(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("ParsePerMTok(%q) = %d, %v; want %d, err %v", tt.in, got, err, tt.want, tt.err)
		}
		if err == nil {
			if back, _ := ParsePerMTok(got.PerMTok()); back != got {
				t.Errorf("PerMTok round trip of %q: %q", tt.in, got.PerMTok())
			}
		}
	}
}

func TestNanosString(t *testing.T) {
	t.Parallel()
	for n, want := range map[Nanos]string{0: "$0.000000000", 1: "$0.000000001", 5*Dollar + 3*Cent: "$5.030000000", -Cent: "-$0.010000000"} {
		if got := n.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", n, got, want)
		}
	}
}

func TestPriceOf(t *testing.T) {
	t.Parallel()
	opus := Defaults["claude-opus-5-5"]
	haiku := Defaults["claude-haiku-5-5"]
	tests := []struct {
		name  string
		price Price
		usage core.Usage
		want  Nanos
	}{
		// 1M input at $4 and 1M output at $20.
		{"opus million each", opus, core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, 24 * Dollar},
		// 1,000 input ($0.004), 500 output ($0.01), 2,000 cache reads
		// ($0.0004), 100 cache writes ($0.0005).
		{"opus mixed", opus, core.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 100}, 14_900_000},
		// A single Haiku input token is 100 nanodollars.
		{"haiku one token", haiku, core.Usage{InputTokens: 1}, 100},
		{"haiku at the threshold", haiku, core.Usage{InputTokens: 100_000}, 100_000 * 100},
		// Above 100K prompt tokens every token is at the long rate.
		{"haiku long", haiku, core.Usage{InputTokens: 60_000, CacheReadTokens: 50_000, OutputTokens: 10}, 60_000*500 + 50_000*50 + 10*2500},
		{"free", Free, core.Usage{InputTokens: 10, OutputTokens: 10}, 0},
	}
	for _, tt := range tests {
		if got := tt.price.Of(tt.usage); got != tt.want {
			t.Errorf("%s: Of = %d (%v), want %d (%v)", tt.name, got, got, tt.want, tt.want)
		}
	}
}

func TestEstimate(t *testing.T) {
	t.Parallel()
	req := &core.Request{
		System:   "be brief",                                                           // 8 chars
		Messages: []core.Message{{Role: core.RoleUser, Text: "what is two plus two?"}}, // 21 chars
	}
	// 2 messages (system counts) × 4 + ceil(29/3) = 8 + 10.
	if got := EstimateInputTokens(req); got != 18 {
		t.Errorf("EstimateInputTokens = %d, want 18", got)
	}
	if got := EstimateOutputTokens(req, 4096); got != 4096 {
		t.Errorf("fallback output = %d", got)
	}
	req.MaxTokens = 64
	if got := Estimate(req, 4096); got != (core.Usage{InputTokens: 18, OutputTokens: 64}) {
		t.Errorf("Estimate = %+v", got)
	}
	// Multi-byte text counts characters, not bytes.
	if got := EstimateInputTokens(&core.Request{Messages: []core.Message{{Text: "日本語"}}}); got != 5 {
		t.Errorf("EstimateInputTokens(日本語) = %d, want 5", got)
	}
}

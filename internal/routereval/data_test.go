package routereval

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestSampleMMLUStratifies(t *testing.T) {
	t.Parallel()
	var rows []mmluRaw
	for s := range 7 {
		for i := range 20 + s {
			rows = append(rows, mmluRaw{Question: fmt.Sprint(s, i), Subject: fmt.Sprintf("s%d", s), Choices: []string{"a", "b", "c", "d"}, Answer: i % 4})
		}
	}
	got := SampleMMLU(rows, 30)
	if len(got) != 30 {
		t.Fatalf("drew %d, want 30", len(got))
	}
	per := map[string]int{}
	ids := map[string]bool{}
	for _, q := range got {
		per[q.Subject]++
		if ids[q.ID] {
			t.Errorf("duplicate id %s", q.ID)
		}
		ids[q.ID] = true
		if q.Answer < "A" || q.Answer > "D" {
			t.Errorf("answer %q", q.Answer)
		}
	}
	for s, n := range per {
		if n != 4 && n != 5 {
			t.Errorf("subject %s drew %d, want 4 or 5 (30 over 7 subjects)", s, n)
		}
	}
	if again := SampleMMLU(rows, 30); !slices.EqualFunc(got, again, func(a, b Question) bool { return a.ID == b.ID }) {
		t.Error("the draw is not deterministic")
	}
}

func TestSampleGSM8K(t *testing.T) {
	t.Parallel()
	rows := make([]gsmRaw, 50)
	for i := range rows {
		rows[i] = gsmRaw{Question: fmt.Sprint("q", i), Answer: fmt.Sprintf("work\n#### 1,%03d", i)}
	}
	got := SampleGSM8K(rows, 10)
	if len(got) != 10 || !slices.IsSortedFunc(got, func(a, b Question) int { return strings.Compare(a.ID, b.ID) }) {
		t.Fatalf("draw %v: want 10 in file order", got)
	}
	for _, q := range got {
		if len(q.Answer) != 4 || q.Answer[0] != '1' {
			t.Errorf("answer %q: want the final number without its separator", q.Answer)
		}
	}
}

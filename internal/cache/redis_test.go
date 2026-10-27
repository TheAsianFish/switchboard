package cache

import (
	"testing"
)

func TestParseSearchBothProtocols(t *testing.T) {
	t.Parallel()
	r := &RedisIndex{keyPrefix: "sb:cache:sem:m:"}
	tests := []struct {
		name  string
		reply any
	}{
		{"RESP2", []any{int64(2),
			"sb:cache:sem:m:a", []any{"__dist", "0.25", "payload", "pa"},
			"sb:cache:sem:m:b", []any{"__dist", "0.5", "payload", "pb"},
			"sb:cache:sem:m:gone", nil,
		}},
		{"RESP3", map[any]any{
			"total_results": int64(2),
			"format":        "STRING",
			"results": []any{
				map[any]any{"id": "sb:cache:sem:m:a", "extra_attributes": map[any]any{"__dist": "0.25", "payload": "pa"}, "values": []any{}},
				map[any]any{"id": "sb:cache:sem:m:b", "extra_attributes": map[any]any{"__dist": 0.5, "payload": "pb"}, "values": []any{}},
				map[any]any{"id": "sb:cache:sem:m:gone", "extra_attributes": map[any]any{}},
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ms, err := r.parseSearch(tt.reply)
			if err != nil {
				t.Fatal(err)
			}
			if len(ms) != 2 || ms[0].ID != "a" || ms[0].Similarity != 0.75 || string(ms[0].Payload) != "pa" ||
				ms[1].ID != "b" || ms[1].Similarity != 0.5 || string(ms[1].Payload) != "pb" {
				t.Fatalf("got %+v", ms)
			}
		})
	}
}

func TestTagValueIsInjectiveAndSafe(t *testing.T) {
	t.Parallel()
	values := []string{"", "a", "A", "abc", "ABC", "a,b", "a b", "~61", "7e61", "0f", "{}", "*", "é"}
	seen := map[string]string{}
	for _, v := range values {
		tv := tagValue(v)
		if prev, ok := seen[tv]; ok {
			t.Fatalf("tagValue(%q) = tagValue(%q) = %q", v, prev, tv)
		}
		seen[tv] = v
		for i := 0; i < len(tv); i++ {
			if c := tv[i]; !isLowerAlnum(c) && c != '~' {
				t.Fatalf("tagValue(%q) = %q has %q", v, tv, c)
			}
		}
	}
}

func TestEscapeTag(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"abc_09", "abc_09"},
		{"~7e", `\~7e`},
		{"a b,c}", `a\ b\,c\}`},
	}
	for _, tt := range tests {
		if got := escapeTag(tt.in); got != tt.want {
			t.Errorf("escapeTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

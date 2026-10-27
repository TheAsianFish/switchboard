package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/embed"
)

// ollama serves /api/embed with reply, after checking the request.
func ollama(t *testing.T, status int, reply string, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/embed" {
			t.Errorf("request %s %s, want POST /api/embed", r.Method, r.URL.Path)
		}
		var req struct {
			Model     string   `json:"model"`
			Input     []string `json:"input"`
			Truncate  *bool    `json:"truncate"`
			KeepAlive string   `json:"keep_alive"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "all-minilm" || len(req.Input) == 0 || req.Truncate == nil || *req.Truncate || req.KeepAlive == "" {
			t.Errorf("body %+v, %v", req, err)
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOllama(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		reply   string
		delay   time.Duration
		inputs  []string
		want    [][]float32
		wantErr string
	}{
		{"renormalises", 200, `{"model":"all-minilm","embeddings":[[3,4],[0,2]]}`, 0, []string{"a", "b"}, [][]float32{{0.6, 0.8}, {0, 1}}, ""},
		{"one vector short", 200, `{"embeddings":[[1,0]]}`, 0, []string{"a", "b"}, nil, "1 vectors for 2 inputs"},
		{"ragged vectors", 200, `{"embeddings":[[1,0],[1,0,0]]}`, 0, []string{"a", "b"}, nil, "vector 1 has 3 dimensions"},
		{"model missing", 404, `{"error":"model \"all-minilm\" not found, try pulling it first"}`, 0, []string{"a"}, nil, "returned 404"},
		{"longer than the context", 400, `{"error":"the input length exceeds the context length"}`, 0, []string{"a"}, nil, "longer than the model's context"},
		{"too slow", 200, `{"embeddings":[[1]]}`, time.Second, []string{"a"}, nil, "deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := ollama(t, tt.status, tt.reply, tt.delay)
			o := &embed.Ollama{BaseURL: srv.URL + "/", Name: "all-minilm", Timeout: 100 * time.Millisecond}
			got, err := o.Embed(context.Background(), tt.inputs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				if errors.Is(err, embed.ErrTooLong) != (tt.status == 400) {
					t.Errorf("errors.Is(err, ErrTooLong) = %v", errors.Is(err, embed.ErrTooLong))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for i := range tt.want {
				for j := range tt.want[i] {
					if math.Abs(float64(got[i][j]-tt.want[i][j])) > 1e-6 {
						t.Fatalf("vectors = %v, want %v", got, tt.want)
					}
				}
			}
		})
	}
	if v, err := (&embed.Ollama{Name: "x"}).Embed(context.Background(), nil); v != nil || err != nil {
		t.Errorf("no inputs: %v %v", v, err)
	}
}

func TestFake(t *testing.T) {
	t.Parallel()
	f := &embed.Fake{Dims: 32}
	v, err := f.Embed(context.Background(), []string{"the cat sat", "Sat the CAT!", "dogs bark loudly"})
	if err != nil {
		t.Fatal(err)
	}
	if c := embed.Cosine(v[0], v[1]); math.Abs(c-1) > 1e-6 {
		t.Errorf("same words, other order and case: cosine %v, want 1", c)
	}
	if c := embed.Cosine(v[0], v[2]); c > 0.5 {
		t.Errorf("no shared words: cosine %v", c)
	}
	if calls, texts := f.Calls(); calls != 1 || texts != 3 {
		t.Errorf("calls %d texts %d", calls, texts)
	}
	f.Err = errors.New("down")
	if _, err := f.Embed(context.Background(), []string{"x"}); err == nil {
		t.Error("Err not returned")
	}
}

func TestCosineAndNormalise(t *testing.T) {
	t.Parallel()
	if c := embed.Cosine([]float32{1, 0}, []float32{0, 1}); c != 0 {
		t.Errorf("orthogonal: %v", c)
	}
	if c := embed.Cosine([]float32{0, 0}, []float32{1, 1}); c != 0 {
		t.Errorf("zero vector: %v", c)
	}
	v := embed.Normalise([]float32{0, 0})
	if v[0] != 0 || v[1] != 0 {
		t.Errorf("zero vector normalised to %v", v)
	}
}

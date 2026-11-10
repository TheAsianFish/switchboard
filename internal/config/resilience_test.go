package config

import (
	"strings"
	"testing"
	"time"
)

func TestResilience(t *testing.T) {
	t.Parallel()
	const file = `
providers:
  - name: up
    type: fake
    resilience:
      retries: 0
      backoff_base: 50ms
      breaker: {failures: 3, cooldown: 5s}
      hedge: {enabled: true, quantile: 0.9, budget: 0.05}
models: [{name: m, provider: up}]
`
	cfg, err := Load(writeFile(t, file), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Providers[0].Resilience
	if r.Retries == nil || *r.Retries != 0 {
		t.Errorf("retries = %v, want an explicit 0", r.Retries)
	}
	if r.BackoffBase != 50*time.Millisecond || *r.Breaker.Failures != 3 || r.Breaker.Cooldown != 5*time.Second {
		t.Errorf("resilience = %+v", r)
	}
	if !r.Hedge.Enabled || r.Hedge.Quantile != 0.9 || *r.Hedge.Budget != 0.05 {
		t.Errorf("hedge = %+v", r.Hedge)
	}
}

func TestResilienceValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ yaml, want string }{
		{"retries: -1", "retries must be between 0 and 10"},
		{"hedge: {quantile: 1.5}", "hedge.quantile"},
		{"hedge: {budget: 2}", "hedge.budget"},
		{"hedge: {window: 10, min_samples: 20}", "min_samples exceeds"},
		{"backoff_base: 2s\n      backoff_max: 1s", "backoff_max is below"},
		{"breaker: {cooldown: -1s}", "must not be negative"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			file := "providers:\n  - name: up\n    type: fake\n    resilience:\n      " + tc.yaml + "\nmodels: [{name: m, provider: up}]\n"
			_, err := Load(writeFile(t, file), env(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

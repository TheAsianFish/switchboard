package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRouter(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeFile(t, `
providers: [{name: fake, type: fake}]
models: [{name: small, provider: fake}]
router: {enabled: true, weights: w.json, quality_target: 0.7}
keys: [{name: a, key: sk-a, models: [auto, small]}]
`), env(map[string]string{"SWITCHBOARD_ROUTER_EMBED_URL": "http://ollama:11434"}))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Router
	if !r.Enabled || r.Weights != "w.json" || r.QualityTarget == nil || *r.QualityTarget != 0.7 ||
		r.EmbedURL != "http://ollama:11434" || r.Timeout != time.Second {
		t.Errorf("router = %+v", r)
	}
	off, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if off.Router.Enabled || off.Router.QualityTarget != nil {
		t.Errorf("router is on by default: %+v", off.Router)
	}
}

func TestRouterErrors(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, yaml, want string }{
		{"auto in a key without the router", `
providers: [{name: fake, type: fake}]
models: [{name: small, provider: fake}]
keys: [{name: a, key: sk-a, models: [auto]}]`, `unknown model "auto"`},
		{"a model named auto", `
providers: [{name: fake, type: fake}]
models: [{name: auto, provider: fake}]
router: {enabled: true}`, "taken by the router"},
		{"target above 1", `
router: {quality_target: 1.5}`, "quality_target must be between 0 and 1"},
		{"no embed url", `
router: {enabled: true, embed_url: ""}`, "router.embed_url is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeFile(t, tt.yaml), env(nil))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

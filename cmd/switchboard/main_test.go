package main

import (
	"bytes"
	"context"
	"runtime/debug"
	"strings"
	"testing"
)

func TestRunUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{"no command", nil, 2, "", "usage: switchboard"},
		{"unknown command", []string{"frobnicate"}, 2, "", `unknown command "frobnicate"`},
		{"help", []string{"help"}, 0, "commands:", ""},
		{"eval without a subcommand", []string{"eval"}, 2, "", "usage: switchboard eval results"},
		{"serve with a stray argument", []string{"serve", "extra"}, 2, "", "unexpected arguments"},
		{"serve with a missing config", []string{"serve", "--config", "/nonexistent/switchboard.yaml"}, 1, "", "read config"},
		{"fake-provider with a bad flag", []string{"fake-provider", "--latency", "soon"}, 2, "", "invalid value"},
		{"fake-provider with a bad model interval", []string{"fake-provider", "--model-token-interval", "fake-budget"}, 2, "", "want model=duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := run(context.Background(), tt.args, &out, &errOut)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, errOut.String())
			}
			if !strings.Contains(out.String(), tt.wantOut) || !strings.Contains(errOut.String(), tt.wantErr) {
				t.Errorf("stdout %q stderr %q; want %q / %q", out.String(), errOut.String(), tt.wantOut, tt.wantErr)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	t.Parallel()
	vcs := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "vcs.time", Value: "2026-11-11T10:00:00Z"},
		},
	}
	tests := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		ok      bool
		want    string
	}{
		{"release build stamp wins", "v1.0.0", vcs, true, "v1.0.0"},
		{"go install module version", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0"}}, true, "v1.0.0"},
		{"local build with vcs", "", vcs, true, "(devel) (0123456789ab-dirty, 2026-11-11)"},
		{"no build info", "", nil, false, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := versionString(tt.stamped, tt.info, tt.ok); got != tt.want {
				t.Errorf("versionString = %q, want %q", got, tt.want)
			}
		})
	}
}

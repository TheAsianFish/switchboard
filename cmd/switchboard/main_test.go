package main

import (
	"bytes"
	"context"
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

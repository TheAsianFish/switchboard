package config

import (
	"strings"
	"testing"
)

func TestLoadTracing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		env  map[string]string
		want Tracing
	}{
		{"off by default", "", nil, Tracing{SampleRatio: 1, ServiceName: "switchboard"}},
		{"from the file", "tracing: {endpoint: http://jaeger:4318, sample_ratio: 0.25, service_name: gw}", nil,
			Tracing{Endpoint: "http://jaeger:4318", SampleRatio: 0.25, ServiceName: "gw"}},
		{"env overrides the file", "tracing: {endpoint: http://jaeger:4318, sample_ratio: 0.25}", map[string]string{
			"SWITCHBOARD_TRACING_ENDPOINT":     "https://collector.example:4318/custom/path",
			"SWITCHBOARD_TRACING_SAMPLE_RATIO": "0",
			"SWITCHBOARD_TRACING_SERVICE_NAME": "edge",
		}, Tracing{Endpoint: "https://collector.example:4318/custom/path", SampleRatio: 0, ServiceName: "edge"}},
		{"the standard OTel variable does not turn it on", "", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://jaeger:4318"},
			Tracing{SampleRatio: 1, ServiceName: "switchboard"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(writeFile(t, tt.body), env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Tracing != tt.want {
				t.Errorf("tracing = %+v, want %+v", cfg.Tracing, tt.want)
			}
		})
	}
}

func TestValidateTracing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body string
		env        map[string]string
		want       string
	}{
		{"no scheme", "tracing: {endpoint: 'jaeger:4318'}", nil, "tracing.endpoint"},
		{"grpc scheme", "tracing: {endpoint: 'grpc://jaeger:4317'}", nil, "tracing.endpoint"},
		{"no host", "tracing: {endpoint: 'http://'}", nil, "tracing.endpoint"},
		{"ratio above 1", "tracing: {sample_ratio: 2}", nil, "tracing.sample_ratio"},
		{"negative ratio", "tracing: {sample_ratio: -0.1}", nil, "tracing.sample_ratio"},
		{"empty service name", "tracing: {service_name: ''}", nil, "tracing.service_name"},
		{"ratio env not a number", "", map[string]string{"SWITCHBOARD_TRACING_SAMPLE_RATIO": "half"}, "SWITCHBOARD_TRACING_SAMPLE_RATIO"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeFile(t, tt.body), env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

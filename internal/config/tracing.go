package config

import (
	"fmt"
	"net/url"
	"strconv"
)

// Tracing configures OpenTelemetry span export. Only the switchboard keys
// below (and their SWITCHBOARD_TRACING_* overrides) turn tracing on;
// OTEL_EXPORTER_OTLP_ENDPOINT is not read, so an environment that happens
// to set it for another program does not start exporting from this one.
type Tracing struct {
	// Endpoint is the OTLP/HTTP collector's base URL, for example
	// http://localhost:4318 (Jaeger); /v1/traces is appended when the URL
	// has no path. Empty, the default, turns tracing off: no spans, no
	// X-Trace-Id header, incoming traceparent headers ignored.
	Endpoint string `yaml:"endpoint"`
	// SampleRatio is the share of traces exported, 0 to 1. A request that
	// carries a traceparent continues its caller's trace id, but the
	// caller's sampled flag is ignored: the ratio decides.
	SampleRatio float64 `yaml:"sample_ratio"`
	// ServiceName is the service.name the spans are reported under.
	ServiceName string `yaml:"service_name"`
}

// Tracing defaults.
const (
	DefaultSampleRatio = 1.0
	DefaultServiceName = "switchboard"
)

func defaultTracing() Tracing {
	return Tracing{SampleRatio: DefaultSampleRatio, ServiceName: DefaultServiceName}
}

func applyTracingEnv(t *Tracing, getenv func(string) string) error {
	if v := getenv("SWITCHBOARD_TRACING_ENDPOINT"); v != "" {
		t.Endpoint = v
	}
	if v := getenv("SWITCHBOARD_TRACING_SERVICE_NAME"); v != "" {
		t.ServiceName = v
	}
	if v := getenv("SWITCHBOARD_TRACING_SAMPLE_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("SWITCHBOARD_TRACING_SAMPLE_RATIO: %q is not a number", v)
		}
		t.SampleRatio = r
	}
	return nil
}

func (c *Config) validateTracing() []error {
	var errs []error
	t := c.Tracing
	if t.Endpoint != "" {
		u, err := url.Parse(t.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("tracing.endpoint %q: want an http(s) URL such as http://localhost:4318", t.Endpoint))
		}
	}
	if t.SampleRatio < 0 || t.SampleRatio > 1 {
		errs = append(errs, fmt.Errorf("tracing.sample_ratio must be in [0, 1], got %v", t.SampleRatio))
	}
	if t.ServiceName == "" {
		errs = append(errs, fmt.Errorf("tracing.service_name must not be empty"))
	}
	return errs
}

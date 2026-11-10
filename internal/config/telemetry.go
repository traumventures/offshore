package config

import (
	"fmt"
	"strings"
)

// Telemetry is the [telemetry] section of the client and the pool
// configuration. Tracing is off unless an exporter or endpoint is set here
// or by the standard OTEL_* environment variables (which win).
//
//	[telemetry]
//	endpoint = "https://otel.example.com:4318"   # OTLP/HTTP base URL
//	headers = { "x-api-key" = "env:OTEL_KEY" }   # secret references
//	sampling = 0.25                              # parent based ratio
//	machine_endpoint = "http://10.0.0.5:4318"    # pool: as seen by machines
//	events_retention_days = 30                   # pool: analysis event log
type Telemetry struct {
	// Exporter is "otlp", "console" or "none" ("" = otlp when an endpoint
	// is set, else none).
	Exporter string `toml:"exporter"`
	// Endpoint is the OTLP/HTTP base URL (spans go to Endpoint/v1/traces).
	Endpoint string `toml:"endpoint"`
	// Headers are OTLP request headers; values are references (env:,
	// file:, cmd:) since they usually carry credentials.
	Headers map[string]string `toml:"headers"`
	// Sampling is the trace id ratio of new traces (0..1, default 1);
	// downstream components follow the upstream decision.
	Sampling *float64 `toml:"sampling"`
	// ServiceName overrides the component's default service.name.
	ServiceName string `toml:"service_name"`
	// MachineEndpoint is the OTLP base URL forwarded to machines (pool
	// configuration; default: Endpoint).
	MachineEndpoint string `toml:"machine_endpoint"`
	// EventsRetentionDays bounds the analysis event log of the orchestrator
	// (pool configuration; default 30).
	EventsRetentionDays int `toml:"events_retention_days"`
}

// validate returns problems of the section.
func (t Telemetry) validate() []string {
	var errs []string
	switch strings.ToLower(t.Exporter) {
	case "", "otlp", "console", "none":
	default:
		errs = append(errs, fmt.Sprintf("telemetry.exporter: unsupported %q (otlp, console, none)", t.Exporter))
	}
	for _, ep := range []struct{ key, v string }{{"endpoint", t.Endpoint}, {"machine_endpoint", t.MachineEndpoint}} {
		if ep.v != "" && !strings.HasPrefix(ep.v, "http://") && !strings.HasPrefix(ep.v, "https://") {
			errs = append(errs, fmt.Sprintf("telemetry.%s: an http(s) URL is required", ep.key))
		}
	}
	for name, ref := range t.Headers {
		kind, _, _ := strings.Cut(ref, ":")
		if kind != "env" && kind != "file" && kind != "cmd" {
			errs = append(errs, fmt.Sprintf("telemetry.headers.%s: must be a reference (env:, file:, cmd:)", name))
		}
	}
	if t.Sampling != nil && (*t.Sampling < 0 || *t.Sampling > 1) {
		errs = append(errs, "telemetry.sampling: must be between 0 and 1")
	}
	if t.EventsRetentionDays < 0 {
		errs = append(errs, "telemetry.events_retention_days: must not be negative")
	}
	return errs
}

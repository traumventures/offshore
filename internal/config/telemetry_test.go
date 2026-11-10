package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelemetrySection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(`
[telemetry]
endpoint = "http://collector:4318"
headers = { "x-api-key" = "env:OTEL_KEY" }
sampling = 0.5
events_retention_days = 7

[pool]
id = "f"
`), 0o600)
	t.Setenv("CLOUDSOURCED_POOL_CONFIG", path)
	c, err := LoadClientFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Telemetry.Endpoint != "http://collector:4318" || c.Telemetry.Headers["x-api-key"] != "env:OTEL_KEY" || *c.Telemetry.Sampling != 0.5 {
		t.Fatalf("client telemetry %+v", c.Telemetry)
	}
	f, err := LoadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Telemetry.EventsRetentionDays != 7 {
		t.Fatalf("pool telemetry %+v", f.Telemetry)
	}
	for _, w := range append(c.Warnings, f.Warnings...) {
		if strings.Contains(w, "telemetry") {
			t.Fatalf("telemetry is shared by both roles: %s", w)
		}
	}
	bad := 1.5
	errs := Telemetry{Exporter: "zipkin", Endpoint: "collector:4318", Headers: map[string]string{"k": "literal-secret"}, Sampling: &bad, EventsRetentionDays: -1}.validate()
	if len(errs) != 5 {
		t.Fatalf("want 5 problems, got %v", errs)
	}
	for _, e := range errs {
		if strings.Contains(e, "literal-secret") {
			t.Fatalf("secret in error: %s", e)
		}
	}
}

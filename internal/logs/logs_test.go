package logs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestJSONFormat(t *testing.T) {
	o, err := Parse(env(map[string]string{EnvFormat: "JSON", EnvLevel: "warn"}))
	if err != nil || !o.JSON || o.Level != slog.LevelWarn {
		t.Fatalf("%+v %v", o, err)
	}
	var b bytes.Buffer
	l := o.New(&b)
	l.Info("dropped")
	l.Warn("request failed", "request_id", "abc", "path", "/v1/acquire")
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line (info filtered), got %q", b.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["level"] != "WARN" || rec["msg"] != "request failed" || rec["request_id"] != "abc" || rec["time"] == nil {
		t.Fatalf("record %v", rec)
	}
}

func TestTextDefaultAndDebug(t *testing.T) {
	o, err := Parse(env(nil))
	if err != nil || o.JSON || o.Level != slog.LevelInfo {
		t.Fatalf("%+v %v", o, err)
	}
	o, _ = Parse(env(map[string]string{"OFFSHORE_DEBUG": "1"}))
	if o.Level != slog.LevelDebug {
		t.Fatal("OFFSHORE_DEBUG should enable debug")
	}
	// An explicit level wins over the legacy switch.
	o, _ = Parse(env(map[string]string{"OFFSHORE_DEBUG": "1", EnvLevel: "error"}))
	if o.Level != slog.LevelError {
		t.Fatal("level should win")
	}
	var b bytes.Buffer
	o, _ = Parse(env(map[string]string{EnvFormat: "text"}))
	o.New(&b).Info("hello", "k", "v")
	if !strings.Contains(b.String(), "level=INFO msg=hello k=v") {
		t.Fatalf("text %q", b.String())
	}
}

func TestInvalid(t *testing.T) {
	o, err := Parse(env(map[string]string{EnvFormat: "xml", EnvLevel: "loud"}))
	if err == nil || o.JSON || o.Level != slog.LevelInfo {
		t.Fatalf("%+v %v", o, err)
	}
	if !strings.Contains(err.Error(), EnvFormat) || !strings.Contains(err.Error(), EnvLevel) {
		t.Fatal(err)
	}
}

func TestForward(t *testing.T) {
	t.Setenv(EnvFormat, "json")
	t.Setenv(EnvLevel, "")
	f := Forward()
	if f[EnvFormat] != "json" || len(f) != 1 {
		t.Fatal(f)
	}
}

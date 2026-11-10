package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRepo(t *testing.T, content string) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RepoFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadRepoDefaultsAndOverride(t *testing.T) {
	dir := writeRepo(t, `
version = 1
[environment]
image = "rust:1"
[tool.cargo]
eager = ["target/debug/app"]
`)
	os.WriteFile(filepath.Join(dir, LocalFileName), []byte("[resources]\ncpu = 16\n"), 0o644)
	r, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Resources.CPU != 16 || r.Resources.Memory != "4G" || r.Cache.Build.TTL != "24h" || r.Environment.Base != "image" {
		t.Fatalf("unexpected defaults/override: %+v", r)
	}
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	if root, err := FindRepo(sub); err != nil || root != dir {
		t.Fatalf("FindRepo = %q, %v", root, err)
	}
}

func TestValidateErrors(t *testing.T) {
	dir := writeRepo(t, `
version = 1
[environment]
base = "image"
[[source.rule]]
paths = ["x"]
mode = "sometimes"
[cache.build]
paths = ["../outside"]
ttl = "soon"
`)
	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"environment.image", "source.rule[0].mode", "cache.build.paths", "cache.build.ttl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	dir := writeRepo(t, "version = 1\n[environment]\nimage = \"x\"\nimagee = \"typo\"\n")
	if _, err := LoadRepo(dir); err == nil || !strings.Contains(err.Error(), "imagee") {
		t.Fatalf("typo not reported: %v", err)
	}
}

func TestParse(t *testing.T) {
	if d, _ := ParseDuration("7d"); d != 7*24*time.Hour {
		t.Fatal(d)
	}
	if d, _ := ParseDuration("90m"); d != 90*time.Minute {
		t.Fatal(d)
	}
	for in, want := range map[string]int64{"512M": 512 << 20, "24G": 24 << 30, "1GiB": 1 << 30, "100": 100} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v", in, got, err)
		}
	}
}

func TestOutputsAndEditorModes(t *testing.T) {
	r, err := LoadRepo(writeRepo(t, "version = 1\n[environment]\nimage = \"rust:1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outputs.Mode != OutputsEager || r.Editor.RustAnalyzer != RustAnalyzerLocal {
		t.Fatalf("defaults: %+v %+v", r.Outputs, r.Editor)
	}
	r, err = LoadRepo(writeRepo(t, "version = 1\n[environment]\nimage = \"rust:1\"\n[cache.build]\npaths = [\"target\"]\n[outputs]\nmode = \"fuse\"\n[editor]\nrust_analyzer = \"lsp-remote\"\n"))
	if err != nil || r.Outputs.Mode != OutputsFuse || r.Editor.RustAnalyzer != RustAnalyzerLSPRemote {
		t.Fatalf("fuse/lsp-remote: %v", err)
	}
	_, err = LoadRepo(writeRepo(t, "version = 1\n[environment]\nimage = \"rust:1\"\n[outputs]\nmode = \"fuse\"\n[editor]\nrust_analyzer = \"remote\"\n"))
	if err == nil || !strings.Contains(err.Error(), "needs [cache.build] paths") || !strings.Contains(err.Error(), "editor.rust_analyzer") {
		t.Fatalf("validation: %v", err)
	}
}

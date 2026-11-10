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

func TestCompilerCacheAndLinker(t *testing.T) {
	dir := writeRepo(t, `
version = 1
[environment]
image = "rust:1"
linker = "mold"
[cache.compiler]
tool = "sccache"
ttl = "12h"
incremental = false
`)
	r, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Cache.Compiler.Tool != CompilerCacheSccache || r.CompilerCacheTTL() != 12*time.Hour || r.Environment.Linker != LinkerMold {
		t.Fatalf("unexpected: %+v %+v", r.Cache.Compiler, r.Environment)
	}
	if r.Cache.Compiler.Incremental == nil || *r.Cache.Compiler.Incremental {
		t.Fatal("incremental = false not parsed")
	}
	for name, tc := range map[string]struct{ body, want string }{
		"unknown tool":        {"[cache.compiler]\ntool = \"ccache\"\n", "cache.compiler.tool"},
		"ttl too long":        {"[cache.compiler]\ntool = \"sccache\"\nttl = \"2d\"\n", "cache.compiler.ttl"},
		"ttl invalid":         {"[cache.compiler]\ntool = \"sccache\"\nttl = \"soon\"\n", "cache.compiler.ttl"},
		"ttl without tool":    {"[cache.compiler]\nttl = \"1h\"\n", "require tool"},
		"incremental only":    {"[cache.compiler]\nincremental = true\n", "require tool"},
		"unknown linker":      {"[environment]\nimage = \"x\"\nlinker = \"gold\"\n", "environment.linker"},
		"unknown cache field": {"[cache.compiler]\ntool = \"sccache\"\nsize = 1\n", "size"},
	} {
		body := "version = 1\n" + tc.body
		if !strings.Contains(tc.body, "[environment]") {
			body += "[environment]\nimage = \"x\"\n"
		}
		_, err := LoadRepo(writeRepo(t, body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error mentioning %q, got %v", name, tc.want, err)
		}
	}
	// lld and the defaults are accepted.
	if _, err := LoadRepo(writeRepo(t, "version = 1\n[environment]\nimage = \"x\"\nlinker = \"lld\"\n[cache.compiler]\ntool = \"sccache\"\n")); err != nil {
		t.Fatal(err)
	}
}

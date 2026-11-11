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
	if r.Resources.CPU != 16 || r.Resources.Memory != "4G" || len(r.Caches) != 0 || r.Environment.Base != "image" {
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
[cache.target]
paths = ["../outside"]
ttl = "soon"
`)
	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"environment.image", "source.rule[0].mode", "cache.target.paths", "cache.target.ttl"} {
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
	r, err = LoadRepo(writeRepo(t, "version = 1\n[environment]\nimage = \"rust:1\"\n[cache.target]\npaths = [\"target\"]\n[outputs]\nmode = \"fuse\"\n[editor]\nrust_analyzer = \"lsp-remote\"\n"))
	if err != nil || r.Outputs.Mode != OutputsFuse || r.Editor.RustAnalyzer != RustAnalyzerLSPRemote {
		t.Fatalf("fuse/lsp-remote: %v", err)
	}
	_, err = LoadRepo(writeRepo(t, "version = 1\n[environment]\nimage = \"rust:1\"\n[outputs]\nmode = \"fuse\"\n[editor]\nrust_analyzer = \"remote\"\n"))
	if err == nil || !strings.Contains(err.Error(), "needs a slot-scope cache") || !strings.Contains(err.Error(), "editor.rust_analyzer") {
		t.Fatalf("validation: %v", err)
	}
}

func TestCachesAndLinker(t *testing.T) {
	dir := writeRepo(t, `
version = 1
[environment]
image = "rust:1"
linker = "mold"
[cache.target]
paths = ["target"]
max_size = "100G"
prune = "rust"
[cache.registry]
scope = "machine"
paths = ["$CARGO_HOME/registry"]
ttl = "3d"
[cache.node]
scope = "machine"
paths = ["web/node_modules"]
digest = ["web/bun.lock"]
install = "cd web && bun install --frozen-lockfile"
[cache.sccache]
tool = "sccache"
ttl = "12h"
`)
	r, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Caches) != 4 || r.Caches[0].Name != "node" || r.Environment.Linker != LinkerMold {
		t.Fatalf("caches %+v", r.Caches)
	}
	sc, ok := r.GlobalCache(CacheToolSccache)
	if !ok || sc.Scope != ScopeGlobal || sc.TTLDuration() != 12*time.Hour {
		t.Fatalf("sccache %+v", sc)
	}
	tg := r.CachesByScope(ScopeSlot)[0]
	if tg.MaxBytes() != 100<<30 || tg.Prune != PruneRust || tg.TTL != "24h" {
		t.Fatalf("target %+v", tg)
	}
	if got := r.SlotPaths(); len(got) != 1 || got[0] != "target" {
		t.Fatalf("slot paths %v", got)
	}
	if got := r.DigestPaths(); len(got) != 1 || got[0] != "web/node_modules" {
		t.Fatalf("digest paths %v", got)
	}
	if slot, machine := r.CacheTTLs(); slot != 24*time.Hour || machine != 7*24*time.Hour {
		t.Fatalf("ttls %v %v", slot, machine)
	}
	if n := r.CachesByScope(ScopeMachine); n[0].Prune != PruneNone || n[1].Prune != PruneLRU {
		t.Fatalf("machine prune defaults %+v", n)
	}
	for name, tc := range map[string]struct{ body, want string }{
		"unknown tool":        {"[cache.c]\ntool = \"bazel\"\n", "cache.c.tool"},
		"ttl too long":        {"[cache.c]\ntool = \"sccache\"\nttl = \"2d\"\n", "cache.c.ttl"},
		"ttl invalid":         {"[cache.c]\npaths = [\"t\"]\nttl = \"soon\"\n", "cache.c.ttl"},
		"global no tool":      {"[cache.c]\nscope = \"global\"\n", "cache.c.tool"},
		"duplicate tool":      {"[cache.a]\ntool = \"go\"\n[cache.b]\ntool = \"go\"\n", "also used by"},
		"slot outside":        {"[cache.c]\npaths = [\"/abs\"]\n", "cache.c.paths"},
		"slot no paths":       {"[cache.c]\nscope = \"slot\"\n", "required for scope"},
		"slot duplicate":      {"[cache.a]\npaths = [\"t\"]\n[cache.b]\npaths = [\"t/\"]\n", "also in cache.a"},
		"digest no install":   {"[cache.c]\nscope = \"machine\"\npaths = [\"n\"]\ndigest = [\"l\"]\n", "install: required"},
		"install no digest":   {"[cache.c]\nscope = \"machine\"\npaths = [\"n\"]\ninstall = \"x\"\n", "requires digest"},
		"digest abs path":     {"[cache.c]\nscope = \"machine\"\npaths = [\"$HOME/n\"]\ndigest = [\"l\"]\ninstall = \"x\"\n", "digest caches"},
		"bad scope":           {"[cache.c]\nscope = \"cluster\"\npaths = [\"t\"]\n", "cache.c.scope"},
		"bad prune":           {"[cache.c]\npaths = [\"t\"]\nprune = \"often\"\n", "cache.c.prune"},
		"bad size":            {"[cache.c]\npaths = [\"t\"]\nmax_size = \"lots\"\n", "cache.c.max_size"},
		"bad name":            {"[cache.C]\npaths = [\"t\"]\n", "name must match"},
		"unknown linker":      {"[environment]\nimage = \"x\"\nlinker = \"gold\"\n", "environment.linker"},
		"unknown cache field": {"[cache.c]\ntool = \"sccache\"\nsize = 1\n", "size"},
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
}

func TestToolSplit(t *testing.T) {
	if got := (Tool{}).SplitSubcommands("cargo"); len(got) != 2 {
		t.Fatalf("cargo default %v", got)
	}
	if got := (Tool{}).SplitSubcommands("npm"); got != nil {
		t.Fatalf("npm default %v", got)
	}
	dir := t.TempDir()
	write := func(s string) error {
		if err := os.WriteFile(filepath.Join(dir, RepoFileName), []byte("version = 1\n[environment]\nimage = \"rust:1\"\n"+s), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadRepo(dir)
		return err
	}
	if err := write("[tool.cargo]\nsplit = []\n"); err != nil {
		t.Fatal(err)
	}
	if err := write("[tool.cargo]\nsplit = [\"nextest\"]\n"); err != nil {
		t.Fatal(err)
	}
	if err := write("[tool.cargo]\nsplit = [\"build\"]\n"); err == nil {
		t.Fatal("split = [build] accepted")
	}
	if err := write("[tool.npm]\nsplit = [\"test\"]\n"); err == nil {
		t.Fatal("tool.npm.split accepted")
	}
}

func TestLinkerValues(t *testing.T) {
	for l, ok := range map[string]bool{"": true, "mold": true, "lld": true, "wild": true, "gold": false} {
		dir := writeRepo(t, "version = 1\n[environment]\nimage = \"rust:1\"\nlinker = \""+l+"\"\n")
		_, err := LoadRepo(dir)
		if (err == nil) != ok {
			t.Errorf("linker %q: err %v", l, err)
		}
	}
	if LinkerArgs(LinkerMold) != "-fuse-ld=mold" || LinkerArgs(LinkerWild) != "-fuse-ld=bfd -B"+WildDir {
		t.Fatal("linker args")
	}
}

func TestExtraToolchainValues(t *testing.T) {
	for v, ok := range map[string]bool{`["headstart"]`: true, `[]`: true, `["nightly"]`: false} {
		dir := writeRepo(t, "version = 1\n[environment]\nimage = \"rust:1\"\n[environment.toolchains]\nextra = "+v+"\n")
		if _, err := LoadRepo(dir); (err == nil) != ok {
			t.Errorf("extra %s: err %v", v, err)
		}
	}
}

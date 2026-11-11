package adapter

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/platform"
)

var linux = &platform.Platform{OS: "linux", Arch: "amd64", Libc: platform.Glibc}

func classify(tool, args string, cfg config.Tool, env map[string]string) Plan {
	return Classify(Command{Tool: tool, Args: strings.Fields(args), Config: cfg, Host: linux,
		Getenv: func(k string) string { return env[k] }})
}

// phases renders a plan's phases without commands: "build@remote run@local".
func phases(p Plan) string {
	var out []string
	for _, ph := range p.Phases {
		out = append(out, string(ph.Name)+"@"+string(ph.Where))
	}
	return strings.Join(out, " ")
}

func TestRemoteRules(t *testing.T) {
	p := config.Tool{Local: []string{"publish", "login"}}
	cases := map[string]bool{"build": true, "+nightly build": true, "publish": false, "-v login": false, "": true}
	for args, want := range cases {
		if _, got := remoteRules(p, "cargo", subcommand(strings.Fields(args))); got != want {
			t.Errorf("remoteRules(%q) = %v, want %v", args, got, want)
		}
	}
	only := config.Tool{Remote: []string{"build", "test"}}
	why, ok := remoteRules(only, "cargo", "run")
	if _, ok2 := remoteRules(only, "cargo", "test"); ok || !ok2 {
		t.Error("remote allowlist not honoured")
	}
	if why != `subcommand "run" is not listed in [tool.cargo] remote` {
		t.Errorf("reason %q", why)
	}
}

func TestAdapterFor(t *testing.T) {
	for tool, want := range map[string]string{"cargo": "cargo", "go": "go", "bun": "node", "npm": "node", "pnpm": "node", "npx": "node", "make": "generic"} {
		if got := For(tool, config.Tool{}).Name(); got != want {
			t.Errorf("%s -> %s, want %s", tool, got, want)
		}
	}
	if got := For("cross", config.Tool{Adapter: "cargo"}).Name(); got != "cargo" {
		t.Errorf("explicit adapter ignored: %s", got)
	}
}

func TestClassifyCargo(t *testing.T) {
	cfg := config.Tool{Local: []string{"publish"}}
	cases := []struct{ args, want string }{
		{"build --release", "build@remote"},
		{"fetch", "deps@remote"},
		{"publish", "build@local"},
		{"test -p a", "build@remote run@local run@remote"}, // doc tests run remotely afterwards
		{"test --lib", "build@remote run@local"},
		{"t --tests foo -- --nocapture", "build@remote run@local"},
		{"test --no-run", "run@remote"},
		{"test --doc", "run@remote"},
		{"test --frobnicate", "run@remote"},
		{"nextest run -p a foo", "build@remote run@local"},
		{"nextest list", "run@remote"},
		{"nextest run --no-run", "run@remote"},
		{"nextest run --archive-file x.tar.zst", "run@remote"},
		{"check --tests", "build@remote"},
	}
	for _, c := range cases {
		p := classify("cargo", c.args, cfg, nil)
		if got := phases(p); got != c.want {
			t.Errorf("cargo %s: %s, want %s (%s)", c.args, got, c.want, p.Why)
		}
	}
	p := classify("cargo", "test --lib", cfg, nil)
	if !p.Split() || !reflect.DeepEqual(p.Phases[0].Argv, strings.Fields("cargo test --no-run --message-format=json-render-diagnostics --lib")) {
		t.Fatalf("build phase %v", p.Phases[0].Argv)
	}
	if !strings.Contains(p.Why, "split") {
		t.Errorf("why %q", p.Why)
	}
	// Switches.
	only := []string{"nextest"}
	if p := classify("cargo", "test --lib", config.Tool{Split: &only}, nil); p.Split() {
		t.Error("cargo test split although only nextest is enabled")
	}
	none := []string{}
	if p := classify("cargo", "nextest run", config.Tool{Split: &none}, nil); p.Split() {
		t.Error("split = [] still splits")
	}
	if p := classify("cargo", "test --lib", config.Tool{}, map[string]string{"OFFSHORE_SPLIT": "0"}); p.Split() {
		t.Error("OFFSHORE_SPLIT=0 still splits")
	}
	// Another OS: the tests run remotely with a reason.
	mac := &platform.Platform{OS: "darwin", Arch: "arm64"}
	p = Classify(Command{Tool: "cargo", Args: strings.Fields("test --lib"), Host: mac, Getenv: func(string) string { return "" }})
	if phases(p) != "run@remote" || !strings.Contains(p.Why, "darwin") {
		t.Fatalf("darwin: %s %q", phases(p), p.Why)
	}
}

func TestClassifyGo(t *testing.T) {
	cases := []struct{ args, want string }{
		{"build ./...", "build@remote"},
		{"vet ./...", "build@remote"},
		{"mod download", "deps@remote"},
		{"run ./cmd/x", "run@remote"},
		{"test ./...", "build@remote run@local"},
		{"test -race -run TestX -v ./pkg", "build@remote run@local"},
		{"test -json ./...", "run@remote"},
		{"test -coverprofile=c.out ./...", "run@remote"},
		{"test -weird ./...", "run@remote"},
	}
	for _, c := range cases {
		p := classify("go", c.args, config.Tool{}, nil)
		if got := phases(p); got != c.want {
			t.Errorf("go %s: %s, want %s (%s)", c.args, got, c.want, p.Why)
		}
	}
	none := []string{}
	if p := classify("go", "test ./...", config.Tool{Split: &none}, nil); p.Split() {
		t.Error("split = [] still splits go test")
	}
}

func TestClassifyNode(t *testing.T) {
	local := config.Tool{Local: []string{"run dev"}}
	cases := []struct {
		tool, args, want string
	}{
		{"bun", "install", "deps@remote"},
		{"npm", "ci", "deps@remote"},
		{"pnpm", "i", "deps@remote"},
		{"yarn", "", "deps@remote"},
		{"npm", "test", "run@remote"},
		{"bun", "run build", "run@remote"},
		{"npm", "run dev", "run@local"},
		{"npx", "tsc -p .", "run@remote"},
	}
	for _, c := range cases {
		p := classify(c.tool, c.args, local, nil)
		if got := phases(p); got != c.want {
			t.Errorf("%s %s: %s, want %s (%s)", c.tool, c.args, got, c.want, p.Why)
		}
	}
	if p := classify("npm", "test", config.Tool{Local: []string{"test"}}, nil); phases(p) != "run@local" {
		t.Errorf("local test: %s", phases(p))
	}
}

func TestClassifyGeneric(t *testing.T) {
	if p := classify("make", "-j8 all", config.Tool{}, nil); phases(p) != "run@remote" || p.Adapter != "generic" {
		t.Fatalf("%+v", p)
	}
	if p := classify("make", "install", config.Tool{Local: []string{"install"}}, nil); phases(p) != "run@local" {
		t.Fatalf("%+v", p)
	}
}

func TestDetectAndCaches(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"Cargo.toml", "go.mod", "package.json", "pnpm-lock.yaml"} {
		os.WriteFile(filepath.Join(root, f), nil, 0o644)
	}
	var found []string
	for _, a := range All {
		if a.Detect(root) {
			found = append(found, a.Name())
		}
	}
	if strings.Join(found, ",") != "cargo,go,node" {
		t.Fatalf("detected %v", found)
	}
	nc := Node{}.DefaultCaches(root)
	if len(nc) != 1 || nc[0].Install != "pnpm install --frozen-lockfile" || !reflect.DeepEqual(nc[0].Digest, []string{"package.json", "pnpm-lock.yaml"}) || nc[0].Scope != ScopeMachine {
		t.Fatalf("node caches %+v", nc)
	}
	gc := Go{}.DefaultCaches(root)
	if gc[0].Scope != ScopeGlobal || gc[0].Tool != config.CacheToolGo || gc[1].Scope != ScopeMachine {
		t.Fatalf("go caches %+v", gc)
	}
	if cc := (Cargo{}).DefaultCaches(root); cc[0].Name != "target" || cc[0].Scope != ScopeSlot {
		t.Fatalf("cargo caches %+v", cc)
	}
}

func TestRealBinarySkipsShims(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shims, real := t.TempDir(), t.TempDir()
	os.Symlink(self, filepath.Join(shims, "mytool"))
	os.WriteFile(filepath.Join(real, "mytool"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+real)
	got, err := RealBinary("mytool")
	if err != nil || got != filepath.Join(real, "mytool") {
		t.Fatalf("got %s %v", got, err)
	}
}

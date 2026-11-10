package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/proto"
)

func TestRemoteAllowed(t *testing.T) {
	p := config.Tool{Local: []string{"publish", "login"}}
	cases := map[string]bool{
		"build":          true,
		"+nightly build": true,
		"publish":        false,
		"-v login":       false,
		"":               true,
	}
	for args, want := range cases {
		var a []string
		if args != "" {
			a = splitArgs(args)
		}
		if got := remoteAllowed(p, a); got != want {
			t.Errorf("remoteAllowed(%q) = %v, want %v", args, got, want)
		}
	}
	only := config.Tool{Remote: []string{"build", "test"}}
	if remoteAllowed(only, []string{"run"}) || !remoteAllowed(only, []string{"test"}) {
		t.Error("remote allowlist not honoured")
	}
}

func splitArgs(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

func TestSafeJoin(t *testing.T) {
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "", "."} {
		if _, err := safeJoin("/repo", bad); err == nil {
			t.Errorf("safeJoin accepted %q", bad)
		}
	}
	if p, err := safeJoin("/repo", "src/main.rs"); err != nil || p != "/repo/src/main.rs" {
		t.Errorf("got %q, %v", p, err)
	}
}

func TestRealBinarySkipsShims(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	real := t.TempDir()
	os.Symlink(self, filepath.Join(shims, "mytool"))
	os.WriteFile(filepath.Join(real, "mytool"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+real)
	got, err := RealBinary("mytool")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(real, "mytool") {
		t.Fatalf("got %s", got)
	}
}

func TestAllowedEnv(t *testing.T) {
	t.Setenv("RUSTFLAGS", "-Copt")
	t.Setenv("CARGO_TERM_COLOR", "always")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "nope")
	env := allowedEnv([]string{"RUSTFLAGS", "CARGO_*"})
	if env["RUSTFLAGS"] != "-Copt" || env["CARGO_TERM_COLOR"] != "always" {
		t.Fatalf("missing allowed variables: %v", env)
	}
	if _, ok := env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Fatal("secret leaked")
	}
	if _, ok := env["PATH"]; ok {
		t.Fatal("PATH must come from the remote environment")
	}
}

func TestFormatCompilerCache(t *testing.T) {
	got := FormatCompilerCache(&proto.CompilerCacheStats{Tool: "sccache", Requests: 5, Hits: 3, Misses: 1, NotCacheable: 1})
	if got != "sccache: 3 hits / 1 misses (5 requests, 1 not cacheable)" {
		t.Fatal(got)
	}
	if got := FormatCompilerCache(&proto.CompilerCacheStats{Tool: "sccache", Errors: 2}); got != "sccache: 0 hits / 0 misses (0 requests, 2 errors)" {
		t.Fatal(got)
	}
}

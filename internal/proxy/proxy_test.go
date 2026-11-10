package proxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/traumventures/cloudsourced/internal/config"
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
		if _, got := remoteAllowed(p, "cargo", a); got != want {
			t.Errorf("remoteAllowed(%q) = %v, want %v", args, got, want)
		}
	}
	only := config.Tool{Remote: []string{"build", "test"}}
	why, ok := remoteAllowed(only, "cargo", []string{"run"})
	if _, ok2 := remoteAllowed(only, "cargo", []string{"test"}); ok || !ok2 {
		t.Error("remote allowlist not honoured")
	}
	if why != `subcommand "run" is not listed in [tool.cargo] remote` {
		t.Errorf("reason %q", why)
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
	got := FormatCompilerCache(map[string]int64{"sccache_requests": 5, "sccache_hits": 3, "sccache_misses": 1, "sccache_not_cacheable": 1})
	if got != "sccache: 3 hits / 1 misses (5 requests, 1 not cacheable)" {
		t.Fatal(got)
	}
	if got := FormatCompilerCache(map[string]int64{"sccache_requests": 0, "sccache_errors": 2}); got != "sccache: 0 hits / 0 misses (0 requests, 2 errors)" {
		t.Fatal(got)
	}
	if got := FormatCompilerCache(nil); got != "" {
		t.Fatal(got)
	}
}

func TestPortsFromEnvAndConfig(t *testing.T) {
	prof := config.Tool{Ports: []any{int64(8080)}}
	t.Setenv("CLOUDSOURCED_PORTS", "")
	ps, err := ports(prof)
	if err != nil || len(ps) != 1 || ps[0] != (config.Port{Local: 8080, Remote: 8080}) {
		t.Fatalf("config ports: %v %v", ps, err)
	}
	t.Setenv("CLOUDSOURCED_PORTS", "3000:3001")
	ps, err = ports(prof)
	if err != nil || len(ps) != 1 || ps[0] != (config.Port{Local: 3000, Remote: 3001}) {
		t.Fatalf("env ports: %v %v", ps, err)
	}
	t.Setenv("CLOUDSOURCED_PORTS", "x")
	if _, err := ports(prof); err == nil {
		t.Fatal("invalid env accepted")
	}
}

func TestBusyLocalPortIsSkipped(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	fport := free.Addr().(*net.TCPAddr).Port
	free.Close()
	f := listenPorts([]config.Port{{Local: port, Remote: 1}, {Local: fport, Remote: 2}})
	defer f.close()
	if got := f.forwarded(); len(got) != 1 || got[0] != fmt.Sprintf("%d:2", fport) {
		t.Fatalf("forwarded %v", got)
	}
}

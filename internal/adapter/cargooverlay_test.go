package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/platform"
)

func TestCargoHomeRustflags(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "config.toml"), []byte(`
[build]
rustflags = "-C debuginfo=1"
rustc-wrapper = "/usr/local/bin/sccache-user"
[target.x86_64-unknown-linux-gnu]
rustflags = ["-C", "link-arg=-fuse-ld=mold"]
[target.aarch64-unknown-linux-gnu]
rustflags = ["-C", "link-arg=-fuse-ld=mold"]
`), 0o644)
	host := "x86_64-unknown-linux-gnu"
	key := "CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUSTFLAGS"
	got := cargoHomeRustflags(d, host, map[string]string{})
	if got["CARGO_BUILD_RUSTFLAGS"] != "-C debuginfo=1" {
		t.Fatalf("build: %v", got)
	}
	if got[key] != "-C link-arg=-fuse-ld=mold" || len(got) != 2 {
		t.Fatalf("target: %v", got)
	}
	if got := cargoHomeRustflags(d, host, map[string]string{"RUSTFLAGS": "-x"}); len(got) != 0 {
		t.Fatalf("RUSTFLAGS overrides config: %v", got)
	}
	if got := cargoHomeRustflags(d, host, map[string]string{key: "-y"}); got[key] != "" {
		t.Fatalf("forwarded variable wins: %v", got)
	}
	if got := cargoHomeRustflags(t.TempDir(), host, nil); len(got) != 0 {
		t.Fatalf("no config: %v", got)
	}
}

// Remote cargo plans carry the alignment (homes, rustflags); other tools'
// plans none.
func TestCargoPlanAlignsOutputs(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".cargo"), 0o755)
	os.WriteFile(filepath.Join(home, ".cargo", "config.toml"), []byte("[target.aarch64-unknown-linux-gnu]\nrustflags = \"-C x\"\n"), 0o644)
	env := map[string]string{"HOME": home, "RUSTUP_HOME": "/opt/rustup"}
	arm := platform.Platform{OS: "linux", Arch: "arm64"}
	for _, args := range [][]string{{"build"}, {"test"}, {"nextest", "run"}} {
		p := Classify(Command{Tool: "cargo", Args: args, Config: config.Tool{}, Getenv: func(k string) string { return env[k] }, Host: &arm})
		a, ok := p.AlignOutputs(map[string]string{})
		if !ok || a.CargoHome != filepath.Join(home, ".cargo") || a.RustupHome != "/opt/rustup" ||
			a.Env["CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_RUSTFLAGS"] != "-C x" {
			t.Fatalf("cargo %v: %+v %v", args, a, ok)
		}
	}
	if _, ok := Classify(Command{Tool: "go", Args: []string{"build"}, Config: config.Tool{}}).AlignOutputs(nil); ok {
		t.Fatal("go plans need no cargo alignment")
	}
}

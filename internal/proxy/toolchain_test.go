package proxy

import (
	"os"
	"path/filepath"
	"testing"
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
	got := cargoHomeRustflags(d, map[string]string{})
	if got["CARGO_BUILD_RUSTFLAGS"] != "-C debuginfo=1" {
		t.Fatalf("build: %v", got)
	}
	key := "CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUSTFLAGS"
	if hostTriple() == "aarch64-unknown-linux-gnu" {
		key = "CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_RUSTFLAGS"
	}
	if got[key] != "-C link-arg=-fuse-ld=mold" || len(got) != 2 {
		t.Fatalf("target: %v", got)
	}
	if got := cargoHomeRustflags(d, map[string]string{"RUSTFLAGS": "-x"}); len(got) != 0 {
		t.Fatalf("RUSTFLAGS overrides config: %v", got)
	}
	if got := cargoHomeRustflags(d, map[string]string{key: "-y"}); got[key] != "" {
		t.Fatalf("forwarded variable wins: %v", got)
	}
	if got := cargoHomeRustflags(t.TempDir(), nil); len(got) != 0 {
		t.Fatalf("no config: %v", got)
	}
}

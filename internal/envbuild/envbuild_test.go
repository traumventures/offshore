package envbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traumventures/cloudsourced/internal/config"
)

func repo(t *testing.T, cfg string, files map[string]string) *config.Repo {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, config.RepoFileName), []byte(cfg), 0o644)
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644)
	}
	r, err := config.LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const cfg = `
version = 1
[environment]
image = "rust:1-slim-bookworm"
[environment.packages]
apt = ["pkg-config", "clang"]
[environment.toolchains]
rust = "rust-toolchain.toml"
[[environment.setup]]
run = "cargo install --locked cargo-nextest"
[[environment.setup]]
run = "echo deps"
inputs = ["deps.lock"]
`

func TestResolveHashTracksInputs(t *testing.T) {
	files := map[string]string{"rust-toolchain.toml": "[toolchain]\nchannel = \"1.85\"\n", "deps.lock": "a"}
	r1 := repo(t, cfg, files)
	p1, err := Resolve(r1)
	if err != nil {
		t.Fatal(err)
	}
	r2 := repo(t, cfg, files)
	p2, _ := Resolve(r2)
	if p1.Hash != p2.Hash {
		t.Fatal("identical configurations must share one environment")
	}
	files["deps.lock"] = "b"
	p3, _ := Resolve(repo(t, cfg, files))
	if p3.Hash == p1.Hash {
		t.Fatal("setup input change did not change the hash")
	}
	files["deps.lock"] = "a"
	files["rust-toolchain.toml"] = "[toolchain]\nchannel = \"1.86\"\n"
	p4, _ := Resolve(repo(t, cfg, files))
	if p4.Hash == p1.Hash {
		t.Fatal("toolchain change did not change the hash")
	}
	df := p1.Dockerfile
	for _, want := range []string{"FROM rust:1-slim-bookworm", "apt-get install -y --no-install-recommends clang pkg-config", "rustup toolchain install", "COPY setup/1/", "cargo install --locked cargo-nextest"} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, df)
		}
	}
	// Package order does not matter for the generated layer.
	if strings.Index(df, "nextest") > strings.Index(df, "echo deps") {
		t.Error("setup steps out of order")
	}
}

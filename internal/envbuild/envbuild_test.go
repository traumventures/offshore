package envbuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
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

func TestCompilerCacheAndLinkerTools(t *testing.T) {
	base := "version = 1\n[environment]\nimage = \"rust:1-slim-bookworm\"\n"
	p0, err := Resolve(repo(t, base, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p0.Dockerfile, "sccache") || strings.Contains(p0.Dockerfile, "mold") || len(p0.tools) != 0 {
		t.Fatalf("tools installed without configuration:\n%s", p0.Dockerfile)
	}
	p1, err := Resolve(repo(t, base+"linker = \"mold\"\n[cache.compiler]\ntool = \"sccache\"\n", nil))
	if err != nil {
		t.Fatal(err)
	}
	if p1.Hash == p0.Hash {
		t.Fatal("enabling sccache and mold did not change the environment hash")
	}
	for _, want := range []string{
		"COPY tools/sccache /usr/local/bin/sccache",
		"COPY tools/mold /usr/local/bin/mold",
		"ln -sf mold /usr/local/bin/ld.mold",
		`exec cc "$@" -fuse-ld=mold`,
		"/usr/local/bin/cloudsourced-cc-mold",
	} {
		if !strings.Contains(p1.Dockerfile, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, p1.Dockerfile)
		}
	}
	if p1.tools["tools/sccache"].SHA256 != toolReleases["sccache"]["x86_64"].SHA256 {
		t.Fatal("pinned sccache release not used")
	}
	// The compiler cache TTL and incremental setting do not change the image.
	p2, _ := Resolve(repo(t, base+"linker = \"mold\"\n[cache.compiler]\ntool = \"sccache\"\nttl = \"1h\"\nincremental = false\n", nil))
	if p2.Hash != p1.Hash {
		t.Fatal("session-only compiler cache settings changed the environment hash")
	}
	p3, _ := Resolve(repo(t, base+"linker = \"lld\"\n", nil))
	if !strings.Contains(p3.Dockerfile, "--no-install-recommends lld") || !strings.Contains(p3.Dockerfile, "-fuse-ld=lld") || len(p3.tools) != 0 {
		t.Fatalf("lld not installed:\n%s", p3.Dockerfile)
	}
	pa, _ := Resolve(repo(t, base+"[resources]\narch = \"aarch64\"\n[cache.compiler]\ntool = \"sccache\"\n", nil))
	if !strings.Contains(pa.tools["tools/sccache"].URL, "aarch64-unknown-linux-musl") {
		t.Fatalf("wrong arch: %s", pa.tools["tools/sccache"].URL)
	}
}

func TestToolDownloadVerifiesChecksum(t *testing.T) {
	// A local archive with a known checksum stands in for the release.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho fake\n")
	tw.WriteHeader(&tar.Header{Name: "pkg/bin/fake", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(buf.Bytes()) }))
	defer srv.Close()
	t.Setenv("CLOUDSOURCED_TOOL_CACHE", t.TempDir())
	good := tool{Name: "fake", Version: "1", URL: srv.URL + "/fake.tar.gz", SHA256: hex.EncodeToString(sum[:]), Member: "pkg/bin/fake"}
	dst := filepath.Join(t.TempDir(), "fake")
	if err := good.extract(dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("extracted binary: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != string(body) {
		t.Fatalf("content %q", b)
	}
	// Cached: works without the server.
	srv.Close()
	if err := good.extract(dst); err != nil {
		t.Fatalf("cached archive not reused: %v", err)
	}
	bad := good
	bad.SHA256 = strings.Repeat("0", 64)
	bad.URL = "http://127.0.0.1:1/fake.tar.gz"
	if err := bad.extract(dst); err == nil {
		t.Fatal("unverified archive accepted")
	}
}

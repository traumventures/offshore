package blobcache

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pinned sccache release used by the integration test (same as envbuild).
const (
	testSccacheVersion = "0.19.0"
	testSccacheAMD64   = "aa73c7270c997894038c0d8f0d297865f57569e1687883322bc431713d4bb9dd"
	testSccacheARM64   = "3e47fc4a71b1ddd1b4294a0de9d34498922fdf41e3cd0bda5df3954e3d6deb9d"
)

// sccacheBinary returns a sccache binary for the integration test.
// CLOUDSOURCED_TEST_SCCACHE=download fetches the pinned release (checksum
// verified) into a temporary directory; any other value is a binary path.
func sccacheBinary(t *testing.T) string {
	v := os.Getenv("CLOUDSOURCED_TEST_SCCACHE")
	if v == "" {
		t.Skip("set CLOUDSOURCED_TEST_SCCACHE=download (or a sccache path) to run")
	}
	if v != "download" {
		return v
	}
	arch, sum := "x86_64", testSccacheAMD64
	if runtime.GOARCH == "arm64" {
		arch, sum = "aarch64", testSccacheARM64
	}
	name := fmt.Sprintf("sccache-v%s-%s-unknown-linux-musl", testSccacheVersion, arch)
	url := fmt.Sprintf("https://github.com/mozilla/sccache/releases/download/v%s/%s.tar.gz", testSccacheVersion, name)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("download %s: %v %s", url, err, resp.Status)
	}
	if h := sha256.Sum256(data); hex.EncodeToString(h[:]) != sum {
		t.Fatalf("checksum mismatch for %s", url)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("sccache not in archive: %v", err)
		}
		if h.Name == name+"/sccache" {
			out := filepath.Join(t.TempDir(), "sccache")
			b, _ := io.ReadAll(tr)
			if err := os.WriteFile(out, b, 0o755); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
}

type methodLog struct {
	mu   sync.Mutex
	seen map[string]int
}

func (m *methodLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.seen[r.Method]++
		m.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

type sccacheStats struct {
	Stats struct {
		CacheHits   struct{ Counts map[string]int64 } `json:"cache_hits"`
		CacheMisses struct{ Counts map[string]int64 } `json:"cache_misses"`
	} `json:"stats"`
}

func sum(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

// TestSccacheIntegration compiles a crate twice through a real sccache with
// fresh local state: the second compilation must be a cache hit served by
// the blobcache over WebDAV.
func TestSccacheIntegration(t *testing.T) {
	bin := sccacheBinary(t)
	rustc, err := exec.LookPath("rustc")
	if err != nil {
		t.Skip("rustc not installed")
	}
	store, err := NewDirStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ml := &methodLog{seen: map[string]int{}}
	h := &Handler{Store: store, Namespace: "envhash", Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: ml.wrap(h)}
	go srv.Serve(ln)
	defer srv.Close()

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "lib.rs"), []byte("pub fn answer() -> u32 { 42 }\n"), 0o644)
	compile := func(round int) sccacheStats {
		state := t.TempDir()
		env := append(os.Environ(),
			"SCCACHE_WEBDAV_ENDPOINT=http://"+ln.Addr().String()+"/",
			"SCCACHE_WEBDAV_KEY_PREFIX=_ttl/3600",
			"SCCACHE_DIR="+state,
			"SCCACHE_SERVER_UDS="+filepath.Join(state, "s.sock"),
			"SCCACHE_IDLE_TIMEOUT=0",
		)
		run := func(args ...string) []byte {
			cmd := exec.Command(bin, args...)
			cmd.Env = env
			cmd.Dir = src
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("round %d: sccache %v: %v\n%s", round, args, err, out)
			}
			return out
		}
		outDir := filepath.Join(t.TempDir(), "out")
		os.MkdirAll(outDir, 0o755)
		run(rustc, "--crate-name", "answer", "--edition=2021", "--crate-type", "lib", "--emit=dep-info,metadata,link", "--out-dir", outDir, "lib.rs")
		if _, err := os.Stat(filepath.Join(state, "s.sock")); err != nil {
			t.Fatalf("round %d: sccache server not on the unix socket: %v", round, err)
		}
		var st sccacheStats
		raw := run("--show-stats", "--stats-format=json")
		t.Logf("round %d stats: %s", round, raw)
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		run("--stop-server")
		return st
	}
	first := compile(1)
	second := compile(2)
	t.Logf("first: hits %v misses %v; second: hits %v misses %v; methods %v",
		first.Stats.CacheHits.Counts, first.Stats.CacheMisses.Counts, second.Stats.CacheHits.Counts, second.Stats.CacheMisses.Counts, ml.seen)
	if sum(first.Stats.CacheMisses.Counts) != 1 || sum(first.Stats.CacheHits.Counts) != 0 {
		t.Errorf("first round: want 1 miss, 0 hits")
	}
	if sum(second.Stats.CacheHits.Counts) != 1 {
		t.Errorf("second round: want 1 hit")
	}
	if ml.seen[http.MethodPut] == 0 || ml.seen[http.MethodGet] == 0 {
		t.Errorf("expected PUT and GET requests, got %v", ml.seen)
	}
}

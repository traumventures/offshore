package server

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traumventures/cloudsourced/internal/proto"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestSessionEnvCompilerCache(t *testing.T) {
	s := &Server{env: []string{"PATH=/usr/bin", "RUSTFLAGS=-Dwarnings"}, ccEndpoint: "http://127.0.0.1:1234/"}
	off := false
	req := proto.Exec{
		Cwd: "/r", Env: map[string]string{"FOO": "1"},
		CompilerCache: &proto.CompilerCache{Tool: "sccache", TTLSeconds: 3600, Incremental: &off},
		Linker:        "mold",
	}
	m := envMap(s.sessionEnv(req, "abc"))
	want := map[string]string{
		"RUSTC_WRAPPER":                            "sccache",
		"SCCACHE_WEBDAV_ENDPOINT":                  "http://127.0.0.1:1234/",
		"SCCACHE_WEBDAV_KEY_PREFIX":                "_ttl/3600",
		"SCCACHE_SERVER_UDS":                       sccacheDir("abc") + "/server.sock",
		"SCCACHE_DIR":                              sccacheDir("abc"),
		"SCCACHE_IDLE_TIMEOUT":                     "0",
		"CARGO_INCREMENTAL":                        "0",
		"CARGO_TARGET_" + hostTriple() + "_LINKER": "/usr/local/bin/cloudsourced-cc-mold",
		"RUSTFLAGS":                                "-Dwarnings", // untouched
		"FOO":                                      "1",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	// User settings win; incremental stays cargo's default when unset.
	req.Env = map[string]string{"RUSTC_WRAPPER": "/my/wrapper", "CARGO_TARGET_" + hostTriple() + "_LINKER": "clang"}
	req.CompilerCache.Incremental = nil
	req.CompilerCache.TTLSeconds = 0
	m = envMap(s.sessionEnv(req, "abc"))
	if m["RUSTC_WRAPPER"] != "/my/wrapper" || m["CARGO_TARGET_"+hostTriple()+"_LINKER"] != "clang" {
		t.Errorf("user settings overridden: %v", m)
	}
	if _, ok := m["CARGO_INCREMENTAL"]; ok {
		t.Error("CARGO_INCREMENTAL forced without incremental = false")
	}
	if _, ok := m["SCCACHE_WEBDAV_KEY_PREFIX"]; ok {
		t.Error("key prefix without ttl")
	}
	// Without the setting nothing is added.
	m = envMap(s.sessionEnv(proto.Exec{Cwd: "/r"}, "abc"))
	for k := range m {
		if strings.HasPrefix(k, "SCCACHE_") || k == "RUSTC_WRAPPER" || strings.HasPrefix(k, "CARGO_TARGET_") {
			t.Errorf("unexpected %s", k)
		}
	}
	// Without an endpoint the cache is off.
	s.ccEndpoint = ""
	m = envMap(s.sessionEnv(proto.Exec{Cwd: "/r", CompilerCache: &proto.CompilerCache{Tool: "sccache"}}, "abc"))
	if _, ok := m["RUSTC_WRAPPER"]; ok {
		t.Error("wrapper set without endpoint")
	}
}

func TestParseSccacheStats(t *testing.T) {
	raw := `{"stats":{"compile_requests":5,"requests_not_cacheable":1,"requests_executed":4,
	"cache_errors":{"counts":{"Rust":1},"adv_counts":{}},"cache_hits":{"counts":{"Rust":2,"C/C++":1},"adv_counts":{}},
	"cache_misses":{"counts":{"Rust":1},"adv_counts":{}},"cache_read_errors":0,"cache_write_errors":1,
	"non_cacheable_compilations":0,"not_cached":{"incremental":1}},"version":"0.19.0"}`
	st, err := parseSccacheStats([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"sccache_requests": 5, "sccache_hits": 3, "sccache_misses": 1, "sccache_not_cacheable": 1, "sccache_errors": 2}
	if !maps.Equal(st, want) {
		t.Fatalf("got %v, want %v", st, want)
	}
}

func TestBuiltinCompilerCacheEndpoint(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: Config{EnvHash: "abc123", SccacheDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startCompilerCache(ctx)
	if !strings.HasPrefix(s.ccEndpoint, "http://127.0.0.1:") {
		t.Fatalf("endpoint %q", s.ccEndpoint)
	}
	req, _ := http.NewRequest("PUT", s.ccEndpoint+"a/b", strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}
	resp.Body.Close()
	// Stored under the environment hash.
	resp, err = http.Get(s.ccEndpoint + "a/b")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET: %v %v", resp, err)
	}
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(dir, "abc123", "a", "b")); err != nil {
		t.Fatalf("not stored under the environment hash: %v", err)
	}
	// External endpoint: namespace appended.
	s2 := &Server{cfg: Config{EnvHash: "abc123", SccacheEndpoint: "http://172.17.0.1:9000/cache/"}, log: s.log}
	s2.startCompilerCache(ctx)
	if s2.ccEndpoint != "http://172.17.0.1:9000/cache/abc123/" {
		t.Fatalf("external endpoint %q", s2.ccEndpoint)
	}
}

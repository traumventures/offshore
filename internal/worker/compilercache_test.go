package worker

import (
	"context"
	"fmt"
	"github.com/traumventures/cloudsourced/internal/store"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	s := &Worker{env: []string{"PATH=/usr/bin", "RUSTFLAGS=-Dwarnings"}, ccEndpoint: "http://127.0.0.1:1234/", self: "/bin/cs", cfg: Config{CacheDir: "/cache"}}
	req := proto.Exec{
		Cwd: "/r", Env: map[string]string{"FOO": "1"},
		Caches: []proto.Cache{
			{Name: "sc", Scope: "global", Tool: "sccache", TTLSeconds: 3600},
			{Name: "cc", Scope: "global", Tool: "ccache"},
			{Name: "go", Scope: "global", Tool: "go", TTLSeconds: 7200},
		},
		Linker: "mold",
	}
	m := envMap(s.sessionEnv(req, "abc"))
	want := map[string]string{
		"RUSTC_WRAPPER":                            "sccache",
		"SCCACHE_WEBDAV_ENDPOINT":                  "http://127.0.0.1:1234/",
		"SCCACHE_WEBDAV_KEY_PREFIX":                "_ttl/3600",
		"SCCACHE_SERVER_UDS":                       sccacheDir("abc") + "/server.sock",
		"SCCACHE_DIR":                              sccacheDir("abc"),
		"SCCACHE_IDLE_TIMEOUT":                     "0",
		"CCACHE_REMOTE_STORAGE":                    "http://127.0.0.1:1234/ccache|layout=subdirs",
		"CCACHE_DIR":                               "/cache/ccache",
		"GOCACHEPROG":                              "/bin/cs gocacheprog /cache/gocache http://127.0.0.1:1234/_ttl/7200/",
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
	req.Caches[0].TTLSeconds = 0
	m = envMap(s.sessionEnv(req, "abc"))
	if m["RUSTC_WRAPPER"] != "/my/wrapper" || m["CARGO_TARGET_"+hostTriple()+"_LINKER"] != "clang" {
		t.Errorf("user settings overridden: %v", m)
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
	m = envMap(s.sessionEnv(proto.Exec{Cwd: "/r", Caches: []proto.Cache{{Scope: "global", Tool: "sccache"}}}, "abc"))
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

func TestSccachePendingWrites(t *testing.T) {
	stats := func(misses, writes, writeErrors int) string {
		return fmt.Sprintf(`{"stats":{"cache_misses":{"counts":{"Rust":%d}},"cache_writes":%d,"cache_write_errors":%d}}`, misses, writes, writeErrors)
	}
	for _, c := range []struct {
		raw  string
		want int64
	}{
		{stats(3, 0, 0), 3}, // compiled, nothing stored yet
		{stats(3, 2, 0), 1}, // one upload in flight
		{stats(3, 2, 1), 0}, // a failed write is finished too
		{`{"stats":{}}`, 0}, // no compilation at all
	} {
		_, pending, err := parseSccacheJSON([]byte(c.raw))
		if err != nil || pending != c.want {
			t.Errorf("%s: pending %d, %v; want %d", c.raw, pending, err, c.want)
		}
	}
}

func TestBuiltinCompilerCacheEndpoint(t *testing.T) {
	dir := t.TempDir()
	s := &Worker{cfg: Config{EnvHash: "abc123", SccacheDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	// A store grant for the environment backs the cache: objects are
	// uploaded, and a machine with an empty local tier reads them.
	storeDir := t.TempDir()
	g, _ := store.NewGranter(ctx, store.Config{Name: "local", Dir: storeDir, Prefix: "cache"})
	gr, _ := g.Grant(ctx, []string{store.RepoScope("r"), store.EnvScope("abc123")}, time.Hour)
	s.noteGrant("r", &gr)
	req, _ = http.NewRequest("PUT", s.ccEndpoint+"c/d", strings.NewReader("y"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 201 {
		t.Fatalf("PUT: %v %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "cache", "envs", "abc123", "abc123", "c", "d")); err != nil {
		t.Fatalf("not uploaded to the store: %v", err)
	}
	s2 := &Worker{cfg: Config{EnvHash: "abc123", SccacheDir: t.TempDir()}, log: s.log}
	s2.startCompilerCache(ctx)
	s2.noteGrant("r", &gr)
	resp, err = http.Get(s2.ccEndpoint + "c/d")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET from the store: %v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "y" {
		t.Fatalf("content %q", b)
	}
	if got, ok := s2.grants.get("r", time.Minute); !ok || got.Store != "local" {
		t.Fatal("grant not remembered")
	}
}

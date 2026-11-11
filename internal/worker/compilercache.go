package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/proto"
)

// Result caches (global [cache.<name>] tables: sccache, ccache, go).
//
// The worker serves a blobcache WebDAV endpoint on 127.0.0.1 (sessions
// share the machine's network namespace). Its namespace is fixed to the
// machine's environment hash, so toolchains never mix and sessions cannot
// reach other environments. Objects live in a tiered store: a directory
// on the machine disk (24 h TTL), backed by the pool's store under the
// environment's prefix once a session brings a store grant for it, so a
// fresh machine reuses what earlier machines compiled.
//
// Every session runs its own sccache server on a unix socket in a private
// directory, so concurrent sessions do not share (or kill) each other's
// server and the server's statistics are exactly the session's.

// startCompilerCache starts the endpoint sessions use.
func (s *Worker) startCompilerCache(ctx context.Context) {
	ns := s.cfg.EnvHash
	if ns == "" {
		ns = "default"
	}
	local, err := blobcache.NewDirStore(s.cfg.SccacheDir, blobcache.MaxTTL)
	if err != nil {
		s.log.Warn("compiler cache disabled", "err", err)
		return
	}
	if n := local.Sweep(); n > 0 {
		s.log.Info("removed expired compiler cache entries", "count", n)
	}
	s.rcStore = &tieredStore{local: local, log: s.log}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.log.Warn("compiler cache disabled", "err", err)
		return
	}
	srv := &http.Server{
		Handler:           &blobcache.Handler{Store: s.rcStore, Namespace: ns, Log: s.log},
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("compiler cache server", "err", err)
		}
	}()
	go local.SweepEvery(ctx, 30*time.Minute)
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	s.ccEndpoint = "http://" + ln.Addr().String() + "/"
	s.log.Info("compiler cache", "endpoint", s.ccEndpoint, "dir", s.cfg.SccacheDir)
}

// sessionTmp is a session's temporary directory ($TMPDIR), removed when
// the session ends. It lies below the cache directory: temporary files
// (rustc, sccache, linkers) then live on the cache medium, which the disk
// watermarks and the prune account for, not on the container's root file
// system.
func (s *Worker) sessionTmp(session string) string {
	return filepath.Join(s.cfg.CacheDir, "tmp", session)
}

// sccacheDir is the private directory of a session's sccache server.
func (s *Worker) sccacheDir(session string) string {
	return filepath.Join(s.sessionTmp(session), "sccache")
}

// compilerCacheEnv returns the session variables for the compiler cache
// and the linker. has reports whether a variable is already set (by the
// image or the request) and must not be overridden.
func (s *Worker) compilerCacheEnv(req proto.Exec, session string, has func(string) bool) [][2]string {
	var out [][2]string
	if cc := req.GlobalCache(config.CacheToolSccache); cc != nil && s.ccEndpoint != "" {
		dir := s.sccacheDir(session)
		if !has("RUSTC_WRAPPER") {
			out = append(out, [2]string{"RUSTC_WRAPPER", "sccache"})
		}
		out = append(out,
			[2]string{"SCCACHE_WEBDAV_ENDPOINT", s.ccEndpoint},
			[2]string{"SCCACHE_DIR", dir},
			[2]string{"SCCACHE_SERVER_UDS", filepath.Join(dir, "server.sock")},
			[2]string{"SCCACHE_IDLE_TIMEOUT", "0"}, // lives as long as the session
			// A cache that fails must never fail the build: sccache
			// compiles locally when its server or storage errors.
			[2]string{"SCCACHE_IGNORE_SERVER_IO_ERROR", "1"},
		)
		if p := ttlPrefix(cc); p != "" {
			out = append(out, [2]string{"SCCACHE_WEBDAV_KEY_PREFIX", strings.TrimSuffix(p, "/")})
		}
	}
	if cc := req.GlobalCache(config.CacheToolCcache); cc != nil && s.ccEndpoint != "" {
		// ccache's http remote storage on the same endpoint; the local
		// ccache directory is the machine's.
		if !has("CCACHE_REMOTE_STORAGE") {
			out = append(out, [2]string{"CCACHE_REMOTE_STORAGE", s.ccEndpoint + ttlPrefix(cc) + "ccache|layout=subdirs"})
		}
		if !has("CCACHE_DIR") {
			out = append(out, [2]string{"CCACHE_DIR", filepath.Join(s.cfg.CacheDir, "ccache")})
		}
	}
	if cc := req.GlobalCache(config.CacheToolGo); cc != nil && s.ccEndpoint != "" && s.self != "" && !has("GOCACHEPROG") {
		// offshore gocacheprog: outputs on the machine's disk, shared
		// through the endpoint.
		out = append(out, [2]string{"GOCACHEPROG", fmt.Sprintf("%s gocacheprog %s %s", s.self, filepath.Join(s.cfg.CacheDir, "gocache"), s.ccEndpoint+ttlPrefix(cc))})
	}
	if l := req.Linker; l != "" && req.Toolchain == nil {
		// With a relocated toolchain the linker comes from a "cc" on PATH
		// (linkerShim): cargo hashes this variable into fingerprints.
		// A linker driver instead of RUSTFLAGS: it leaves RUSTFLAGS and
		// rustflags from .cargo/config.toml untouched.
		key := "CARGO_TARGET_" + hostTriple() + "_LINKER"
		if !has(key) {
			out = append(out, [2]string{key, config.LinkerDriver(l)})
		}
	}
	return out
}

func hostTriple() string {
	if runtime.GOARCH == "arm64" {
		return "AARCH64_UNKNOWN_LINUX_GNU"
	}
	return "X86_64_UNKNOWN_LINUX_GNU"
}

// sccacheJSON is the subset of `sccache --show-stats --stats-format=json`.
type sccacheJSON struct {
	Stats struct {
		CompileRequests       int64        `json:"compile_requests"`
		RequestsNotCacheable  int64        `json:"requests_not_cacheable"`
		CacheHits             sccacheCount `json:"cache_hits"`
		CacheMisses           sccacheCount `json:"cache_misses"`
		CacheErrors           sccacheCount `json:"cache_errors"`
		CacheReadErrors       int64        `json:"cache_read_errors"`
		CacheWriteErrors      int64        `json:"cache_write_errors"`
		CacheWrites           int64        `json:"cache_writes"`
		NonCacheableCompiling int64        `json:"non_cacheable_compilations"`
	} `json:"stats"`
}

type sccacheCount struct {
	Counts map[string]int64 `json:"counts"`
}

func (c sccacheCount) total() (n int64) {
	for _, v := range c.Counts {
		n += v
	}
	return n
}

// parseSccacheStats maps the statistics to proto.Exit.Cache counters.
func parseSccacheStats(b []byte) (map[string]int64, error) {
	st, _, err := parseSccacheJSON(b)
	return st, err
}

// parseSccacheJSON returns the counters and the number of cache writes
// still in flight (every miss is followed by exactly one write, counted
// as a write or a write error once it finished).
func parseSccacheJSON(b []byte) (map[string]int64, int64, error) {
	var j sccacheJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, 0, err
	}
	st := j.Stats
	pending := st.CacheMisses.total() - st.CacheWrites - st.CacheWriteErrors
	return map[string]int64{
		"sccache_requests":      st.CompileRequests,
		"sccache_hits":          st.CacheHits.total(),
		"sccache_misses":        st.CacheMisses.total(),
		"sccache_not_cacheable": st.RequestsNotCacheable + st.NonCacheableCompiling,
		"sccache_errors":        st.CacheErrors.total() + st.CacheReadErrors + st.CacheWriteErrors,
	}, pending, nil
}

// sccacheStats reads the statistics of the session's sccache server and
// stops it. It returns nil if the session never started one.
func (s *Worker) sccacheStats(env []string, session string) map[string]int64 {
	if !s.sccacheStarted(session) {
		return nil
	}
	st, err := awaitSccacheWrites(env, sccacheWriteWait)
	if err != nil {
		s.log.Warn("compiler cache statistics", "session", session, "err", err)
	}
	runSccache(env, "--stop-server")
	return st
}

func (s *Worker) sccacheStarted(session string) bool {
	_, err := os.Stat(filepath.Join(s.sccacheDir(session), "server.sock"))
	return err == nil
}

// stopSccache stops the session's sccache server (if any). Without
// cgroups it might otherwise outlive the session.
func (s *Worker) stopSccache(env []string, session string) {
	if s.sccacheStarted(session) {
		awaitSccacheWrites(env, sccacheWriteWait)
		runSccache(env, "--stop-server")
	}
}

// sccacheWriteWait bounds the wait for pending cache writes at session end.
const sccacheWriteWait = 30 * time.Second

// awaitSccacheWrites polls the statistics of the session's sccache server
// until every cache miss has been written (or failed to). sccache answers
// a compilation before it stores the result; the session's processes are
// killed when it ends, so without this wait a slow upload (a loaded host,
// a remote cache) is lost and the next machine misses. It returns the last
// statistics.
func awaitSccacheWrites(env []string, max time.Duration) (map[string]int64, error) {
	deadline := time.Now().Add(max)
	for {
		out, err := runSccache(env, "--show-stats", "--stats-format=json")
		if err != nil {
			return nil, err
		}
		st, pending, err := parseSccacheJSON(out)
		if err != nil || pending <= 0 || time.Now().After(deadline) {
			return st, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runSccache(env []string, args ...string) ([]byte, error) {
	bin := "sccache"
	for _, kv := range env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, d := range filepath.SplitList(p) {
				if fi, err := os.Stat(filepath.Join(d, "sccache")); err == nil && fi.Mode().IsRegular() {
					bin = filepath.Join(d, "sccache")
					break
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = "/"
	return cmd.Output()
}

// ttlPrefix is the endpoint path prefix that lowers the maximum age of
// reused entries to the cache's TTL ("" for the store's own limit).
func ttlPrefix(c *proto.Cache) string {
	if c.TTLSeconds <= 0 || c.TTLSeconds >= int64(blobcache.MaxTTL/time.Second) {
		return ""
	}
	return fmt.Sprintf("_ttl/%d/", c.TTLSeconds)
}

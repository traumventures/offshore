package worker

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/sandbox"
)

func TestDigestCacheInstallOnceAndLink(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "web"), 0o755)
	os.WriteFile(filepath.Join(repo, "web", "bun.lock"), []byte("v1"), 0o644)
	cache := t.TempDir()
	sl, _ := newSlots(filepath.Join(cache, "build"))
	w := &Worker{cfg: Config{CacheDir: cache, DepsDir: filepath.Join(cache, "deps")}, slots: sl}
	installs := 0
	defer func(f func(string, sandbox.Spec, io.Writer) error) { runInstallCmd = f }(runInstallCmd)
	runInstallCmd = func(_ string, spec sandbox.Spec, out io.Writer) error {
		installs++
		if spec.Cwd != repo || spec.Argv[2] != "bun install" {
			t.Errorf("spec %+v", spec)
		}
		lock, _ := os.ReadFile(filepath.Join(repo, "web", "bun.lock"))
		for _, b := range spec.Binds {
			if b.Dst == filepath.Join(repo, "web", "node_modules") {
				os.WriteFile(filepath.Join(b.Src, "pkg.js"), lock, 0o644)
			}
		}
		return nil
	}
	c := proto.Cache{Name: "node", Scope: "machine", Paths: []string{"web/node_modules"}, Digest: []string{"web/*.lock"}, Install: "bun install"}
	req := proto.Exec{Repo: repo, RepoID: "r", Caches: []proto.Cache{c}}
	lease := func() string {
		slot, _, err := sl.lease("r", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return slot
	}
	var out bytes.Buffer
	check := func(slot, want string) {
		t.Helper()
		binds, err := w.cacheBinds(req, slot, nil, nil, &out)
		if err != nil {
			t.Fatal(err)
		}
		if len(binds) != 1 || binds[0].Dst != filepath.Join(repo, "web", "node_modules") {
			t.Fatalf("binds %+v", binds)
		}
		if b, _ := os.ReadFile(filepath.Join(binds[0].Src, "pkg.js")); string(b) != want {
			t.Fatalf("linked content %q, want %q", b, want)
		}
	}
	s1, s2 := lease(), lease()
	check(s1, "v1")
	check(s2, "v1") // second slot: linked from the store, no install
	if installs != 1 || !strings.Contains(out.String(), "cache node: installing") {
		t.Fatalf("installs %d, output %q", installs, out.String())
	}
	os.WriteFile(filepath.Join(repo, "web", "bun.lock"), []byte("v2"), 0o644)
	check(s1, "v2")
	if installs != 2 {
		t.Fatalf("lockfile change: installs %d", installs)
	}
	// Both digests stay until unused for their TTL.
	if n := w.gcDigests(time.Now()); n != 0 {
		t.Fatalf("gc removed %d fresh digests", n)
	}
	if n := w.gcDigests(time.Now().Add(8 * 24 * time.Hour)); n != 2 {
		t.Fatalf("gc removed %d expired digests, want 2", n)
	}
	// No digest file: an error, not an empty cache.
	os.Remove(filepath.Join(repo, "web", "bun.lock"))
	if _, err := w.cacheBinds(req, s1, nil, nil, &out); err == nil {
		t.Fatal("missing digest files accepted")
	}
}

func TestCacheDigestInputs(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "a.lock"), []byte("x"), 0o644)
	c := proto.Cache{Name: "n", Paths: []string{"nm"}, Digest: []string{"a.lock"}, Install: "i"}
	d1, _ := cacheDigest(repo, c, "linux/amd64/glibc")
	d2, _ := cacheDigest(repo, c, "linux/arm64/glibc")
	c.Install = "j"
	d3, _ := cacheDigest(repo, c, "linux/amd64/glibc")
	if d1 == "" || d1 == d2 || d1 == d3 {
		t.Fatalf("digest ignores platform or install: %s %s %s", d1, d2, d3)
	}
}

func TestPruneLRU(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, name := range []string{"old", "mid", "new"} {
		write(t, filepath.Join(dir, name), 64<<10, now.Add(time.Duration(i-3)*time.Hour))
	}
	if freed := pruneLRU(dir, 140<<10); freed < 64<<10 {
		t.Fatalf("freed %d", freed)
	}
	if exists(filepath.Join(dir, "old")) || !exists(filepath.Join(dir, "new")) || !exists(filepath.Join(dir, "mid")) {
		t.Fatal("not the least recently used file removed")
	}
	if pruneLRU(dir, 1<<30) != 0 {
		t.Fatal("pruned below budget")
	}
}

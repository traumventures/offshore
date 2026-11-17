package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/manifest"
)

// Idle roots and roots of removed worktrees are evicted; roots in recent
// use or with a machine attached stay. A removed worktree's sync stream
// is stopped.
func TestEvictRoots(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{log: log, roots: map[string]*root{}, conns: map[string]*machineConn{}}
	d.env = newEnvWatch(d)
	dir := t.TempDir()
	now := time.Now()
	add := func(name string, lastUse time.Time) *root {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Join(p, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(p, "src", "f"), []byte("x"), 0o644)
		r, err := d.rootFor(p, manifest.Rules{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.close)
		r.lastUse = lastUse
		return r
	}
	idle := add("idle", now.Add(-rootIdleTTL-time.Minute))
	recent := add("recent", now.Add(-time.Minute))
	busy := add("busy", now.Add(-rootIdleTTL-time.Minute))
	gone := add("gone", now)

	mc := &machineConn{d: d, log: log, roots: map[string]*rootSync{}}
	d.conns["m"] = mc
	attach := func(r *root) *rootSync {
		rs := &rootSync{mc: mc, root: r, wake: make(chan struct{}, 1), ackCh: make(chan struct{})}
		mc.roots[r.path] = rs
		r.subscribe(rs)
		return rs
	}
	busySync, goneSync := attach(busy), attach(gone)
	if err := os.RemoveAll(gone.path); err != nil {
		t.Fatal(err)
	}

	got := d.evictRoots(now)
	slices.Sort(got)
	if want := []string{gone.path, idle.path}; !slices.Equal(got, want) {
		t.Fatalf("evicted %v, want %v", got, want)
	}
	for _, r := range []*root{recent, busy} {
		if d.rootByPath(r.path) != r {
			t.Errorf("%s evicted", r.path)
		}
	}
	for _, r := range []*root{idle, gone} {
		if d.rootByPath(r.path) != nil || !r.closed {
			t.Errorf("%s still tracked or open", r.path)
		}
	}
	if _, ok := mc.roots[gone.path]; ok || !goneSync.stopped || gone.attached() {
		t.Error("sync stream of the removed worktree not stopped")
	}
	if _, ok := mc.roots[busy.path]; !ok || busySync.stopped {
		t.Error("sync stream of a live root stopped")
	}

	// An evicted root is tracked again on its next use.
	r, err := d.rootFor(idle.path, manifest.Rules{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if r == idle {
		t.Error("evicted root reused")
	}
	if _, ok := r.entry("src/f"); !ok {
		t.Error("rescanned root lacks src/f")
	}
}

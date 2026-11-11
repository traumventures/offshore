package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/store"
)

// fsWorker is a worker with a build slot file system in the given mode
// whose repository "repo" uses the directory store storeDir.
func fsWorker(t *testing.T, mode fsMode, storeDir string, opt cacheFSOptions) (*Worker, *store.Grant) {
	t.Helper()
	if mode != fsPlain {
		if _, err := os.Stat("/dev/fuse"); err != nil {
			t.Skip("no FUSE")
		}
	}
	if mode == fsKernel && os.Geteuid() != 0 {
		t.Skip("kernel overlay needs root")
	}
	cache := t.TempDir()
	sl, err := newSlots(filepath.Join(cache, "build"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opt.Mode, opt.Log = &mode, log
	cfs, err := newCacheFS(filepath.Join(cache, "anyfs"), opt)
	if err != nil {
		t.Fatal(err)
	}
	sl.remove = cfs.removeSlot
	t.Cleanup(cfs.unmountAll)
	w := &Worker{cfg: Config{CacheDir: cache, DepsDir: filepath.Join(cache, "deps"), BuildTTL: 24 * time.Hour}, slots: sl, cfs: cfs, log: log}
	g, err := store.NewGranter(context.Background(), store.Config{Name: "local", Dir: storeDir, Prefix: "cache"})
	if err != nil {
		t.Fatal(err)
	}
	gr, err := g.Grant(context.Background(), []string{store.RepoScope("repo")}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return w, &gr
}

// runSession leases a slot like a session, runs f on its data directory and
// releases it.
func runSession(t *testing.T, w *Worker, f func(data string)) (slot string, warm bool) {
	t.Helper()
	slot, _, err := w.slots.lease("repo", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	warm, err = w.cfs.prepare(context.Background(), w.slots, slot, "repo", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f(filepath.Join(slot, "data"))
	time.Sleep(1100 * time.Millisecond) // last_used has second resolution
	w.slots.release(slot)
	return slot, warm
}

func put(t *testing.T, p string, b []byte, mt time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mt, mt)
}

func ref(t *testing.T, w *Worker, slot string) slotRef {
	m, err := readMeta(slot)
	if err != nil {
		t.Fatal(err)
	}
	return slotRef{Repo: "repo", Path: slot, Meta: m}
}

func TestSlotCommitFlushRestore(t *testing.T) {
	for _, mode := range []fsMode{fsPlain, fsUser, fsKernel} {
		t.Run(mode.String(), func(t *testing.T) {
			testSlotCommitFlushRestore(t, mode)
		})
	}
}

func testSlotCommitFlushRestore(t *testing.T, mode fsMode) {
	ctx := context.Background()
	storeDir := t.TempDir()
	a, gr := fsWorker(t, mode, storeDir, cacheFSOptions{})
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	big := []byte(strings.Repeat("rlib", 1<<19)) // 2 MiB: pages of its own
	slot, warm := runSession(t, a, func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/libx.rlib"), big, mt)
		put(t, filepath.Join(data, "target/debug/.fingerprint/x/lib"), []byte("fp"), mt)
		put(t, filepath.Join(data, "target/debug/old"), []byte("old"), mt)
		os.Symlink("deps/libx.rlib", filepath.Join(data, "target/debug/link"))
	})
	if warm {
		t.Fatal("first slot warm")
	}
	if got := a.changedRepos(); len(got) != 1 {
		t.Fatalf("changed %v", got)
	}
	// Without a store grant the save fails.
	if _, err := a.saveSlot(ctx, "repo", nil); err == nil {
		t.Fatal("save without grant")
	}
	res, err := a.saveSlot(ctx, "repo", gr)
	if err != nil || res.Head == nil || res.Head.Layer == "" || res.Pages == 0 || len(res.Head.Chain) != 1 || res.Head.Store != "local" {
		t.Fatalf("save: %+v %v", res, err)
	}
	if got := a.changedRepos(); len(got) != 0 {
		t.Fatalf("changed after save %v", got)
	}
	if h := a.cfs.flushedHeads(); len(h) != 1 || h[0].Layer != res.Head.Layer {
		t.Fatalf("heads %+v", h)
	}
	// The next session reuses the slot warm; its changes are a layer on
	// top (deleted files are whiteouts).
	slot2, warm := runSession(t, a, func(data string) {
		if b, err := os.ReadFile(filepath.Join(data, "target/debug/old")); err != nil || string(b) != "old" {
			t.Fatalf("warm slot lost data: %v", err)
		}
		os.Remove(filepath.Join(data, "target/debug/old"))
		put(t, filepath.Join(data, "target/debug/new"), []byte("new"), mt)
	})
	if slot2 != slot || !warm {
		t.Fatalf("slot %s warm %v", slot2, warm)
	}
	h2, err := a.writebackSlot(ctx, ref(t, a, slot), "test")
	if err != nil || len(h2.Chain) != 2 || h2.Chain[0] != res.Head.Layer || h2.Delta != 3 {
		t.Fatalf("second commit %+v %v", h2, err)
	}
	// Unchanged: the same head, nothing written.
	if h3, err := a.writebackSlot(ctx, ref(t, a, slot), "test"); err != nil || h3.Layer != h2.Layer || h3.Pages != 0 {
		t.Fatalf("unchanged commit %+v %v", h3, err)
	}

	// Another machine mounts the head (contents fault in from the store).
	b, gr2 := fsWorker(t, mode, storeDir, cacheFSOptions{})
	if r, err := b.restoreSlot(ctx, "repo", h2, gr2); err != nil || r.Seconds <= 0 {
		t.Fatalf("restore: %+v %v", r, err)
	}
	check := func(data string) {
		got, err := os.ReadFile(filepath.Join(data, "target/debug/deps/libx.rlib"))
		fi, _ := os.Stat(filepath.Join(data, "target/debug/deps/libx.rlib"))
		if err != nil || string(got) != string(big) || !fi.ModTime().Equal(mt) {
			t.Fatalf("restored content: %v mtime %v", err, fi.ModTime())
		}
		if _, err := os.Stat(filepath.Join(data, "target/debug/old")); err == nil {
			t.Fatal("deleted file restored")
		}
		if l, err := os.Readlink(filepath.Join(data, "target/debug/link")); err != nil || l != "deps/libx.rlib" {
			t.Fatalf("symlink %q %v", l, err)
		}
	}
	rs, warm := runSession(t, b, check)
	if !warm {
		t.Fatal("restored slot not warm")
	}
	m, _ := readMeta(rs)
	if r, _ := restoreUsage(m); !r || m.Base != h2.Layer {
		t.Fatalf("restored meta %+v", m)
	}
	// A concurrent second slot is another view of the same head.
	s1, _, _ := b.slots.lease("repo", 24*time.Hour)
	s2, _, _ := b.slots.lease("repo", 24*time.Hour)
	if s1 == s2 {
		t.Fatal("shared slot")
	}
	for _, s := range []string{s1, s2} {
		if _, err := b.cfs.prepare(ctx, b.slots, s, "repo", 24*time.Hour); err != nil {
			t.Fatal(err)
		}
		check(filepath.Join(s, "data"))
	}
	b.slots.release(s1)
	b.slots.release(s2)

	if mode != fsPlain {
		// Reclaim drops resident content; it faults in again.
		if n := b.cfs.reclaim(ctx, 1<<40); n <= 0 {
			t.Fatalf("reclaimed %d", n)
		}
		b.cfs.unmount(rs) // drop kernel caches
		b.mountSlot(rs, "repo")
		check(filepath.Join(rs, "data"))
	}
}

// Pages of a file deleted before its commit was flushed are never
// written back.
func TestSlotDiscardUnflushed(t *testing.T) {
	ctx := context.Background()
	w, _ := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	big := []byte(strings.Repeat("y", 3<<20))
	slot, _ := runSession(t, w, func(data string) { put(t, filepath.Join(data, "tmp.bin"), big, time.Now()) })
	res, err := w.commitSlot(ctx, ref(t, w, slot))
	if err != nil || !res.changed {
		t.Fatalf("commit %+v %v", res, err)
	}
	n, _ := res.tree.Get("tmp.bin")
	k := n.Pages[0].Key
	if st, ok := res.repo.local.State(k); !ok || st != anyfs.Dirty {
		t.Fatal("page not dirty")
	}
	runSession(t, w, func(data string) { os.Remove(filepath.Join(data, "tmp.bin")) })
	if _, err := w.commitSlot(ctx, ref(t, w, slot)); err != nil {
		t.Fatal(err)
	}
	if _, ok := res.repo.local.State(k); ok {
		t.Fatal("page of a deleted file kept dirty")
	}
}

// Deep chains are compacted into one base layer.
func TestSlotCompaction(t *testing.T) {
	ctx := context.Background()
	w, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{CompactDepth: 2})
	var slot string
	var h []string
	for i := 0; i < 3; i++ {
		slot, _ = runSession(t, w, func(data string) { put(t, filepath.Join(data, "f"), []byte{byte(i)}, time.Now()) })
		res, err := w.saveSlot(ctx, "repo", gr)
		if err != nil {
			t.Fatal(err)
		}
		h = res.Head.Chain
	}
	if len(h) != 1 {
		t.Fatalf("chain %v, want a compacted base", h)
	}
	if m, _ := readMeta(slot); m.Head != h[0] {
		t.Fatalf("head %s", m.Head)
	}
}

// A rebase moves committed upper files into Files and mounts a fresh
// upper over the head.
func TestSlotRebase(t *testing.T) {
	for _, mode := range []fsMode{fsUser, fsKernel} {
		t.Run(mode.String(), func(t *testing.T) { testSlotRebase(t, mode) })
	}
}

func testSlotRebase(t *testing.T, mode fsMode) {
	ctx := context.Background()
	w, gr := fsWorker(t, mode, t.TempDir(), cacheFSOptions{})
	big := []byte(strings.Repeat("z", 2<<20))
	slot, _ := runSession(t, w, func(data string) { put(t, filepath.Join(data, "a/b.bin"), big, time.Now()) })
	if _, err := w.saveSlot(ctx, "repo", gr); err != nil {
		t.Fatal(err)
	}
	// A metadata-only change (metacopy in the kernel overlay) on top.
	runSession(t, w, func(data string) { os.Chmod(filepath.Join(data, "a/b.bin"), 0o600) })
	if _, err := w.saveSlot(ctx, "repo", gr); err != nil {
		t.Fatal(err)
	}
	if !w.rebaseSlot(ctx, ref(t, w, slot)) {
		t.Fatal("rebase failed")
	}
	m, _ := readMeta(slot)
	if m.Base != m.Head || m.Base == "" {
		t.Fatalf("meta %+v", m)
	}
	if !dirEmpty(filepath.Join(slot, "upper")) {
		t.Fatal("upper not fresh")
	}
	if fi, err := os.Stat(filepath.Join(slot, "data", "a/b.bin")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after rebase: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(slot, "data", "a/b.bin")); err != nil || string(b) != string(big) {
		t.Fatalf("content after rebase: %v", err)
	}
	if w.cfs.files.Stats().Faults != 0 {
		t.Fatal("rebase refetched content")
	}
}

// Commits after the first read only what changed since the last one:
// the upper directory's entries newer than the watermark (overlay modes)
// or the files with a newer ctime (plain).
func TestSlotIncrementalCommit(t *testing.T) {
	for _, mode := range []fsMode{fsPlain, fsUser, fsKernel} {
		t.Run(mode.String(), func(t *testing.T) { testSlotIncrementalCommit(t, mode) })
	}
}

func testSlotIncrementalCommit(t *testing.T, mode fsMode) {
	ctx := context.Background()
	w, _ := fsWorker(t, mode, t.TempDir(), cacheFSOptions{})
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	slot, _ := runSession(t, w, func(data string) {
		for i := 0; i < 200; i++ {
			put(t, filepath.Join(data, "target/debug/deps", fmt.Sprintf("d%d/f%d", i%10, i)), []byte(fmt.Sprint("f", i)), mt)
		}
	})
	res, err := w.commitSlot(ctx, ref(t, w, slot))
	if err != nil || res.scan.Hashed != 200 {
		t.Fatalf("first commit hashed %d: %v", res.scan.Hashed, err)
	}
	runSession(t, w, func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/d3/f3"), []byte("F3"), mt) // same size and mtime
		put(t, filepath.Join(data, "target/debug/new"), []byte("new"), mt)
		os.Remove(filepath.Join(data, "target/debug/deps/d4/f4"))
	})
	res, err = w.commitSlot(ctx, ref(t, w, slot))
	if err != nil || !res.changed {
		t.Fatalf("second commit %+v %v", res, err)
	}
	if res.scan.Hashed != 2 {
		t.Fatalf("second commit hashed %d files, want 2", res.scan.Hashed)
	}
	if mode != fsPlain && res.scan.Examined > 20 {
		t.Fatalf("second commit examined %d entries", res.scan.Examined)
	}
	n, _ := res.tree.Get("target/debug/deps/d3/f3")
	if _, gone := res.tree.Get("target/debug/deps/d4/f4"); gone || n == nil || n.Digest != string(anyfs.KeyOf([]byte("F3"))) || res.tree.Len() != 213 {
		t.Fatalf("tree after incremental commit: %d nodes", res.tree.Len())
	}
	t.Logf("%s: hashed %d, examined %d of %d", mode, res.scan.Hashed, res.scan.Examined, res.tree.Len())
}

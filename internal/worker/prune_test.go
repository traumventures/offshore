package worker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/proto"
)

func TestCargoUnit(t *testing.T) {
	for rel, want := range map[string]string{
		"debug/deps/libserde-0123456789abcdef.rlib":                            "debug\x00serde-0123456789abcdef",
		"debug/deps/serde-0123456789abcdef.d":                                  "debug\x00serde-0123456789abcdef",
		"debug/deps/app_api-0123456789abcdef":                                 "debug\x00app_api-0123456789abcdef",
		"debug/.fingerprint/serde-0123456789abcdef/lib":                        "debug\x00serde-0123456789abcdef",
		"x86_64-unknown-linux-gnu/release/build/ring-0123456789abcdef/out/x.o": "x86_64-unknown-linux-gnu/release\x00ring-0123456789abcdef",
	} {
		if got, ok := cargoUnit(rel); !ok || got != want {
			t.Errorf("cargoUnit(%q) = %q, %v; want %q", rel, got, ok, want)
		}
	}
	for _, rel := range []string{"debug/app", "debug/deps/notaunit.rlib", "debug/incremental/a-x/s-1/f", "CACHEDIR.TAG"} {
		if _, ok := cargoUnit(rel); ok {
			t.Errorf("cargoUnit(%q): want no unit", rel)
		}
	}
}

func write(t *testing.T, path string, n int, at time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// read simulates a read under relatime: atime moves past mtime.
func read(t *testing.T, path string, at time.Time) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, at, fi.ModTime())
}

func TestRecordRunTracksReadsAndRearmsAtime(t *testing.T) {
	data := t.TempDir()
	old := time.Now().Add(-time.Hour)
	write(t, filepath.Join(data, "a"), 4096, old)
	write(t, filepath.Join(data, "b"), 4096, old)
	idx := readIndex{"a": 1, "b": 1, "gone": 1}
	start := time.Now().Add(-time.Minute)
	read(t, filepath.Join(data, "a"), time.Now())
	write(t, filepath.Join(data, "c"), 4096, time.Now())
	if n := recordRun(data, start, 2, idx, nil); n < 3*4096 {
		t.Fatalf("size %d", n)
	}
	if idx["a"] != 2 || idx["b"] != 1 || idx["c"] != 2 {
		t.Fatalf("index %v", idx)
	}
	if _, ok := idx["gone"]; ok {
		t.Fatal("deleted file kept in index")
	}
	fi, _ := os.Stat(filepath.Join(data, "a"))
	if at, mt := fileTimes(fi); at.After(mt) {
		t.Fatal("atime of a read file not reset to its mtime")
	}
}

// rustSlot creates a slot whose target dir has two units and incremental
// sessions; unit "old" was last read in run 1, "new" in run 10.
func rustSlot(t *testing.T, root string) (string, slotMeta) {
	slot := filepath.Join(root, "repo", "0")
	data := filepath.Join(slot, "data")
	ts := time.Now().Add(-time.Hour)
	tgt := filepath.Join(data, "target", "debug")
	files := map[string]int64{
		"deps/libold-0123456789abcdef.rlib":     1,
		"deps/old-0123456789abcdef.d":           1,
		".fingerprint/old-0123456789abcdef/lib": 1,
		"deps/libnew-fedcba9876543210.rlib":     10,
		".fingerprint/new-fedcba9876543210/lib": 10,
		"app":                                   1, // no unit: never pruned
	}
	idx := readIndex{}
	for f, run := range files {
		write(t, filepath.Join(tgt, f), 64<<10, ts)
		idx[filepath.Join("target", "debug", f)] = run
	}
	write(t, filepath.Join(tgt, "incremental", "app-xyz", "s-old", "x"), 64<<10, ts.Add(-time.Hour))
	write(t, filepath.Join(tgt, "incremental", "app-xyz", "s-new", "x"), 64<<10, ts)
	os.Chtimes(filepath.Join(tgt, "incremental", "app-xyz", "s-old"), ts.Add(-time.Hour), ts.Add(-time.Hour))
	if err := saveReads(slot, idx); err != nil {
		t.Fatal(err)
	}
	meta := slotMeta{Created: ts.Unix(), LastUsed: ts.Unix(), Runs: 10, Caches: []slotCache{{Path: "target", Prune: config.PruneRust}}}
	writeMeta(slot, meta)
	return slot, meta
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneSlotPolicyStages(t *testing.T) {
	slot, meta := rustSlot(t, t.TempDir())
	tgt := filepath.Join(slot, "data", "target", "debug")
	r := pruneSlot(slot, meta, 0, defaultUnreadRuns, false, rmAll)
	if r.Sessions != 1 || r.Units != 1 || r.Cold {
		t.Fatalf("result %+v", r)
	}
	if exists(filepath.Join(tgt, "incremental", "app-xyz", "s-old")) || !exists(filepath.Join(tgt, "incremental", "app-xyz", "s-new")) {
		t.Fatal("stage 1 kept the old or removed the newest incremental session")
	}
	if exists(filepath.Join(tgt, "deps", "libold-0123456789abcdef.rlib")) || exists(filepath.Join(tgt, ".fingerprint", "old-0123456789abcdef")) {
		t.Fatal("stale unit not removed completely")
	}
	if !exists(filepath.Join(tgt, "deps", "libnew-fedcba9876543210.rlib")) || !exists(filepath.Join(tgt, "app")) {
		t.Fatal("recent unit or non-unit file removed")
	}
	if idx := loadReads(slot); idx[filepath.Join("target", "debug", "deps", "old-0123456789abcdef.d")] != 0 {
		t.Fatal("removed files stay in the read index")
	}
}

func TestPruneSlotTargetGoesCold(t *testing.T) {
	slot, meta := rustSlot(t, t.TempDir())
	// The latest run's unit is protected: a tiny target ends cold.
	if r := pruneSlot(slot, meta, 1, defaultUnreadRuns, false, rmAll); r.Cold || !exists(slot) || r.BytesLeft == 0 {
		t.Fatalf("without allowCold: %+v", r)
	}
	if r := pruneSlot(slot, meta, 1, defaultUnreadRuns, true, rmAll); !r.Cold || exists(slot) {
		t.Fatalf("with allowCold: %+v", r)
	}
}

func TestPruneRepoRPC(t *testing.T) {
	root := t.TempDir()
	sl, _ := newSlots(root)
	slot, _ := rustSlot(t, root)
	// An older second slot goes cold first.
	old := filepath.Join(root, "repo", "1")
	write(t, filepath.Join(old, "data", "target", "x"), 64<<10, time.Now())
	writeMeta(old, slotMeta{Created: time.Now().Unix(), LastUsed: time.Now().Add(-2 * time.Hour).Unix(), Bytes: 64 << 10})
	m, _ := readMeta(slot)
	m.Bytes = treeSize(filepath.Join(slot, "data"))
	writeMeta(slot, m)
	w := &Worker{slots: sl, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	res := w.slotOp(t.Context(), proto.SlotOp{Op: proto.SlotPrune, Repo: "repo", TargetBytes: m.Bytes})
	if res.Err != "" || res.Freed < 64<<10 || exists(old) || !exists(slot) {
		t.Fatalf("prune to the newest slot's size: %+v", res)
	}
	if res := w.slotOp(t.Context(), proto.SlotOp{Op: proto.SlotPrune, Repo: "nope"}); res.Err == "" {
		t.Fatal("unknown repo accepted")
	}
	if res := w.slotOp(t.Context(), proto.SlotOp{Op: "explode"}); res.Err == "" {
		t.Fatal("unknown op accepted")
	}
}

// A prune before a placement must not wait for a long maintenance prune
// (production: placements waited up to 20 minutes behind one), nor take
// the lock at all for a repository without a slot.
func TestPruneRepoBoundedByRunningPrune(t *testing.T) {
	root := t.TempDir()
	sl, _ := newSlots(root)
	rustSlot(t, root)
	w := &Worker{slots: sl, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	old := pruneLockWait
	pruneLockWait = 100 * time.Millisecond
	defer func() { pruneLockWait = old }()
	w.pruneMu.Lock()
	begin := time.Now()
	res := w.slotOp(t.Context(), proto.SlotOp{Op: proto.SlotPrune, Repo: "repo"})
	if res.Err == "" || time.Since(begin) > 5*time.Second {
		t.Fatalf("prune behind a running prune: %+v after %s", res, time.Since(begin))
	}
	begin = time.Now()
	if res := w.slotOp(t.Context(), proto.SlotOp{Op: proto.SlotPrune, Repo: "nope"}); !strings.Contains(res.Err, "no build slot") || time.Since(begin) > 50*time.Millisecond {
		t.Fatalf("unknown repo waited for the lock: %+v after %s", res, time.Since(begin))
	}
	w.pruneMu.Unlock()
	if res := w.slotOp(t.Context(), proto.SlotOp{Op: proto.SlotPrune, Repo: "repo"}); res.Err != "" {
		t.Fatalf("prune after the lock is free: %+v", res)
	}
}

func TestSlotBudgetAndCaches(t *testing.T) {
	req := proto.Exec{Caches: []proto.Cache{
		{Scope: "slot", Paths: []string{"target/"}, Prune: "rust", MaxBytes: 10},
		{Scope: "slot", Paths: []string{"out"}, MaxBytes: 5},
		{Scope: "machine", Paths: []string{"x"}},
	}}
	cs := slotCaches(req)
	if len(cs) != 2 || cs[0].Path != "target" || slotBudget(slotMeta{Caches: cs}) != 15 {
		t.Fatalf("caches %+v", cs)
	}
	cs[1].MaxBytes = 0
	if slotBudget(slotMeta{Caches: cs}) != 0 {
		t.Fatal("an unbounded cache must make the slot unbounded")
	}
}

func TestBudgetBytes(t *testing.T) {
	if got := budgetBytes(config.DiskBudget{}, 1000); got != 900 {
		t.Errorf("default budget = %d, want 90%%", got)
	}
	b, _ := config.ParseDiskBudget("50G")
	if got := budgetBytes(b, 100<<30); got != 50<<30 {
		t.Errorf("50G budget = %d", got)
	}
}

func rmAll(p string) { os.RemoveAll(p) }

func TestReclaimSourcesKeepsOpenFiles(t *testing.T) {
	files, err := anyfs.OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []anyfs.Node
	for _, c := range []string{"aaaa", "bbbb", "cccc"} {
		n := anyfs.Node{Path: c, Kind: anyfs.File, Size: 4, Digest: string(anyfs.KeyOf([]byte(c)))}
		if _, err := files.EnsureWith(context.Background(), &n, func(_ context.Context, _ *anyfs.Node, w io.Writer) error {
			_, err := io.WriteString(w, c)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, n)
	}
	f, _ := files.Open(&nodes[0])
	defer f.Close()
	w := &Worker{srcFiles: files, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	over := func() int64 { return files.Stats().ResidentBytes }
	if left := w.reclaimSources(over); left != 4 {
		t.Fatalf("left over %d, want the open file's 4 bytes", left)
	}
	if _, ok := files.Local(&nodes[0]); !ok {
		t.Fatal("open source file reclaimed")
	}
}

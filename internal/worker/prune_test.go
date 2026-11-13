package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
	if n, garbage := recordRun(data, start, 2, idx, nil, nil); n < 3*4096 || garbage != 0 {
		t.Fatalf("size %d, garbage %d", n, garbage)
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
	r := pruneSlot(slot, meta, 0)
	if r.Sessions != 1 || r.Units != 1 {
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

// A size target never removes the latest run's units nor the slot.
func TestPruneSlotTargetKeepsLatestRun(t *testing.T) {
	slot, meta := rustSlot(t, t.TempDir())
	r := pruneSlot(slot, meta, 1)
	tgt := filepath.Join(slot, "data", "target", "debug")
	if !exists(filepath.Join(tgt, "deps", "libnew-fedcba9876543210.rlib")) || r.BytesLeft == 0 || exists(filepath.Join(tgt, "deps", "libold-0123456789abcdef.rlib")) {
		t.Fatalf("result %+v", r)
	}
}

// Garbage is computed at the end of every run: rust units unread for 3
// runs, lru files unread for 5.
func TestRecordRunGarbage(t *testing.T) {
	data := t.TempDir()
	old := time.Now().Add(-time.Hour)
	write(t, filepath.Join(data, "target/debug/deps/libold-0123456789abcdef.rlib"), 64<<10, old)
	write(t, filepath.Join(data, "target/debug/deps/libnew-fedcba9876543210.rlib"), 64<<10, old)
	write(t, filepath.Join(data, "node_modules/x.js"), 64<<10, old)
	caches := []slotCache{{Path: "target", Prune: config.PruneRust}, {Path: "node_modules", Prune: config.PruneLRU}}
	idx := readIndex{"target/debug/deps/libold-0123456789abcdef.rlib": 2, "target/debug/deps/libnew-fedcba9876543210.rlib": 4, "node_modules/x.js": 2}
	_, garbage := recordRun(data, time.Now(), 5, idx, map[string]bool{}, caches)
	if garbage < 64<<10 || garbage >= 128<<10 {
		t.Fatalf("garbage %d, want the old unit only", garbage)
	}
	_, garbage = recordRun(data, time.Now(), 7, idx, map[string]bool{}, caches)
	if garbage < 3*(64<<10) {
		t.Fatalf("garbage %d, want everything", garbage)
	}
}

func TestSlotBudgetAndCaches(t *testing.T) {
	req := proto.Exec{Caches: []proto.Cache{
		{Scope: "slot", Paths: []string{"target/"}, Prune: "rust", MaxBytes: 10},
		{Scope: "slot", Paths: []string{"out"}, MaxBytes: 5},
		{Scope: "machine", Paths: []string{"x"}},
	}}
	cs := slotCaches(req)
	if len(cs) != 2 || cs[0].Path != "target" || cachesBudget(slotMeta{Caches: cs}) != 15 {
		t.Fatalf("caches %+v", cs)
	}
	cs[1].MaxBytes = 0
	if cachesBudget(slotMeta{Caches: cs}) != 0 {
		t.Fatal("an unbounded cache must make the slot unbounded")
	}
	const g = int64(1) << 30
	for _, c := range []struct {
		m     slotMeta
		total int64
		want  int64
	}{
		{slotMeta{Caches: []slotCache{{Path: "t", MaxBytes: 30 * g}}}, 200 * g, 30 * g},
		{slotMeta{ColdBytes: 12 * g}, 200 * g, 24 * g},
		{slotMeta{ColdBytes: 1 * g}, 200 * g, 10 * g},  // at least 10G
		{slotMeta{ColdBytes: 60 * g}, 200 * g, 80 * g}, // at most 40 %
	} {
		if got := slotBudget(c.m, c.total); got != c.want {
			t.Errorf("slot budget %+v = %d GiB, want %d", c.m, got/g, c.want/g)
		}
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

// Cargo units in both target layouts: deps/ and .fingerprint/ (and
// build/<unit>-<hash>/) of older cargo, build/<package>/<hash>/ of the
// build-dir layout v2.
func TestCargoUnitRootLayouts(t *testing.T) {
	for _, c := range []struct{ rel, key, root string }{
		{"debug/deps/libserde-0123456789abcdef.rlib", "debug\x00serde-0123456789abcdef", "debug/deps/libserde-0123456789abcdef.rlib"},
		{"debug/.fingerprint/serde-0123456789abcdef/lib-serde", "debug\x00serde-0123456789abcdef", "debug/.fingerprint/serde-0123456789abcdef"},
		{"debug/build/serde-0123456789abcdef/out/x", "debug\x00serde-0123456789abcdef", "debug/build/serde-0123456789abcdef"},
		{"debug/build/serde_json/4e072dbae59c1bf0/out/libserde_json-4e072dbae59c1bf0.rlib", "debug\x00serde_json-4e072dbae59c1bf0", "debug/build/serde_json/4e072dbae59c1bf0"},
		{"x86_64-unknown-linux-gnu/release/build/serde_json/ac657e40b17285ad/run/stdout", "x86_64-unknown-linux-gnu/release\x00serde_json-ac657e40b17285ad", "x86_64-unknown-linux-gnu/release/build/serde_json/ac657e40b17285ad"},
		{"debug/smoke", "", ""},
		{"debug/incremental/smoke-0123456789abcdef/s-x/a.o", "", ""},
	} {
		key, root, ok := cargoUnitRoot(c.rel)
		if ok != (c.key != "") || key != c.key || root != c.root {
			t.Errorf("%s: %q %q %v, want %q %q", c.rel, key, root, ok, c.key, c.root)
		}
	}
}

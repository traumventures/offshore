package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/proto"
)

// fakeDisk makes the worker's statfs a disk holding exactly the cache
// directory (its files' blocks) with headroom bytes free at the start.
func fakeDisk(w *Worker, headroom int64) {
	cache := w.cfg.CacheDir
	total := treeSize(cache) + headroom
	w.statfs = func() (int64, int64, bool) { return total, total - treeSize(cache), true }
}

// ledgerSetup is a plain-mode worker with a flushed slot of repository
// "repo" (3 MiB), two dormant repositories sharing a pool page and a
// machine cache directory.
func ledgerSetup(t *testing.T) *Worker {
	t.Helper()
	w, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	start := time.Now()
	slot, _ := runSession(t, w, func(data string) {
		for _, n := range []string{"a", "b", "c"} {
			put(t, filepath.Join(data, "target/debug/deps/lib"+n+"-0123456789abcdef.rlib"), []byte(strings.Repeat(n, 1<<20)), mt)
		}
	})
	w.recordSlotRun(slot, start, nil)
	if _, err := w.saveSlot(t.Context(), "repo", gr); err != nil {
		t.Fatal(err)
	}
	dir := w.cfs.dir
	writePage(t, dir, "gone", "aa01", 1<<20)
	p := filepath.Join(dir, "repos", "gone2", "local", "pages", "aa", "aa01")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.Link(filepath.Join(dir, "repos", "gone", "local", "pages", "aa", "aa01"), p); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, r := range []string{"gone", "gone2"} {
		os.Chtimes(filepath.Join(dir, "repos", r, "local"), old, old)
		os.Chtimes(filepath.Join(dir, "repos", r), old, old)
	}
	write(t, filepath.Join(w.cfg.DepsDir, "registry", "x"), 256<<10, time.Now())
	w.measureMachineCaches()
	return w
}

// The ledger accounts for every used byte once: pinned plus reclaimable
// is exactly the used disk, and the storage items (slots by their own
// disk, page tiers measured with hard links once, the pool, dormant
// tiers, caches) leave no large "other" and count nothing twice.
func TestLedgerAccountsEveryByteOnce(t *testing.T) {
	w := ledgerSetup(t)
	fakeDisk(w, 1<<30)
	d := w.diskStatus(w.load())
	l := d.Ledger
	if l == nil {
		t.Fatal("no ledger")
	}
	used := l.Total - l.Free
	if l.Pinned < 0 || l.Pinned+l.Reclaimable() != used {
		t.Fatalf("pinned %d + reclaimable %d != used %d", l.Pinned, l.Reclaimable(), used)
	}
	classes := map[string]int64{}
	for i, e := range l.Reclaim {
		classes[e.Class] += e.Bytes
		if i > 0 {
			p := l.Reclaim[i-1]
			if p.Cost/float64(p.Bytes) > e.Cost/float64(e.Bytes) {
				t.Errorf("ledger not in ascending cost per byte: %+v before %+v", p, e)
			}
		}
	}
	if classes[proto.ReclaimSlot] < 3<<20 || classes[proto.ReclaimDormant] < 1<<20 || classes[proto.ReclaimDormant] > 1<<20+64<<10 {
		t.Fatalf("classes %v (the shared dormant page counts once)", classes)
	}
	var sum, other int64
	for _, it := range d.Items {
		if it.Kind == proto.StorageOther {
			other += it.Bytes
			continue
		}
		switch it.Kind {
		case proto.StorageSlot:
			continue // logical size; its disk is in the sum below
		}
		sum += it.Bytes
	}
	for _, r := range w.slots.list() {
		sum += r.Meta.Physical
	}
	if sum > used+64<<10 || used-sum > 256<<10 || other > 256<<10 {
		t.Fatalf("items %d (other %d) vs used %d: double counted or untracked; items %+v", sum, other, used, d.Items)
	}
}

// reclaim frees in ledger order until statfs reports the wanted free
// bytes, keeps the slots of the repository the room is for, and reports
// the bytes statfs says it freed.
func TestReclaimFreesToWant(t *testing.T) {
	w := ledgerSetup(t)
	fakeDisk(w, 0)
	slot := filepath.Join(w.cfg.CacheDir, "build", "repo", "0")
	_, free, _ := w.diskStat()
	w.reclaimMu.Lock()
	freed, err := w.reclaim(t.Context(), free+1<<40, "repo", false)
	w.reclaimMu.Unlock()
	if err == nil || !exists(slot) {
		t.Fatalf("impossible want: err %v, kept slot %v", err, exists(slot))
	}
	if _, err := os.Stat(filepath.Join(w.cfs.dir, "repos", "gone")); err == nil {
		t.Fatal("dormant tier not reclaimed")
	}
	_, after, _ := w.diskStat()
	if freed != after-free || freed < 1<<20 {
		t.Fatalf("freed %d, statfs delta %d", freed, after-free)
	}
	w.measureMachineCaches()
	_, free, _ = w.diskStat()
	w.reclaimMu.Lock()
	freed, err = w.reclaim(t.Context(), free+2<<20, "", false)
	w.reclaimMu.Unlock()
	if err != nil || freed < 2<<20 {
		t.Fatalf("freed %d, err %v", freed, err)
	}
}

// SlotOp reclaim makes FreeBytes free beyond the reserve; behind a
// running reclaim it fails within its bound.
func TestSlotOpReclaim(t *testing.T) {
	w := ledgerSetup(t)
	fakeDisk(w, reserveFloorMin) // reserve: the 5 GiB floor
	res := w.slotOp(t.Context(), proto.SlotOp{Op: proto.SlotReclaim, Repo: "other", FreeBytes: 1 << 20})
	if res.Err != "" || res.Freed < 1<<20 {
		t.Fatalf("reclaim %+v", res)
	}
	old := reclaimWait
	reclaimWait = 100 * time.Millisecond
	defer func() { reclaimWait = old }()
	w.reclaimMu.Lock()
	begin := time.Now()
	res = w.slotOp(t.Context(), proto.SlotOp{Op: proto.SlotReclaim, Repo: "repo", FreeBytes: 1 << 30})
	w.reclaimMu.Unlock()
	if res.Err == "" || time.Since(begin) > 5*time.Second {
		t.Fatalf("reclaim behind a running reclaim: %+v after %s", res, time.Since(begin))
	}
	if res := w.slotOp(t.Context(), proto.SlotOp{Op: "explode"}); res.Err == "" {
		t.Fatal("unknown op accepted")
	}
}

// The trim after a session frees disk, not just merged-view bytes: the
// stale unit is deleted, the deletion committed and flushed, the slot
// rebased and the pages no tree references dropped.
func TestTrimFreesPhysicalBytes(t *testing.T) {
	for _, mode := range []fsMode{fsPlain, fsUser, fsKernel} {
		t.Run(mode.String(), func(t *testing.T) {
			w, gr := fsWorker(t, mode, t.TempDir(), cacheFSOptions{})
			w.noteGrant("repo", gr) // as a session does
			oldMin := minSlotBudget
			minSlotBudget = 1 << 20
			defer func() { minSlotBudget = oldMin }()
			mt := time.Now().Add(-time.Hour).Truncate(time.Second)
			slot, _ := runSession(t, w, func(data string) {
				put(t, filepath.Join(data, "target/debug/deps/libold-0123456789abcdef.rlib"), []byte(strings.Repeat("o", 4<<20)), mt)
				put(t, filepath.Join(data, "target/debug/deps/libnew-fedcba9876543210.rlib"), []byte(strings.Repeat("n", 1<<20)), mt)
			})
			if _, err := w.saveSlot(t.Context(), "repo", gr); err != nil {
				t.Fatal(err)
			}
			if mode != fsPlain {
				m, _ := readMeta(slot)
				w.rebaseSlot(t.Context(), slotRef{Repo: "repo", Path: slot, Meta: m}) // the files live in the lower now
			}
			saveReads(slot, readIndex{"target/debug/deps/libold-0123456789abcdef.rlib": 1, "target/debug/deps/libnew-fedcba9876543210.rlib": 10})
			w.slots.update(slot, func(m *slotMeta) {
				m.Runs, m.Bytes = 10, 5<<20
				m.Caches = []slotCache{{Path: "target", Prune: config.PruneRust, MaxBytes: 2 << 20}}
			})
			before := treeSize(w.cfg.CacheDir)
			head := w.cfs.flushedHeads()[0].Layer
			w.afterSession(slot, "repo")
			after := treeSize(w.cfg.CacheDir)
			if before-after < 7<<20 { // the unit in the slot or file cache and its pages
				t.Fatalf("trim freed %d bytes of disk (before %d, after %d)", before-after, before, after)
			}
			if h := w.cfs.flushedHeads()[0].Layer; h == head {
				t.Fatal("trim not flushed to the store head")
			}
			if _, err := os.Stat(filepath.Join(slot, "data", "target/debug/deps/libnew-fedcba9876543210.rlib")); err != nil {
				t.Fatalf("recent unit gone: %v", err)
			}
		})
	}
}

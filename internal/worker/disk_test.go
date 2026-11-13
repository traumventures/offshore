package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/config"
)

func TestSlotSizesAndDiskStatus(t *testing.T) {
	cache := t.TempDir()
	sl, err := newSlots(filepath.Join(cache, "build"))
	if err != nil {
		t.Fatal(err)
	}
	slot, _, err := sl.lease("repo", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(slot, "data")
	os.MkdirAll(filepath.Join(data, "target", "debug"), 0o755)
	os.WriteFile(filepath.Join(data, "target", "debug", "a"), make([]byte, 64<<10), 0o644)
	os.Link(filepath.Join(data, "target", "debug", "a"), filepath.Join(data, "target", "a-link"))
	n := treeSize(data)
	if n < 64<<10 || n > 80<<10 {
		t.Fatalf("tree size %d (hard link counted twice?)", n)
	}
	if sl.size(slot) != n {
		t.Fatal("size of an unmeasured slot")
	}
	sl.releaseSized(slot, n)
	deps := filepath.Join(cache, "deps")
	os.MkdirAll(deps, 0o755)
	os.WriteFile(filepath.Join(deps, "reg"), make([]byte, 8<<10), 0o644)
	w := &Worker{cfg: Config{CacheDir: cache, DepsDir: deps}, slots: sl}
	w.measureMachineCaches()
	d := w.diskStatus(w.load())
	if d == nil || d.Total <= 0 || d.Used <= 0 || d.Slots != n || d.SlotBytes["repo"] != n || d.MachineCaches < 8<<10 {
		t.Fatalf("disk status %+v", d)
	}
}

func TestSourceCacheBytes(t *testing.T) {
	budget, _ := config.ParseDiskBudget("50%")
	if got := sourceCacheBytes(Config{DiskBudget: budget}, 1600); got != 100 {
		t.Fatalf("default: %d, want an eighth of the budget", got)
	}
	sc, _ := config.ParseSourceCache("300")
	if got := sourceCacheBytes(Config{DiskBudget: budget, SourceCache: sc}, 1600); got != 300 {
		t.Fatalf("size: %d", got)
	}
}

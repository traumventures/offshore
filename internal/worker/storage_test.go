package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/proto"
)

// The storage breakdown lists slots with their worktree and branch, the
// repository's page tier with its head breakdown, machine and global
// caches by name, and the rest of the disk.
func TestStorageItems(t *testing.T) {
	a, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	a.cfg.SccacheDir = filepath.Join(a.cfg.DepsDir, "sccache")
	put(t, filepath.Join(a.cfg.DepsDir, "usr__local__cargo__registry", "x"), []byte(strings.Repeat("r", 5000)), time.Now())
	put(t, filepath.Join(a.cfg.SccacheDir, "y"), []byte(strings.Repeat("s", 3000)), time.Now())
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	slot, _ := runSession(t, a, func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/liba.rlib"), []byte(strings.Repeat("a", 1<<20)), mt)
		put(t, filepath.Join(data, "src/main.rs"), []byte("fn main(){}"), mt)
	})
	a.slots.update(slot, func(m *slotMeta) { m.Worktree, m.Branch = "/home/u/wt", "wp/x" })
	if _, err := a.saveSlot(context.Background(), "repo", gr); err != nil {
		t.Fatal(err)
	}
	a.measureStorage(context.Background())
	items := a.storageItems(1 << 40)
	by := map[string]proto.StorageItem{}
	for _, it := range items {
		by[it.Kind+":"+it.Name] = it
	}
	sl, ok := by["slot:"+slot]
	if !ok || sl.Worktree != "/home/u/wt" || sl.Branch != "wp/x" || sl.Repo != "repo" || sl.Committed == 0 || sl.Stored == 0 {
		t.Fatalf("slot item %+v (all %+v)", sl, items)
	}
	pg := by["pages:repo"]
	if pg.Resident == 0 || pg.Classes == nil || len(pg.TopDirs) == 0 || pg.TopDirs[0].Path != "target/debug/deps" {
		t.Fatalf("pages item %+v", pg)
	}
	if by["machine-cache:usr__local__cargo__registry"].Bytes < 5000 || by["global:sccache"].Bytes < 3000 {
		t.Fatalf("caches %+v", items)
	}
	if by["other:images, system and unaccounted"].Bytes <= 0 {
		t.Fatalf("other missing: %+v", items)
	}
	_ = os.Remove
}

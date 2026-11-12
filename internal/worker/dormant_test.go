package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// writePage writes a page file into a repository's local tier and links
// it into the machine pool, as anyfs.Local does.
func writePage(t *testing.T, anyfsDir, repo, key string, size int) {
	t.Helper()
	p := filepath.Join(anyfsDir, "repos", repo, "local", "pages", key[:2], key)
	pp := filepath.Join(anyfsDir, "pool", key[:2], key)
	for _, d := range []string{filepath.Dir(p), filepath.Dir(pp)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(p, make([]byte, size), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(p, pp); err != nil {
		t.Fatal(err)
	}
}

// The local tiers of repositories without an open stack (a previous
// worker process; slots gone) are counted and, while the disk is over
// its budget, dropped least recently used first with their pool pages;
// a repository with a build slot or an open stack stays.
func TestDormantRepositoriesAreCountedAndReclaimed(t *testing.T) {
	w, _ := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	dir := w.cfs.dir
	writePage(t, dir, "gone", "aa01", 1<<20)
	writePage(t, dir, "slotted", "bb01", 1<<20)
	writePage(t, dir, "open", "cc01", 1<<20)
	if _, err := w.cfs.repo("open"); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(w.cfg.CacheDir, "build", "slotted", "s1")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(slot, slotMeta{}); err != nil {
		t.Fatal(err)
	}
	ds := w.cfs.dormantRepos(true)
	if len(ds) != 2 {
		t.Fatalf("dormant %+v, want gone and slotted", ds)
	}
	if n := w.cfs.dormantBytes(); n < 2<<20 {
		t.Fatalf("dormant bytes %d, want at least 2 MiB", n)
	}
	w.measureMachineCaches()
	if mc := w.machineCacheBytes.Load(); mc < 2<<20 {
		t.Fatalf("machine caches %d do not count the dormant tiers", mc)
	}
	calls := 0
	over := func() int64 { // over until the first drop
		calls++
		if _, err := os.Stat(filepath.Join(dir, "repos", "gone")); err == nil {
			return 1
		}
		return 0
	}
	if left := w.reclaimDormant(over); left > 0 {
		t.Fatalf("still over: %d", left)
	}
	for repo, want := range map[string]bool{"gone": false, "slotted": true, "open": true} {
		_, err := os.Stat(filepath.Join(dir, "repos", repo))
		if (err == nil) != want {
			t.Errorf("repo %s present %v, want %v", repo, err == nil, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pool", "aa", "aa01")); err == nil {
		t.Error("pool page of the dropped repository survived")
	}
	if _, err := os.Stat(filepath.Join(dir, "pool", "bb", "bb01")); err != nil {
		t.Error("pool page of a kept repository was removed")
	}
	if es, _ := os.ReadDir(filepath.Join(dir, "trash")); len(es) != 0 {
		t.Errorf("trash not emptied: %v", es)
	}
}

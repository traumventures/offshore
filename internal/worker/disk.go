package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/traumventures/offshore/internal/proto"
)

// walkFiles calls fn for every non-directory below dir, walking
// directories in parallel (a Rust target directory has 10^5 files). fn
// must be safe for concurrent use.
func walkFiles(dir string, fn func(path string, fi os.FileInfo)) {
	sem := make(chan struct{}, 4*runtime.NumCPU())
	var wg sync.WaitGroup
	var walk func(d string)
	walk = func(d string) {
		defer wg.Done()
		sem <- struct{}{}
		des, _ := os.ReadDir(d)
		<-sem
		for _, de := range des {
			p := filepath.Join(d, de.Name())
			if de.IsDir() {
				wg.Add(1)
				go walk(p)
				continue
			}
			if fi, err := de.Info(); err == nil {
				fn(p, fi)
			}
		}
	}
	wg.Add(1)
	walk(dir)
	wg.Wait()
}

// treeSize returns the bytes allocated by the files below dir (hard links
// counted once).
func treeSize(dir string) int64 {
	var total atomic.Int64
	var seen sync.Map
	walkFiles(dir, func(_ string, fi os.FileInfo) { total.Add(allocated(fi, &seen)) })
	return total.Load()
}

// allocated returns the disk bytes of a file; a file with several links
// counts once per seen map.
func allocated(fi os.FileInfo, seen *sync.Map) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Size()
	}
	if st.Nlink > 1 {
		key := [2]uint64{uint64(st.Dev), st.Ino}
		if _, dup := seen.LoadOrStore(key, true); dup {
			return 0
		}
	}
	return st.Blocks * 512
}

// fsUsage returns the total and used bytes of the filesystem holding dir.
func fsUsage(dir string) (total, used int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	total = int64(st.Blocks) * st.Bsize
	used = total - int64(st.Bfree)*st.Bsize
	return total, used, true
}

// measureMachineCaches records the size of the machine cache directory.
func (s *Worker) measureMachineCaches() {
	if s.cfg.DepsDir != "" {
		s.machineCacheBytes.Store(treeSize(s.cfg.DepsDir))
	}
}

// diskStatus reports the cache disk: filesystem totals, slot bytes (from
// the slot metadata) and the machine caches (measured by the periodic gc).
func (s *Worker) diskStatus() *proto.DiskStatus {
	total, used, ok := fsUsage(s.cfg.CacheDir)
	if !ok {
		return nil
	}
	d := &proto.DiskStatus{Total: total, Used: used, MachineCaches: s.machineCacheBytes.Load(), SlotBytes: map[string]int64{},
		Budget: budgetBytes(s.cfg.DiskBudget, total)}
	for _, r := range s.slots.list() {
		d.Slots += r.Meta.Bytes
		d.SlotBytes[r.Repo] += r.Meta.Bytes
	}
	return d
}

package worker

import (
	"context"
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

// dirBytes returns the bytes allocated below dir, hard links counted once
// per seen map (shared by several measurements).
func dirBytes(dir string, seen *sync.Map) int64 {
	var total atomic.Int64
	walkFiles(dir, func(_ string, fi os.FileInfo) { total.Add(allocated(fi, seen)) })
	return total.Load()
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

// measureMachineCaches measures the storage (measureStorage) and records
// the size of the machine caches: the machine cache directory, the
// written back blobs and the local tiers of dormant repositories (the
// open ones are counted live, residentBytes).
func (s *Worker) measureMachineCaches() {
	s.measureStorage(context.Background())
	var n int64
	if s.cfg.DepsDir != "" {
		n += treeSize(s.cfg.DepsDir)
	}
	s.storage.mu.Lock()
	n += s.storage.dirs[writebackItem]
	for _, d := range s.storage.dormant {
		n += d.bytes
	}
	s.storage.mu.Unlock()
	s.machineCacheBytes.Store(n)
}

// diskStatus reports the cache disk: its ledger (physical, statfs),
// filesystem totals, slot bytes (from the slot metadata, logical) and the
// machine caches (measured by the periodic gc). The caller holds s.mu
// (load: the running sessions).
func (s *Worker) diskStatus(load sessionLoad) *proto.DiskStatus {
	l, _ := s.ledgerWith(load)
	if l == nil {
		return nil
	}
	total, used := l.Total, l.Total-l.Free
	mc := s.machineCacheBytes.Load()
	if s.cfs != nil {
		mc += s.cfs.residentBytes()
	}
	d := &proto.DiskStatus{Total: total, Used: used, MachineCaches: mc, SlotBytes: map[string]int64{},
		Budget: budgetBytes(s.cfg.DiskBudget, total), Ledger: l}
	for _, r := range s.slots.list() {
		d.Slots += r.Meta.Bytes
		d.SlotBytes[r.Repo] += r.Meta.Bytes
	}
	d.Items = s.storageItems(used)
	return d
}

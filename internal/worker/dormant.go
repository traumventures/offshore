package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Dormant repositories: the local page tier, layer copies and access
// data of a repository live below <anyfs>/repos/<id> and outlive its build
// slots and the worker process (machines stop and start on the same
// disk). The cache file system opens a repository only when one of its
// slots is mounted, so after a restart, or once its slots were gone, the
// directory was neither counted (machine caches, storage items) nor
// reclaimed by the capacity and the disk budget: in production two
// machines held 135 GiB of such pages with no build slot left and filled
// the disk (ENOSPC under target/ and on source reads).
//
// A dormant repository has no open stack in this process and no build
// slot on the machine. Nothing reads its pages (a new slot opens the
// repository again and restores from the store, whose head references
// written-back pages only), so the whole directory is reclaimable.

// dormantRepo is a repository directory without an open stack.
type dormantRepo struct {
	id    string
	dir   string
	used  time.Time // newest modification of its metadata
	bytes int64
}

// dormantRepos lists the repository directories without an open stack,
// least recently used first; measure adds their sizes (a page shared
// through the pool counts once, in the first repository).
func (c *cacheFS) dormantRepos(measure bool) []dormantRepo {
	es, err := os.ReadDir(filepath.Join(c.dir, "repos"))
	if err != nil {
		return nil
	}
	c.mu.Lock()
	var out []dormantRepo
	for _, e := range es {
		if !e.IsDir() || c.repos[e.Name()] != nil {
			continue
		}
		d := dormantRepo{id: e.Name(), dir: filepath.Join(c.dir, "repos", e.Name())}
		if fi, err := e.Info(); err == nil {
			d.used = fi.ModTime()
		}
		out = append(out, d)
	}
	c.mu.Unlock()
	for i := range out {
		for _, f := range []string{accessFile, workingSetFile, "local", "layers"} {
			if fi, err := os.Stat(filepath.Join(out[i].dir, f)); err == nil && fi.ModTime().After(out[i].used) {
				out[i].used = fi.ModTime()
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].used.Before(out[j].used) })
	if measure {
		var seen sync.Map
		for i := range out {
			var n atomic.Int64
			walkFiles(out[i].dir, func(_ string, fi os.FileInfo) { n.Add(allocated(fi, &seen)) })
			out[i].bytes = n.Load()
		}
	}
	return out
}

// dormantBytes is the disk held by dormant repositories.
func (c *cacheFS) dormantBytes() int64 {
	var n int64
	for _, d := range c.dormantRepos(true) {
		n += d.bytes
	}
	return n
}

// dropRepo deletes a dormant repository's directory (no open stack; the
// caller checked that no build slot of it exists). The directory is
// renamed away under the lock, so a slot mounted concurrently opens a
// fresh, empty stack. It reports whether the repository was dropped.
func (c *cacheFS) dropRepo(id string) bool {
	dir := filepath.Join(c.dir, "repos", id)
	trash := filepath.Join(c.dir, "trash")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return false
	}
	to := filepath.Join(trash, fmt.Sprintf("%s-%d", id, time.Now().UnixNano()))
	c.mu.Lock()
	if c.repos[id] != nil {
		c.mu.Unlock()
		return false
	}
	err := os.Rename(dir, to)
	c.mu.Unlock()
	if err != nil {
		return false
	}
	os.RemoveAll(to)
	return true
}

// sweepPool deletes pool page files no tier links any more (the pages of
// a dropped repository) and returns the bytes freed. A tier linking a
// page concurrently keeps its own link; only the sharing is lost.
func (c *cacheFS) sweepPool() int64 {
	var freed atomic.Int64
	walkFiles(filepath.Join(c.dir, "pool"), func(p string, fi os.FileInfo) {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Nlink > 1 {
			return
		}
		if os.Remove(p) == nil {
			freed.Add(st.Blocks * 512)
		}
	})
	return freed.Load()
}

// cleanTrash removes repositories dropped by a previous process.
func (c *cacheFS) cleanTrash() { os.RemoveAll(filepath.Join(c.dir, "trash")) }

// reclaimDormant drops dormant repositories without a build slot, least
// recently used first, while the disk is over (see enforceDiskBudget);
// it returns the bytes still over.
func (s *Worker) reclaimDormant(over func() int64) int64 {
	o := over()
	if o <= 0 || s.cfs == nil {
		return o
	}
	withSlot := map[string]bool{}
	for _, r := range s.slots.list() {
		withSlot[r.Repo] = true
	}
	dropped := 0
	for _, d := range s.cfs.dormantRepos(false) {
		if over() <= 0 {
			break
		}
		if withSlot[d.id] || !s.cfs.dropRepo(d.id) {
			continue
		}
		dropped++
		freed := s.cfs.sweepPool()
		s.log.Info("dropped the local tier of a repository without build slots for the disk budget", "repo", d.id, "pool_freed", freed)
	}
	if dropped > 0 {
		go s.measureMachineCaches()
	}
	return over()
}

// writebackDir is the local blob store of written back files (cas).
func (s *Worker) writebackDir() string {
	if s.store == nil {
		return ""
	}
	return s.store.Dir
}

// reclaimWriteback deletes the least recently used blobs of written back
// files while no session runs (a writeback reads them; afterwards they
// only spare a fetch from the client) and returns the bytes still over.
// They were neither counted nor pruned: 10 GiB on a production machine.
func (s *Worker) reclaimWriteback(over func() int64) int64 {
	o := over()
	dir := s.writebackDir()
	if o <= 0 || dir == "" {
		return o
	}
	s.mu.Lock()
	idle := len(s.sessions) == 0
	s.mu.Unlock()
	if !idle {
		return o
	}
	if n := pruneLRU(dir, max(0, treeSize(dir)-o)); n > 0 {
		s.log.Info("pruned written back blobs for the disk budget", "freed", n)
	}
	return over()
}

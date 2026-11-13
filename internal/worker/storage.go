package worker

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/traumventures/offshore/internal/proto"
)

// storageMeasure holds the tree walks of the periodic measurement: sizes
// of machine and global cache directories and, per repository, the bytes
// of its committed head by access class and its largest directories.
type storageMeasure struct {
	mu      sync.Mutex
	dirs    map[string]int64 // cache name -> bytes (see cacheDirs)
	classes map[string]map[string]int64
	topDirs map[string][]proto.DirSize
	dormant []dormantRepo // repositories without an open stack
	// machineDirs are the sizes of machine cache directories with a
	// max_size (directory -> bytes).
	machineDirs map[string]int64
	// repos is the disk of each open repository's directory (pages,
	// layer objects), hard links (the page pool) counted once; pool the
	// pool's pages no tier links.
	repos map[string]int64
	pool  int64
}

// writebackItem is the storage item (and cacheDirs key) of the written
// back blobs.
const writebackItem = "written back files"

// topDirsN is the number of largest directories reported per repository.
const topDirsN = 8

// cacheDirs returns the directories of machine scope and global caches by
// name and kind.
func (s *Worker) cacheDirs() map[string][2]string {
	out := map[string][2]string{}
	if s.cfg.DepsDir != "" {
		if es, err := os.ReadDir(s.cfg.DepsDir); err == nil {
			for _, e := range es {
				if e.IsDir() {
					out[e.Name()] = [2]string{filepath.Join(s.cfg.DepsDir, e.Name()), proto.StorageMachineCache}
				}
			}
		}
	}
	if s.cfg.SccacheDir != "" {
		out["sccache"] = [2]string{s.cfg.SccacheDir, proto.StorageGlobal}
	}
	if d := s.writebackDir(); d != "" {
		out[writebackItem] = [2]string{d, proto.StorageWriteback}
	}
	if s.cfg.CacheDir != "" {
		for _, n := range []string{"ccache", "gocache"} {
			out[n] = [2]string{filepath.Join(s.cfg.CacheDir, n), proto.StorageGlobal}
		}
	}
	return out
}

// measureStorage walks the cache directories, the repository
// directories (dormant and open ones and the page pool, hard links
// counted once overall) and the committed heads (in memory) of the
// repositories, and refreshes the physical size of idle overlay slots;
// called with the machine cache measurement.
func (s *Worker) measureStorage(ctx context.Context) {
	dirs := map[string]int64{}
	for name, d := range s.cacheDirs() {
		if _, err := os.Stat(d[0]); err == nil {
			dirs[name] = treeSize(d[0])
		}
	}
	machine := map[string]int64{}
	s.machineBudgets.mu.Lock()
	for dir, lim := range s.machineBudgets.dirs {
		if lim > 0 {
			machine[dir] = 0
		}
	}
	s.machineBudgets.mu.Unlock()
	for dir := range machine {
		machine[dir] = treeSize(dir)
	}
	var dormant []dormantRepo
	classes := map[string]map[string]int64{}
	tops := map[string][]proto.DirSize{}
	repos := map[string]int64{}
	var pool int64
	if s.cfs != nil {
		var seen sync.Map
		// Open repositories first: a page a dormant one shares with
		// them is theirs (dropping the dormant one frees nothing).
		for _, r := range s.cfs.openRepos() {
			repos[r.id] = dirBytes(filepath.Join(s.cfs.dir, "repos", r.id), &seen)
		}
		dormant = s.cfs.dormantRepos(nil)
		for i := range dormant {
			dormant[i].bytes = dirBytes(dormant[i].dir, &seen)
		}
		pool = dirBytes(filepath.Join(s.cfs.dir, "pool"), &seen) // pages no tier links
		for repo, cl := range s.cfs.headBreakdown(ctx) {
			classes[repo], tops[repo] = cl.classes, cl.top
		}
		if s.cfs.mode != fsPlain {
			for _, r := range s.slots.list() {
				if r.Leased {
					continue
				}
				if n := slotUpperBytes(r.Path); n != r.Meta.Physical {
					s.slots.update(r.Path, func(m *slotMeta) { m.Physical = n })
				}
			}
		}
	}
	s.storage.mu.Lock()
	s.storage.dirs, s.storage.classes, s.storage.topDirs, s.storage.dormant = dirs, classes, tops, dormant
	s.storage.machineDirs, s.storage.repos, s.storage.pool = machine, repos, pool
	s.storage.mu.Unlock()
}

type headBreakdown struct {
	classes map[string]int64
	top     []proto.DirSize
}

// headBreakdown classifies the files of each repository's committed head
// by access class and sums its largest directories (three levels deep).
func (c *cacheFS) headBreakdown(ctx context.Context) map[string]headBreakdown {
	c.mu.Lock()
	type rh struct {
		r    *repoFS
		head string
	}
	var rs []rh
	for id, r := range c.repos {
		if h, ok := c.heads[id]; ok && h.Layer != "" {
			rs = append(rs, rh{r, h.Layer})
		}
	}
	c.mu.Unlock()
	out := map[string]headBreakdown{}
	for _, x := range rs {
		ch, err := x.r.chain(ctx, x.head)
		if err != nil || len(ch) == 0 {
			continue
		}
		b := headBreakdown{classes: map[string]int64{}}
		dirs := map[string]int64{}
		for _, n := range ch.Tree().Nodes() {
			if n.Size <= 0 {
				continue
			}
			if x.r.access != nil {
				b.classes[x.r.access.Classify(n.Path).Class.String()] += n.Size
			}
			parts := strings.Split(strings.Trim(n.Path, "/"), "/")
			if len(parts) > 1 {
				dirs[path.Join(parts[:min(3, len(parts)-1)]...)] += n.Size
			}
		}
		for p, n := range dirs {
			b.top = append(b.top, proto.DirSize{Path: p, Bytes: n})
		}
		sort.Slice(b.top, func(i, j int) bool { return b.top[i].Bytes > b.top[j].Bytes })
		if len(b.top) > topDirsN {
			b.top = b.top[:topDirsN]
		}
		out[x.r.id] = b
	}
	return out
}

// storageItems breaks the disk down (status, cheap: metadata, counters and
// the last measurement).
func (s *Worker) storageItems(used int64) []proto.StorageItem {
	var items []proto.StorageItem
	var sum int64
	heads := map[string]proto.LayerHead{}
	if s.cfs != nil {
		for _, h := range s.cfs.flushedHeads() {
			heads[h.Repo] = h
		}
	}
	for _, r := range s.slots.list() {
		h := heads[r.Repo]
		items = append(items, proto.StorageItem{Kind: proto.StorageSlot, Name: r.Path, Repo: r.Repo, Worktree: r.Meta.Worktree, Branch: r.Meta.Branch,
			Bytes: r.Meta.Bytes, LastUsed: r.Meta.LastUsed, Committed: r.Meta.Committed, StoreAt: h.At, Stored: h.Bytes, Leased: r.Leased})
		// The slot's own disk (Bytes is its logical, merged size).
		if r.Meta.Physical > 0 {
			sum += r.Meta.Physical
		} else {
			sum += r.Meta.UpperBytes
		}
	}
	s.storage.mu.Lock()
	classes, tops, dirs, dormant := s.storage.classes, s.storage.topDirs, s.storage.dirs, s.storage.dormant
	measured, pool := s.storage.repos, s.storage.pool
	s.storage.mu.Unlock()
	for _, d := range dormant {
		items = append(items, proto.StorageItem{Kind: proto.StorageDormant, Name: d.id, Repo: d.id, Bytes: d.bytes, LastUsed: d.used.Unix()})
		sum += d.bytes
	}
	if s.cfs != nil {
		for _, p := range s.cfs.repoPages() {
			h := heads[p.Repo]
			p.Stored, p.StoreAt, p.Classes, p.TopDirs = h.Bytes, h.At, classes[p.Repo], tops[p.Repo]
			if n, ok := measured[p.Repo]; ok {
				// The directory's disk (pages, layer objects; a page
				// shared through the pool counts once, here or in an
				// earlier repository): the counters miss pages every
				// tier marks shared after a restart.
				p.Bytes = n
			}
			items = append(items, p)
			sum += p.Bytes
		}
		if pool > 0 {
			items = append(items, proto.StorageItem{Kind: proto.StoragePages, Name: "page pool (unlinked pages)", Bytes: pool})
			sum += pool
		}
		f := s.cfs.files.Stats()
		items = append(items, proto.StorageItem{Kind: proto.StorageFiles, Name: "materialized files", Bytes: f.ResidentBytes, Resident: f.ResidentBytes})
		sum += f.ResidentBytes
	}
	if s.srcFiles != nil {
		n := s.srcFiles.Stats().ResidentBytes
		items = append(items, proto.StorageItem{Kind: proto.StorageSource, Name: "source contents", Bytes: n, Resident: n})
		sum += n
	}
	kinds := s.cacheDirs()
	for name, n := range dirs {
		items = append(items, proto.StorageItem{Kind: kinds[name][1], Name: name, Bytes: n})
		sum += n
	}
	if o := used - sum; o > 0 {
		items = append(items, proto.StorageItem{Kind: proto.StorageOther, Name: "images, system and unaccounted", Bytes: o})
	}
	return items
}

// repoPages reports each repository's local page tier.
func (c *cacheFS) repoPages() []proto.StorageItem {
	c.mu.Lock()
	repos := make([]*repoFS, 0, len(c.repos))
	for _, r := range c.repos {
		repos = append(repos, r)
	}
	c.mu.Unlock()
	var out []proto.StorageItem
	for _, r := range repos {
		st := r.local.Stats()
		it := proto.StorageItem{Kind: proto.StoragePages, Name: r.id, Repo: r.id, Bytes: st.ResidentBytes - st.SharedBytes,
			Resident: st.ResidentBytes - st.SharedBytes, Dirty: st.DirtyBytes}
		if r.ws != nil {
			it.WorkingSet = r.ws.bytes()
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

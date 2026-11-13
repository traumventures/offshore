package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/proto"
)

// Build slot pruning.
//
// Reads are tracked per session through atime: after a session the worker
// records, per file of the slot, the last run (session number of the slot)
// that read or wrote it, and resets the atime of files read in the run to
// their mtime. With relatime (the Linux default) the next read updates the
// atime again because it is not newer than the mtime. On noatime mounts
// only writes are tracked.
//
// Pruning (the in-slot trim, slottrim.go) runs in stages until the slot
// fits its target:
//  1. rust: old incremental compilation sessions (all but the newest per
//     crate);
//  2. units (rust: all files of one compilation unit, lru: single files)
//     not read for their policy's unread runs (garbage), the stalest
//     first; with a size target also more recently read ones, never those
//     of the latest run.
//
// A slot is never removed here: demotion of whole slots is the disk
// reclaim's (ledger.go).

const readsFile = "reads.json"

// Unread runs after which a unit is garbage: cargo units (rebuilt only
// after a branch switch) and single files of the lru policy.
const (
	rustUnreadRuns = 3
	lruUnreadRuns  = 5
)

// readIndex maps a file path relative to the slot data directory to the
// last run that read or wrote it.
type readIndex map[string]int64

func loadReads(slot string) readIndex {
	idx := readIndex{}
	if b, err := os.ReadFile(filepath.Join(slot, readsFile)); err == nil {
		json.Unmarshal(b, &idx)
	}
	return idx
}

func saveReads(slot string, idx readIndex) error {
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(slot, readsFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(slot, readsFile))
}

func fileTimes(fi os.FileInfo) (atime, mtime time.Time) {
	mtime = fi.ModTime()
	atime = mtime
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		atime = time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return atime, mtime
}

// recordRun scans the slot data after a session that started at start
// and was the slot's run number run. It updates idx (files that no longer
// exist are dropped) and returns the slot's size and its garbage: the
// bytes of units of caches (see unitOf) unread for their policy's runs.
// reads are the files the session read when tracked (nil: from atimes,
// re-armed for relatime).
func recordRun(data string, start time.Time, run int64, idx readIndex, reads map[string]bool, caches []slotCache) (size, garbage int64) {
	var mu sync.Mutex
	var seen sync.Map
	next := readIndex{}
	type unit struct{ last, bytes, runs int64 }
	units := map[string]*unit{}
	walkFiles(data, func(p string, fi os.FileInfo) {
		rel, err := filepath.Rel(data, p)
		if err != nil {
			return
		}
		n := allocated(fi, &seen)
		at, mt := fileTimes(fi)
		used := !mt.Before(start)
		if reads != nil {
			used = used || reads[filepath.ToSlash(rel)]
		} else {
			used = used || !at.Before(start)
			if at.After(mt) && fi.Mode().IsRegular() {
				os.Chtimes(p, mt, mt) // re-arm relatime for the next run
			}
		}
		key, _, runs, isUnit := unitOf(rel, caches)
		mu.Lock()
		defer mu.Unlock()
		size += n
		last, known := idx[rel]
		if used || !known {
			last = run
		}
		next[rel] = last
		if isUnit {
			u := units[key]
			if u == nil {
				u = &unit{runs: runs}
				units[key] = u
			}
			u.last = max(u.last, last)
			u.bytes += n
		}
	})
	for k := range idx {
		delete(idx, k)
	}
	for k, v := range next {
		idx[k] = v
	}
	for _, u := range units {
		if run-u.last >= u.runs {
			garbage += u.bytes
		}
	}
	return size, garbage
}

// effectiveCaches are a slot's caches for pruning (none recorded: the
// whole slot under the lru policy).
func effectiveCaches(m slotMeta) []slotCache {
	if len(m.Caches) == 0 {
		return []slotCache{{Path: ".", Prune: config.PruneLRU}}
	}
	return m.Caches
}

// unitOf returns the prune unit of a file (rel to the slot data
// directory) under the most specific cache containing it: its key, what
// removing it deletes (slash separated, relative to the data directory)
// and the unread runs after which it is garbage; ok=false for files of no
// cache, of a "none" cache, or of no cargo unit.
func unitOf(rel string, caches []slotCache) (key, root string, runs int64, ok bool) {
	rel = filepath.ToSlash(rel)
	best := -1
	for i, c := range caches {
		p := filepath.ToSlash(c.Path)
		if p == "." || rel == p || strings.HasPrefix(rel, p+"/") {
			if best < 0 || len(p) > len(filepath.ToSlash(caches[best].Path)) {
				best = i
			}
		}
	}
	if best < 0 {
		return "", "", 0, false
	}
	c := caches[best]
	p := filepath.ToSlash(c.Path)
	switch c.Prune {
	case config.PruneNone:
		return "", "", 0, false
	case config.PruneRust:
		in := rel
		if p != "." {
			in = strings.TrimPrefix(rel, p+"/")
		}
		k, r, ok := cargoUnitRoot(in)
		if !ok {
			return "", "", 0, false
		}
		if p != "." {
			r = p + "/" + r
		}
		return p + "\x00" + k, r, rustUnreadRuns, true
	}
	return rel, rel, lruUnreadRuns, true
}

// unitHash matches the metadata hash suffix of a cargo unit name.
var unitHash = regexp.MustCompile(`^(.+-[0-9a-f]{16})$`)

// cargoUnit returns the compilation unit of a file below a cargo target
// directory (rel is relative to the target directory): the profile
// directory and the unit name ("serde-0123456789abcdef"), or ok=false for
// files that belong to no unit. root is what removing the unit deletes for
// this file (the file, or its unit directory below .fingerprint or build).
func cargoUnit(rel string) (key string, ok bool) {
	key, _, ok = cargoUnitRoot(rel)
	return key, ok
}

func cargoUnitRoot(rel string) (key, root string, ok bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, p := range parts {
		if i+1 >= len(parts) {
			break
		}
		profile := strings.Join(parts[:i], "/")
		switch p {
		case "deps":
			if i+2 != len(parts) {
				return "", "", false
			}
			name := strings.TrimPrefix(parts[i+1], "lib")
			if j := strings.IndexByte(name, '.'); j >= 0 {
				name = name[:j]
			}
			if unitHash.MatchString(name) {
				return profile + "\x00" + name, rel, true
			}
			return "", "", false
		case ".fingerprint", "build":
			if unitHash.MatchString(parts[i+1]) {
				return profile + "\x00" + parts[i+1], strings.Join(parts[:i+2], "/"), true
			}
			return "", "", false
		case "incremental":
			return "", "", false
		}
	}
	return "", "", false
}

// pruneUnit is a group of files removed together.
type pruneUnit struct {
	last  int64
	runs  int64 // unread runs after which it is garbage
	bytes int64
	files []string        // relative to the slot data directory
	roots map[string]bool // what to delete (files or unit directories, slash separated)
}

// pruneResult describes what a prune removed.
type pruneResult struct {
	Freed     int64
	Units     int // stage 2 units removed
	Sessions  int // stage 1 incremental sessions removed
	BytesLeft int64
}

// pruneSlot prunes a slot (which the caller has acquired) to target bytes
// (0: the garbage prune, removing old incremental sessions and units
// unread for their policy's runs).
func pruneSlot(slot string, meta slotMeta, target int64) pruneResult {
	data := filepath.Join(slot, "data")
	size := treeSize(data)
	res := pruneResult{}
	fits := func() bool { return target > 0 && size <= target }
	if fits() {
		res.BytesLeft = size
		return res
	}
	idx := loadReads(slot)
	caches := effectiveCaches(meta)
	// Stage 1: old incremental sessions.
	for _, c := range caches {
		if c.Prune != config.PruneRust {
			continue
		}
		freed, n := pruneIncremental(filepath.Join(data, c.Path))
		size -= freed
		res.Freed += freed
		res.Sessions += n
	}
	if !fits() {
		// Stage 2: unread units.
		units := map[string]*pruneUnit{}
		var mu sync.Mutex
		var seen sync.Map
		walkFiles(data, func(p string, fi os.FileInfo) {
			rel, err := filepath.Rel(data, p)
			if err != nil {
				return
			}
			key, root, runs, ok := unitOf(rel, caches)
			if !ok {
				return
			}
			n := allocated(fi, &seen)
			mu.Lock()
			defer mu.Unlock()
			u := units[key]
			if u == nil {
				u = &pruneUnit{runs: runs, roots: map[string]bool{}}
				units[key] = u
			}
			u.last = max(u.last, idx[rel])
			u.bytes += n
			u.files = append(u.files, rel)
			u.roots[root] = true
		})
		list := make([]*pruneUnit, 0, len(units))
		for _, u := range units {
			list = append(list, u)
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].last != list[j].last {
				return list[i].last < list[j].last
			}
			return list[i].bytes > list[j].bytes
		})
		for _, u := range list {
			if target > 0 && fits() {
				break
			}
			stale := meta.Runs-u.last >= u.runs
			if target == 0 && !stale || target > 0 && u.last >= meta.Runs {
				continue
			}
			for r := range u.roots {
				os.RemoveAll(filepath.Join(data, filepath.FromSlash(r)))
			}
			for _, f := range u.files {
				delete(idx, f)
			}
			size -= u.bytes
			res.Freed += u.bytes
			res.Units++
		}
		saveReads(slot, idx)
	}
	if size < 0 {
		size = treeSize(data)
	}
	res.BytesLeft = size
	return res
}

// pruneIncremental removes all but the newest incremental compilation
// session of every crate below a cargo target directory.
func pruneIncremental(target string) (freed int64, removed int) {
	var dirs []string
	for _, pat := range []string{"*/incremental/*", "*/*/incremental/*"} {
		m, _ := filepath.Glob(filepath.Join(target, pat))
		dirs = append(dirs, m...)
	}
	for _, crate := range dirs {
		des, err := os.ReadDir(crate)
		if err != nil {
			continue
		}
		type sess struct {
			name string
			mod  time.Time
		}
		var ss []sess
		for _, de := range des {
			if de.IsDir() && strings.HasPrefix(de.Name(), "s-") {
				if fi, err := de.Info(); err == nil {
					ss = append(ss, sess{de.Name(), fi.ModTime()})
				}
			}
		}
		sort.Slice(ss, func(i, j int) bool { return ss[i].mod.After(ss[j].mod) })
		for _, s := range ss[min(1, len(ss)):] {
			p := filepath.Join(crate, s.name)
			freed += treeSize(p)
			os.RemoveAll(p)
			os.Remove(p + ".lock")
			removed++
		}
	}
	return freed, removed
}

// slotCaches returns the slot-scope caches of a request for the slot
// metadata.
func slotCaches(req proto.Exec) []slotCache {
	var out []slotCache
	for _, c := range req.Caches {
		if c.Scope != config.ScopeSlot {
			continue
		}
		for _, p := range c.Paths {
			out = append(out, slotCache{Path: filepath.Clean(p), Prune: c.Prune, MaxBytes: c.MaxBytes})
		}
	}
	return out
}

// recordSlotRun counts a finished session as the slot's next run, updates
// its read index, garbage, physical size and run times, and returns its
// size.
func (s *Worker) recordSlotRun(slot string, start time.Time, reads map[string]bool) int64 {
	var run int64
	var caches []slotCache
	s.slots.update(slot, func(m *slotMeta) {
		m.Runs++
		run, caches = m.Runs, effectiveCaches(*m)
		if m.FirstRun == 0 {
			m.FirstRun = start.Unix()
		}
		m.LastRun = start.Unix()
	})
	idx := loadReads(slot)
	size, garbage := recordRun(filepath.Join(slot, "data"), start, run, idx, reads, caches)
	if err := saveReads(slot, idx); err != nil {
		s.log.Warn("slot read index", "slot", slot, "err", err)
	}
	phys := size // a plain slot's data is its disk
	if s.cfs != nil && s.cfs.mode != fsPlain {
		phys = slotUpperBytes(slot)
	}
	s.slots.update(slot, func(m *slotMeta) {
		m.Garbage, m.Physical = garbage, phys
		if m.ColdBytes == 0 {
			m.ColdBytes = size
		}
	})
	return size
}

// slotUpperBytes is the disk an overlay slot holds alone: its upper
// directory and pending snapshots (the lower is the repository's page
// tier).
func slotUpperBytes(slot string) int64 {
	var n int64
	for _, d := range []string{"upper", snapsDir} {
		if _, err := os.Stat(filepath.Join(slot, d)); err == nil {
			n += treeSize(filepath.Join(slot, d))
		}
	}
	return n
}

// cachesBudget is the sum of the max_size of a slot's caches (0:
// unbounded, also when one cache is unbounded).
func cachesBudget(m slotMeta) int64 {
	var b int64
	for _, c := range m.Caches {
		if c.MaxBytes <= 0 {
			return 0
		}
		b += c.MaxBytes
	}
	return b
}

// budgetBytes applies a budget (zero: config.DefaultDiskBudget) to a file
// system of total bytes (0: unknown).
func budgetBytes(b config.DiskBudget, total int64) int64 {
	if b == (config.DiskBudget{}) {
		b, _ = config.ParseDiskBudget(config.DefaultDiskBudget)
	}
	return b.Of(total)
}

// lockCtx locks mu unless ctx ends first.
func lockCtx(ctx context.Context, mu *sync.Mutex) bool {
	if mu.TryLock() {
		return true
	}
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			if mu.TryLock() {
				return true
			}
		}
	}
}

// sourceCacheBytes is the limit of the fetched source contents on a cache
// file system of total bytes: cfg.SourceCache of the disk budget.
func sourceCacheBytes(cfg Config, total int64) int64 {
	sc := cfg.SourceCache
	if sc == (config.DiskBudget{}) {
		sc, _ = config.ParseSourceCache(config.DefaultSourceCache)
	}
	return sc.Of(budgetBytes(cfg.DiskBudget, total))
}

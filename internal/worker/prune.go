package worker

import (
	"encoding/json"
	"fmt"
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
// Pruning runs in stages until the slot fits its target:
//  1. rust: old incremental compilation sessions (all but the newest per
//     crate);
//  2. units (rust: all files of one compilation unit, lru: single files)
//     not read for unreadRuns runs, the stalest first; with a size target
//     also more recently read ones, never those of the latest run;
//  3. the whole slot (cold), only when allowed and still too large.

const readsFile = "reads.json"

// defaultUnreadRuns is how many runs a unit may stay unread before the
// policy prune (no size target) removes it.
const defaultUnreadRuns = 5

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
// exist are dropped) and returns the slot's size.
func recordRun(data string, start time.Time, run int64, idx readIndex) int64 {
	var mu sync.Mutex
	var seen sync.Map
	var total int64
	next := readIndex{}
	walkFiles(data, func(p string, fi os.FileInfo) {
		rel, err := filepath.Rel(data, p)
		if err != nil {
			return
		}
		n := allocated(fi, &seen)
		at, mt := fileTimes(fi)
		used := !at.Before(start) || !mt.Before(start)
		if at.After(mt) && fi.Mode().IsRegular() {
			os.Chtimes(p, mt, mt) // re-arm relatime for the next run
		}
		mu.Lock()
		defer mu.Unlock()
		total += n
		last, known := idx[rel]
		switch {
		case used || !known:
			next[rel] = run
		default:
			next[rel] = last
		}
	})
	for k := range idx {
		delete(idx, k)
	}
	for k, v := range next {
		idx[k] = v
	}
	return total
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
	bytes int64
	files []string        // relative to the slot data directory
	roots map[string]bool // what to delete (files or unit directories)
}

// pruneResult describes what a prune removed.
type pruneResult struct {
	Freed     int64
	Units     int  // stage 2 units removed
	Sessions  int  // stage 1 incremental sessions removed
	Cold      bool // stage 3: the slot was removed
	BytesLeft int64
}

// pruneSlot prunes a slot (which the caller has acquired) to target bytes
// (0: the policy prune, removing units unread for unreadRuns runs).
// allowCold permits removing the whole slot data in the last stage.
func pruneSlot(slot string, meta slotMeta, target int64, unreadRuns int64, allowCold bool) pruneResult {
	data := filepath.Join(slot, "data")
	size := treeSize(data)
	res := pruneResult{}
	fits := func() bool { return target > 0 && size <= target }
	if target > 0 && fits() {
		res.BytesLeft = size
		return res
	}
	idx := loadReads(slot)
	caches := meta.Caches
	if len(caches) == 0 {
		caches = []slotCache{{Path: ".", Prune: config.PruneLRU}}
	}
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
		for _, c := range caches {
			if c.Prune == config.PruneNone {
				continue
			}
			root := filepath.Join(data, c.Path)
			var mu sync.Mutex
			var seen sync.Map
			walkFiles(root, func(p string, fi os.FileInfo) {
				rel, _ := filepath.Rel(data, p)
				inCache, _ := filepath.Rel(root, p)
				key, root := rel, rel
				if c.Prune == config.PruneRust {
					k, r, ok := cargoUnitRoot(inCache)
					if !ok {
						return
					}
					key, root = c.Path+"\x00"+k, filepath.Join(c.Path, filepath.FromSlash(r))
				}
				mu.Lock()
				u := units[key]
				if u == nil {
					u = &pruneUnit{}
					units[key] = u
				}
				if l := idx[rel]; l > u.last {
					u.last = l
				}
				u.bytes += allocated(fi, &seen)
				u.files = append(u.files, rel)
				if u.roots == nil {
					u.roots = map[string]bool{}
				}
				u.roots[root] = true
				mu.Unlock()
			})
		}
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
			stale := meta.Runs-u.last >= unreadRuns
			if target == 0 && !stale {
				break
			}
			if target > 0 && (fits() || u.last >= meta.Runs) {
				break
			}
			for r := range u.roots {
				os.RemoveAll(filepath.Join(data, r))
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
	if target > 0 && !fits() && allowCold {
		// Stage 3: cold (the next session starts a new slot).
		os.RemoveAll(slot)
		res.Freed += size
		res.Cold = true
		size = 0
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
// its read index and returns its size.
func (s *Worker) recordSlotRun(slot string, start time.Time) int64 {
	var run int64
	s.slots.update(slot, func(m *slotMeta) { m.Runs++; run = m.Runs })
	idx := loadReads(slot)
	size := recordRun(filepath.Join(slot, "data"), start, run, idx)
	if err := saveReads(slot, idx); err != nil {
		s.log.Warn("slot read index", "slot", slot, "err", err)
	}
	return size
}

// slotBudget is the sum of the max_size of a slot's caches (0: unbounded,
// also when one cache is unbounded).
func slotBudget(m slotMeta) int64 {
	var b int64
	for _, c := range m.Caches {
		if c.MaxBytes <= 0 {
			return 0
		}
		b += c.MaxBytes
	}
	return b
}

// diskBudget returns the bytes the cache disk may use (0: unknown).
func (s *Worker) diskBudget() int64 {
	total, _, ok := fsUsage(s.cfg.CacheDir)
	if !ok {
		total = 0
	}
	return budgetBytes(s.cfg.DiskBudget, total)
}

// budgetBytes applies a budget (zero: config.DefaultDiskBudget) to a file
// system of total bytes (0: unknown).
func budgetBytes(b config.DiskBudget, total int64) int64 {
	if b == (config.DiskBudget{}) {
		b, _ = config.ParseDiskBudget(config.DefaultDiskBudget)
	}
	return b.Of(total)
}

// enforceBudgets prunes a released slot over its budget and then the
// machine's slots while the disk exceeds its budget.
func (s *Worker) enforceBudgets(slot string) {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	if m, err := readMeta(slot); err == nil {
		if b := slotBudget(m); b > 0 && m.Bytes > b && s.slots.acquire(slot) {
			r := pruneSlot(slot, m, b, defaultUnreadRuns, false)
			s.slots.releaseMaint(slot, r.BytesLeft)
			s.log.Info("pruned build slot over its budget", "slot", slot, "freed", r.Freed, "left", r.BytesLeft)
		}
	}
	s.enforceDiskBudget()
}

// enforceDiskBudget frees disk while it is above the machine budget: the
// policy prune of the least recently used slots first, then cold slots.
func (s *Worker) enforceDiskBudget() {
	budget := s.diskBudget()
	if budget <= 0 {
		return
	}
	over := func() int64 {
		_, used, _ := fsUsage(s.cfg.CacheDir)
		return used - budget
	}
	if over() <= 0 {
		return
	}
	refs := s.slots.list()
	sort.Slice(refs, func(i, j int) bool { return refs[i].Meta.LastUsed < refs[j].Meta.LastUsed })
	for _, cold := range []bool{false, true} {
		for _, r := range refs {
			if over() <= 0 {
				return
			}
			if r.Leased || !s.slots.acquire(r.Path) {
				continue
			}
			var res pruneResult
			if cold {
				res = coldSlot(r.Path)
			} else {
				res = pruneSlot(r.Path, r.Meta, 0, defaultUnreadRuns, false)
			}
			s.slots.releaseMaint(r.Path, res.BytesLeft)
			if res.Freed > 0 {
				s.log.Info("pruned build slot for the disk budget", "slot", r.Path, "cold", res.Cold, "freed", res.Freed)
			}
		}
	}
}

// pruneRepo shrinks the slots of a repository to target bytes in total
// (0: the policy prune of every slot): older slots go cold first, then
// the newest is pruned in stages. It returns the bytes freed.
func (s *Worker) pruneRepo(repo string, target int64) (int64, error) {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	var refs []slotRef
	for _, r := range s.slots.list() {
		if r.Repo == repo {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return 0, fmt.Errorf("no build slot for repository %s", repo)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Meta.LastUsed > refs[j].Meta.LastUsed })
	var total, freed int64
	for _, r := range refs {
		total += r.Meta.Bytes
	}
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		if r.Leased || !s.slots.acquire(r.Path) {
			continue
		}
		var res pruneResult
		switch {
		case target == 0:
			res = pruneSlot(r.Path, r.Meta, 0, defaultUnreadRuns, false)
		case total <= target:
		case i > 0:
			res = coldSlot(r.Path)
		default:
			res = pruneSlot(r.Path, r.Meta, max(1, target-(total-r.Meta.Bytes)), defaultUnreadRuns, true)
		}
		s.slots.releaseMaint(r.Path, res.BytesLeft)
		total -= res.Freed
		freed += res.Freed
	}
	return freed, nil
}

// coldSlot removes a slot (stage 3 directly).
func coldSlot(slot string) pruneResult {
	n := treeSize(filepath.Join(slot, "data"))
	os.RemoveAll(slot) // the next session starts a new slot
	return pruneResult{Freed: n, Cold: true}
}

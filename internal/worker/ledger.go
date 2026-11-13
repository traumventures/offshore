package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"syscall"
	"time"

	"github.com/traumventures/offshore/internal/proto"
)

// The disk ledger and the one reclaim order (docs/design-storage-v2.md).
//
// The worker reports the physical truth of its cache disk: statfs gives
// total and free; every class of data that can be freed is an entry with
// the bytes freeing it releases on the disk and a cost (build seconds
// expected to be lost: rebuild seconds x P(reuse)). Pinned is the rest of
// the used bytes (leased slots, data not yet in the store, images, the
// system and anything nobody accounts for), so a byte is never counted
// twice and nothing is "untracked": an entry that cannot be measured
// physically is left out (pinned), never estimated high.
//
// reclaim frees entries in ascending cost per byte until statfs reports
// the wanted free bytes. It runs in the background above the watermarks
// (free < 2 x reserve, down to 3 x reserve), synchronously on the 1 s
// guard tick while sessions run (free < reserve; the machine admits
// nothing new only if that fails) and on the scheduler's SlotOp reclaim
// before a placement (free bytes + reserve, bounded by reclaimWait).

const (
	// storeBandwidth is the refetch rate of demoted data (store or
	// page tier, bytes/s) for rebuild seconds; sourceBandwidth the
	// laptop's for source contents.
	storeBandwidth  = 100 << 20
	sourceBandwidth = 10 << 20
	// defaultReuseGap is a slot's gap between runs without history.
	defaultReuseGap = time.Hour
	// reserveFloorMin and reserveFloorShare bound the reserve's floor
	// from below; defaultSlotGrowth is a running session's forecast
	// growth when the scheduler sent none.
	reserveFloorMin   = 5 << 30
	reserveFloorShare = 0.03
	defaultSlotGrowth = 2 << 30
	// maxReclaimFlushes bounds the idle unflushed slots a non-urgent
	// reclaim flushes (and demotes) after the ledger is exhausted.
	maxReclaimFlushes = 2
)

// dropInterval rate-limits the reclaim's scan for unreferenced pages
// (it loads every live tree); trims and demotions drop right away.
const dropInterval = time.Minute

// reclaimWait bounds a reclaim before a placement (SlotOp), the wait for
// a running reclaim included.
var reclaimWait = 20 * time.Second

// diskStat returns the cache disk's total and free bytes (statfs).
func (s *Worker) diskStat() (total, free int64, ok bool) {
	if s.statfs != nil {
		return s.statfs()
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.CacheDir, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bfree) * st.Bsize, true
}

// reserveFloor is the headroom kept free without sessions: max(5 GiB,
// 3 % of the disk, the disk less disk_budget).
func (s *Worker) reserveFloor(total int64) int64 {
	f := max(int64(reserveFloorMin), int64(float64(total)*reserveFloorShare))
	if b := budgetBytes(s.cfg.DiskBudget, total); b > 0 && total-b > f {
		f = total - b
	}
	return f
}

// sessionLoad is what the ledger needs of the running sessions.
type sessionLoad struct {
	n      int   // running sessions
	growth int64 // their forecast slot growth
}

// loadLocked sums the running sessions (s.mu held): their forecast slot
// growth is Exec.SlotGrowth, defaultSlotGrowth when unknown.
func (s *Worker) loadLocked() sessionLoad {
	l := sessionLoad{n: len(s.sessions)}
	for _, se := range s.sessions {
		g := se.req.SlotGrowth
		if g <= 0 {
			g = defaultSlotGrowth
		}
		l.growth += g
	}
	return l
}

// load is loadLocked taking s.mu.
func (s *Worker) load() sessionLoad {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// reserve is the floor plus the forecast growth of the running sessions.
func (s *Worker) reserve(total int64) int64 { return s.reserveFloor(total) + s.load().growth }

// reuseP is the probability that data idle for idle is used again by a
// workload running every gap: gap / (gap + idle).
func reuseP(gap, idle time.Duration) float64 {
	if gap <= 0 {
		gap = defaultReuseGap
	}
	idle = max(0, idle)
	return float64(gap) / float64(gap+idle)
}

// meanGap is a slot's mean time between runs.
func (m slotMeta) meanGap() time.Duration {
	if m.Runs < 2 || m.LastRun <= m.FirstRun {
		return defaultReuseGap
	}
	return time.Duration(m.LastRun-m.FirstRun) * time.Second / time.Duration(m.Runs-1)
}

// rebuildSeconds is the time to fetch bytes again at bandwidth.
func rebuildSeconds(bytes int64, bandwidth float64) float64 { return float64(bytes) / bandwidth }

// reclaimEntry is a ledger entry with what reclaiming it needs.
type reclaimEntry struct {
	proto.Reclaimable
	slot  slotRef // slot, garbage
	dir   string  // dormant: repository id; machine: cache directory
	limit int64   // machine: the cache's max_size
}

func (e reclaimEntry) perByte() float64 { return e.Cost / float64(e.Bytes) }

// reclaimables lists the ledger entries in reclaim order (ascending cost
// per byte, larger first on ties). Counters and the last measurement
// only: cheap enough for every status.
func (s *Worker) reclaimables(now time.Time, load sessionLoad) []reclaimEntry {
	var out []reclaimEntry
	add := func(e reclaimEntry) {
		if e.Bytes > 0 {
			out = append(out, e)
		}
	}
	idle := load.n == 0
	refs := s.slots.list()
	// Per repository: the most recent slot (reuse) and the bytes of its
	// garbage not held by a demotable slot's upper (they are in its
	// page tier, if resident at all).
	latest := map[string]slotMeta{}
	withSlot := map[string]bool{}
	for _, r := range refs {
		withSlot[r.Repo] = true
		if m, ok := latest[r.Repo]; !ok || r.Meta.LastUsed > m.LastUsed {
			latest[r.Repo] = r.Meta
		}
	}
	repoP := func(repo string) float64 {
		m, ok := latest[repo]
		if !ok {
			return reuseP(defaultReuseGap, 0)
		}
		return reuseP(m.meanGap(), now.Sub(time.Unix(m.LastUsed, 0)))
	}
	// Garbage of an idle slot lives in its upper (counted with a
	// demotable slot, taken from it) or in the repository's page tier
	// (taken from its resident pages, if resident at all).
	type garbage struct {
		e            reclaimEntry
		upper, lower int64
	}
	var gs []garbage
	var uppers int64
	for _, r := range refs {
		phys := r.Meta.Physical
		if phys == 0 {
			phys = r.Meta.UpperBytes
		}
		uppers += phys
		if r.Leased {
			continue
		}
		slotBytes := int64(0)
		if s.demotable(r) {
			slotBytes = phys
		}
		g := r.Meta.Garbage
		inUpper := min(g, slotBytes)
		if g > 0 {
			gs = append(gs, garbage{e: reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimGarbage, Repo: r.Repo}, slot: r},
				upper: inUpper, lower: g - inUpper})
		}
		if b := slotBytes - inUpper; b > 0 {
			ws := r.Meta.Bytes
			if s.cfs != nil {
				if rp := s.cfs.openRepo(r.Repo); rp != nil && rp.ws != nil && rp.ws.bytes() > 0 {
					ws = rp.ws.bytes()
				}
			}
			p := reuseP(r.Meta.meanGap(), now.Sub(time.Unix(r.Meta.LastUsed, 0)))
			add(reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimSlot, Repo: r.Repo, Bytes: b,
				Cost: rebuildSeconds(ws, storeBandwidth) * p}, slot: r})
		}
	}
	resident := map[string]int64{}
	if s.cfs != nil {
		for _, rp := range s.cfs.openRepos() {
			st := rp.local.Stats()
			// Clean pages this tier alone holds (pool shared and cloned
			// pages free nothing while another holder lives).
			resident[rp.id] = max(0, st.ResidentBytes-st.SharedBytes-st.DirtyBytes-st.ClonedBytes)
		}
	}
	for _, g := range gs {
		lower := min(g.lower, resident[g.e.Repo])
		resident[g.e.Repo] -= lower
		g.e.Bytes = g.upper + lower
		add(g.e)
	}
	if s.cfs != nil {
		for id, b := range resident {
			add(reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimResident, Repo: id, Bytes: b,
				Cost: rebuildSeconds(b, storeBandwidth) * repoP(id)}})
		}
		f := s.cfs.files.Stats().ResidentBytes
		if s.cfs.reflink && s.cfs.mode != fsPlain {
			// Committed upper files are cloned into the file cache:
			// those extents are the slots' until they are demoted.
			f -= uppers
		}
		add(reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimResident, Bytes: f,
			Cost: rebuildSeconds(f, storeBandwidth) * reuseP(defaultReuseGap, 0)}})
	}
	s.storage.mu.Lock()
	dormant, dirs, machine := s.storage.dormant, s.storage.dirs, s.storage.machineDirs
	s.storage.mu.Unlock()
	for _, d := range dormant {
		if withSlot[d.id] {
			continue
		}
		add(reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimDormant, Repo: d.id, Bytes: d.bytes,
			Cost: rebuildSeconds(d.bytes, storeBandwidth) * reuseP(defaultReuseGap, now.Sub(d.used))}, dir: d.id})
	}
	if idle {
		// Written back blobs and machine caches are read by sessions:
		// reclaimed (and reported) only while none runs.
		if n := dirs[writebackItem]; n > 0 && s.writebackDir() != "" {
			add(reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimWriteback, Bytes: n,
				Cost: rebuildSeconds(n, sourceBandwidth) * 0.5}, dir: s.writebackDir()})
		}
		s.machineBudgets.mu.Lock()
		for dir, lim := range s.machineBudgets.dirs {
			if n := machine[dir] - lim; lim > 0 && n > 0 {
				add(reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimMachine, Bytes: n,
					Cost: rebuildSeconds(n, storeBandwidth) * 0.5}, dir: dir, limit: lim})
			}
		}
		s.machineBudgets.mu.Unlock()
	}
	if s.srcFiles != nil {
		n := s.srcFiles.Stats().ResidentBytes
		add(reclaimEntry{Reclaimable: proto.Reclaimable{Class: proto.ReclaimSource, Bytes: n,
			Cost: rebuildSeconds(n, sourceBandwidth) * 0.5}})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].perByte(), out[j].perByte(); a != b {
			return a < b
		}
		return out[i].Bytes > out[j].Bytes
	})
	return out
}

// demotable reports whether an idle slot may be removed in favour of the
// store head: it is committed, and the store head is its head or newer
// than its last use. A slot the store lacks is pinned until its flush.
func (s *Worker) demotable(r slotRef) bool {
	if r.Leased || s.cfs == nil || r.Meta.uncommitted() {
		return false
	}
	s.cfs.mu.Lock()
	h, ok := s.cfs.heads[r.Repo]
	s.cfs.mu.Unlock()
	if r.Meta.Head == "" {
		return true // empty: nothing to keep
	}
	if r.Meta.Restored != 0 && r.Meta.Head == r.Meta.Base {
		return true // restored from a store head, unchanged since
	}
	return ok && h.Layer != "" && (h.Layer == r.Meta.Head || h.At > r.Meta.LastUsed)
}

// ledger is the physical account of the cache disk and its entries.
func (s *Worker) ledger() (*proto.DiskLedger, []reclaimEntry) { return s.ledgerWith(s.load()) }

// ledgerWith is ledger with the running sessions' load (for callers
// holding s.mu).
func (s *Worker) ledgerWith(load sessionLoad) (*proto.DiskLedger, []reclaimEntry) {
	total, free, ok := s.diskStat()
	if !ok {
		return nil, nil
	}
	es := s.reclaimables(s.cfg.now(), load)
	used := max(0, total-free)
	l := &proto.DiskLedger{Total: total, Free: free, Reserve: s.reserveFloor(total) + load.growth}
	var sum int64
	for i, e := range es {
		if sum+e.Bytes > used {
			// Never claim more than the disk holds (a measurement
			// lagging a deletion): the tail goes.
			e.Bytes = used - sum
			es = es[:i+1]
			es[i] = e
			if e.Bytes <= 0 {
				es = es[:i]
			}
			sum = used
			break
		}
		sum += e.Bytes
	}
	for _, e := range es {
		l.Reclaim = append(l.Reclaim, e.Reclaimable)
	}
	l.Pinned = used - sum
	return l, es
}

// reclaim frees physical bytes in ledger order until statfs reports want
// free bytes, keeping the slots of keepRepo (the repository a placement
// makes room for). When the ledger is exhausted it flushes idle slots
// the store lacks and demotes them: at most maxReclaimFlushes unless
// urgent. It returns the bytes statfs says it freed and an error when it
// stayed short. The caller holds reclaimMu.
func (s *Worker) reclaim(ctx context.Context, want int64, keepRepo string, urgent bool) (int64, error) {
	_, start, ok := s.diskStat()
	if !ok {
		return 0, errors.New("reclaim: statfs of the cache disk failed")
	}
	free := start
	if free >= want {
		return 0, nil
	}
	_, es := s.ledger()
	dropped := false
	for _, e := range es {
		if free >= want || ctx.Err() != nil {
			break
		}
		if e.Class == proto.ReclaimSlot && e.Repo == keepRepo {
			continue
		}
		if e.Class == proto.ReclaimResident && !dropped && s.cfs != nil && time.Since(s.dropAt) >= dropInterval {
			// Unreferenced pages and files first: no warmth lost.
			dropped, s.dropAt = true, time.Now()
			if n := s.cfs.dropUnreferenced(ctx, s.liveLayerMap()); n > 0 {
				s.log.Info("dropped unreferenced pages and files", "bytes", n)
			}
			if _, free, _ = s.diskStat(); free >= want {
				break
			}
		}
		s.reclaimEntry(ctx, e, want-free)
		_, free, _ = s.diskStat()
	}
	if free < want && ctx.Err() == nil && s.cfs != nil {
		refs := s.slots.list()
		sort.Slice(refs, func(i, j int) bool { return refs[i].Meta.LastUsed < refs[j].Meta.LastUsed })
		n := 0
		for _, r := range refs {
			if free >= want || ctx.Err() != nil || !urgent && n >= maxReclaimFlushes {
				break
			}
			if r.Leased || r.Repo == keepRepo || s.demotable(r) || s.connectStore(r.Repo, nil) != nil {
				continue
			}
			n++
			if _, err := s.writebackSlot(ctx, r, "reclaim"); err != nil {
				s.log.Warn("flush build slot for the disk", "slot", r.Path, "err", err)
				continue
			}
			if m, err := readMeta(r.Path); err == nil {
				r.Meta = m
				s.demoteSlot(ctx, r)
			}
			_, free, _ = s.diskStat()
		}
	}
	freed := max(0, free-start)
	if free < want {
		return freed, fmt.Errorf("reclaim: %d bytes free, %d wanted", free, want)
	}
	return freed, nil
}

// reclaimEntry frees (up to need bytes of) one ledger entry.
func (s *Worker) reclaimEntry(ctx context.Context, e reclaimEntry, need int64) {
	switch e.Class {
	case proto.ReclaimGarbage:
		s.trimSlot(ctx, e.slot.Path, e.slot.Repo, true)
	case proto.ReclaimSlot:
		s.demoteSlot(ctx, e.slot)
	case proto.ReclaimResident:
		if e.Repo == "" {
			res := s.cfs.files.Stats().ResidentBytes
			s.cfs.files.Reclaim(max(0, res-need))
		} else if rp := s.cfs.openRepo(e.Repo); rp != nil {
			res := rp.local.Stats().ResidentBytes
			rp.local.Reclaim(ctx, max(0, res-need))
		}
	case proto.ReclaimDormant:
		withSlot := false
		for _, r := range s.slots.list() {
			withSlot = withSlot || r.Repo == e.dir
		}
		if !withSlot && s.cfs.dropRepo(e.dir) {
			freed := s.cfs.sweepPool()
			s.log.Info("dropped the local tier of a repository without build slots", "repo", e.dir, "pool_freed", freed)
			s.forgetDormant(e.dir)
		}
	case proto.ReclaimWriteback:
		if s.idle() {
			n := pruneLRU(e.dir, max(0, treeSize(e.dir)-need))
			s.log.Info("pruned written back blobs for the disk", "freed", n)
		}
	case proto.ReclaimMachine:
		if s.idle() {
			n := pruneLRU(e.dir, e.limit)
			s.log.Info("pruned machine cache over its max_size", "dir", e.dir, "freed", n)
		}
	case proto.ReclaimSource:
		res := s.srcFiles.Stats().ResidentBytes
		s.srcFiles.Reclaim(max(0, res-need))
	}
}

// idle reports whether no session runs.
func (s *Worker) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions) == 0
}

// demoteSlot removes an idle slot whose state the store head holds
// (demotable); its exclusive pages become unreferenced and are dropped.
func (s *Worker) demoteSlot(ctx context.Context, r slotRef) bool {
	if !s.slots.acquire(r.Path) {
		return false
	}
	m, err := readMeta(r.Path)
	if err != nil {
		s.slots.releaseMaint(r.Path, r.Meta.Bytes)
		return false
	}
	r.Meta, r.Leased = m, false
	if !s.demotable(r) {
		s.slots.releaseMaint(r.Path, m.Bytes)
		return false
	}
	s.slots.removeDir(r.Path)
	s.slots.releaseMaint(r.Path, 0) // the metadata is gone: only the lease ends
	s.log.Info("demoted build slot to its store head", "slot", r.Path, "bytes", m.Physical, "head", m.Head)
	if s.cfs != nil {
		s.cfs.dropUnreferenced(ctx, s.liveLayerMap())
	}
	return true
}

// liveLayerMap are the layers the slots of every repository mount or
// start from (repository id -> layer ids).
func (s *Worker) liveLayerMap() map[string][]string {
	out := map[string][]string{}
	for _, r := range s.slots.list() {
		out[r.Repo] = append(out[r.Repo], r.Meta.Base, r.Meta.Head)
	}
	return out
}

// reclaimFor is the SlotOp reclaim of a placement: FreeBytes free beyond
// the reserve, keeping the repository's slots, within reclaimWait (the
// wait for a running reclaim included).
func (s *Worker) reclaimFor(ctx context.Context, repo string, bytes int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, reclaimWait)
	defer cancel()
	if !lockCtx(ctx, &s.reclaimMu) {
		return 0, fmt.Errorf("reclaim for %s: %w (another reclaim is running)", repo, ctx.Err())
	}
	defer s.reclaimMu.Unlock()
	total, _, ok := s.diskStat()
	if !ok {
		return 0, errors.New("reclaim: statfs of the cache disk failed")
	}
	return s.reclaim(ctx, bytes+s.reserve(total), repo, false)
}

// backgroundReclaim is the lifecycle tick's watermark reclaim: below
// 2 x reserve free it frees up to 3 x reserve. It skips the tick while
// another reclaim runs.
func (s *Worker) backgroundReclaim() {
	if !s.reclaimMu.TryLock() {
		return
	}
	defer s.reclaimMu.Unlock()
	if s.cfs != nil {
		if n := s.cfs.enforceCapacity(context.Background()); n > 0 {
			s.log.Info("reclaimed resident slot data above the cache capacity", "freed", n)
		}
	}
	total, free, ok := s.diskStat()
	if !ok {
		return
	}
	r := s.reserve(total)
	if free >= 2*r {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	freed, err := s.reclaim(ctx, 3*r, "", false)
	lvl := slog.LevelDebug
	if freed > 0 {
		lvl = slog.LevelInfo
	}
	s.log.Log(ctx, lvl, "background disk reclaim", "free", free, "reserve", r, "freed", freed, "err", err)
}

// guardInterval is the ENOSPC guard's tick while sessions run.
const guardInterval = time.Second

// diskGuard runs the ENOSPC guard every guardInterval until ctx ends.
func (s *Worker) diskGuard(ctx context.Context) {
	t := time.NewTicker(guardInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.guardTick(ctx)
		}
	}
}

// guardTick: while sessions run and free < reserve, reclaim
// synchronously (urgent: idle slots are flushed and demoted too) up to
// 2 x reserve; when that fails the machine admits no session (reported
// as draining, "disk full") until free is back at 1.5 x reserve.
func (s *Worker) guardTick(ctx context.Context) {
	s.mu.Lock()
	running, full := len(s.sessions) > 0, s.diskFull
	s.mu.Unlock()
	if !running && !full {
		return
	}
	total, free, ok := s.diskStat()
	if !ok {
		return
	}
	r := s.reserve(total)
	if free >= r {
		if full && free >= r+r/2 {
			s.setDiskFull(false, free, r)
		}
		return
	}
	if !running {
		return // the background reclaim frees the disk of an idle machine
	}
	if !s.reclaimMu.TryLock() {
		// A reclaim is running and has not made the reserve yet.
		s.setDiskFull(true, free, r)
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	freed, err := s.reclaim(rctx, 2*r, "", true)
	cancel()
	s.reclaimMu.Unlock()
	_, free, _ = s.diskStat()
	s.log.Warn("disk below its reserve: urgent reclaim", "free", free, "reserve", r, "freed", freed, "err", err)
	if free < r {
		s.setDiskFull(true, free, r)
	}
}

// setDiskFull updates the emergency state (no admissions).
func (s *Worker) setDiskFull(full bool, free, reserve int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diskFull == full {
		return
	}
	s.diskFull = full
	if full {
		s.log.Warn("cache disk below its reserve: admitting no sessions", "free", free, "reserve", reserve)
	} else {
		s.log.Info("cache disk above its reserve: admitting sessions again", "free", free, "reserve", reserve)
	}
}

// forgetDormant removes a dropped repository from the last measurement.
func (s *Worker) forgetDormant(id string) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	kept := s.storage.dormant[:0:0]
	for _, d := range s.storage.dormant {
		if d.id != id {
			kept = append(kept, d)
		}
	}
	s.storage.dormant = kept
}

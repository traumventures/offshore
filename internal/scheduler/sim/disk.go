package sim

import (
	"math"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

// The physical disk model (docs/design-storage-v2.md). A machine's disk
// holds otherBytes (system, images, global caches) and its build slots;
// a running build grows its slot linearly over the run. Of a warm build's
// growth, garbageShare becomes stale build units (superseded outputs a
// trim removes without losing warmth); a slot's dirty bytes reach the
// store after flushLatency plus the bytes at the commit throughput
// (calibrated on production, Nov 11-12: task end to flushed head p50 45 s,
// p90 6.7 min for growth p50 0.64 GB, p90 5.5 GB).
//
// Two worker models: StorageV1 is the worker until storage v2
// (manageDisk: above the disk budget, 90 %, idle slots least recently
// used are policy-pruned, then dropped; the scheduler's prune shrinks the
// placed repository's own slot to the requested bytes; 97 % stops
// admissions). StorageV2 keeps a reserve free (a floor plus the forecast
// growth of the running builds), trims each slot to its budget after a
// build, demotes idle flushed slots in ascending cost per byte and
// reports the ledger the scheduler plans with.

// StorageModel selects the simulated worker's disk management.
type StorageModel int

const (
	StorageV1 StorageModel = iota
	StorageV2
)

const (
	otherBytes      = 18 << 30 // a fresh machine's used disk (prod: 17.5 GB on 196-215 GB)
	garbageShare    = 0.8
	flushLatency    = 10 * time.Second
	commitBandwidth = 15e6 // bytes/s
	v1PruneShare    = 0.6  // garbage units unread for 5 runs
	v2TrimShare     = 0.8  // garbage units unread for 3 runs
	warmStartSlack  = 0.15 // a restore within 15 % of the warm run counts warm
)

// buildSlot is a build slot on a machine.
type buildSlot struct {
	bytes   int64 // on disk (live + garbage)
	garbage int64
	base    int64     // size after its first (cold) build: the budget's base
	flushAt time.Time // its last build is in the store from then on
	first   time.Time
	used    time.Time
	runs    int
}

func (sl *buildSlot) live() int64 { return sl.bytes - sl.garbage }

// flushed reports whether the slot's state is in the store at now.
func (sl *buildSlot) flushed(now time.Time) bool { return !now.Before(sl.flushAt) }

// trim removes share of the slot's garbage and returns the bytes freed.
func (sl *buildSlot) trim(share float64) int64 {
	n := int64(float64(sl.garbage) * share)
	sl.garbage -= n
	sl.bytes -= n
	return n
}

// gap is the mean time between the slot's runs (1 h without history).
func (sl *buildSlot) gap() time.Duration {
	if sl.runs < 2 {
		return time.Hour
	}
	return sl.used.Sub(sl.first) / time.Duration(sl.runs-1)
}

// growth is the bytes a build adds to its slot over the run.
func (r *run) grown() int64 {
	if r.total <= 0 {
		return r.growth
	}
	f := 1 - r.work/r.total
	return int64(float64(r.growth) * math.Max(0, math.Min(1, f)))
}

// used is m's physical disk usage now.
func (s *sim) used(m *machine) int64 {
	n := other(m)
	for _, sl := range m.slots {
		n += sl.bytes
	}
	for _, r := range m.runs {
		n += r.grown()
	}
	return n
}

// other is the disk m uses outside its slots: otherBytes, at most a
// tenth of small (test) disks.
func other(m *machine) int64 {
	if m.disk > 0 {
		return min(otherBytes, m.disk/10)
	}
	return otherBytes
}

// leased reports whether a build of repo runs on m.
func leased(m *machine, repo string) bool {
	for _, r := range m.runs {
		if r.j.Repo == repo {
			return true
		}
	}
	return false
}

// endBuild updates r's slot on m after the build and returns its size.
func (s *sim) endBuild(m *machine, r *run) int64 {
	j := r.j
	sl := m.slots[j.Repo]
	if sl == nil || !(r.warm || r.restored) {
		sl = &buildSlot{first: s.now}
		m.slots[j.Repo] = sl
	}
	if r.warm || r.restored {
		g := r.growth
		sl.garbage += int64(float64(g) * garbageShare)
		sl.bytes += g
	} else {
		sl.bytes, sl.garbage, sl.base = r.growth, 0, r.growth
	}
	if sl.base == 0 {
		sl.base = sl.bytes
	}
	sl.used = s.now
	sl.runs++
	lag := flushLatency + time.Duration(float64(max(r.growth, 0))/commitBandwidth*float64(time.Second))
	sl.flushAt = s.now.Add(lag)
	s.lags = append(s.lags, lag)
	if s.o.Storage == StorageV2 {
		if b := s.slotBudget(m, sl); sl.bytes > b*3/4 {
			s.r.TrimmedBytes += sl.trim(v2TrimShare)
		}
	}
	return sl.bytes
}

// slotBudget is a v2 slot's budget: twice its cold size, 10 GB to 40 %
// of the disk.
func (s *sim) slotBudget(m *machine, sl *buildSlot) int64 {
	b := max(2*sl.base, 10<<30)
	if m.disk > 0 {
		b = min(b, m.disk*2/5)
	}
	return b
}

// reserve is the v2 headroom: max(5 GB, 10 % of the disk: the default
// disk_budget of 90 %) plus the remaining forecast growth of the running
// builds.
func (s *sim) reserve(m *machine) int64 {
	n := max(int64(5<<30), m.disk/10)
	for _, r := range m.runs {
		n += max(r.forecast-r.grown(), 0)
	}
	return n
}

// reclaimable is one v2 ledger entry.
type reclaimable struct {
	repo  string
	slot  bool // demote the whole (flushed, idle) slot; else trim garbage
	bytes int64
	cost  float64 // build seconds expected to be lost
}

// ledger lists what v2 can free on m in reclaim order. urgent includes
// idle slots not yet flushed (an urgent flush first).
func (s *sim) ledger(m *machine, keep string, urgent bool) []reclaimable {
	var out []reclaimable
	repos := make([]string, 0, len(m.slots))
	for repo := range m.slots {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		sl := m.slots[repo]
		if leased(m, repo) {
			continue
		}
		if n := int64(float64(sl.garbage) * v2TrimShare); n > 0 {
			out = append(out, reclaimable{repo: repo, bytes: n})
		}
		if repo == keep || !(urgent || sl.flushed(s.now)) {
			continue
		}
		idle := s.now.Sub(sl.used)
		reuse := sl.gap().Seconds() / (sl.gap().Seconds() + idle.Seconds())
		ws := sl.live() / 2
		restore := scheduler.LazyRestore(m.size.Stack, sl.live(), ws, s.o.Bandwidth).Seconds()
		out = append(out, reclaimable{repo: repo, slot: true, bytes: sl.bytes - int64(float64(sl.garbage)*v2TrimShare), cost: restore * reuse})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].cost/float64(max(out[i].bytes, 1)) < out[j].cost/float64(max(out[j].bytes, 1))
	})
	return out
}

// reclaim frees v2 entries on m until want bytes are free; it returns the
// expected build seconds lost.
func (s *sim) reclaim(m *machine, want int64, keep string, urgent bool) float64 {
	lost := 0.0
	for _, e := range s.ledger(m, keep, urgent) {
		if m.disk-s.used(m) >= want {
			break
		}
		sl := m.slots[e.repo]
		if sl == nil {
			continue
		}
		if !e.slot {
			s.r.TrimmedBytes += sl.trim(v2TrimShare)
			continue
		}
		if !sl.flushed(s.now) { // urgent flush: in the store now
			s.flush(m, e.repo)
		}
		delete(m.slots, e.repo)
		delete(m.warm, e.repo)
		s.r.Demoted++
		lost += e.cost
	}
	return lost
}

// flush makes repo's slot on m the store head now.
func (s *sim) flush(m *machine, repo string) {
	sl := m.slots[repo]
	sl.flushAt = s.now
	s.snapshotNow(m, repo, sl.bytes)
}

// manageDisk is the worker's background disk management after a build
// and on every tick.
func (s *sim) manageDisk(m *machine) {
	if m.disk <= 0 {
		return
	}
	if s.o.Storage == StorageV2 {
		res := s.reserve(m)
		if free := m.disk - s.used(m); free < 2*res {
			s.reclaim(m, 3*res, "", false)
		}
		m.diskFull = m.disk-s.used(m) < res/2
		return
	}
	budget := m.disk * 9 / 10
	low := budget * 85 / 100
	switch share := float64(s.used(m)) / float64(m.disk); {
	case !m.diskFull && share >= 0.97:
		m.diskFull = true
	case m.diskFull && share < 0.95:
		m.diskFull = false
	}
	if s.used(m) <= budget {
		return
	}
	type ref struct {
		repo string
		sl   *buildSlot
	}
	var refs []ref
	for repo, sl := range m.slots {
		if !leased(m, repo) {
			refs = append(refs, ref{repo, sl})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if !refs[i].sl.used.Equal(refs[j].sl.used) {
			return refs[i].sl.used.Before(refs[j].sl.used)
		}
		return refs[i].repo < refs[j].repo
	})
	for _, cold := range []bool{false, true} {
		for _, r := range refs {
			if s.used(m) <= low {
				return
			}
			if cold {
				if _, ok := m.slots[r.repo]; ok {
					delete(m.slots, r.repo)
					delete(m.warm, r.repo)
					s.r.Evictions++
				}
			} else {
				s.r.TrimmedBytes += r.sl.trim(v1PruneShare)
			}
		}
	}
}

// pruneV1 is the v1 worker's prune before a placement: the placed
// repository's own slot is pruned to target bytes (garbage first, then
// live state; losing more than a third of the live state makes it cold).
func (s *sim) pruneV1(m *machine, repo string, target int64) {
	sl := m.slots[repo]
	if sl == nil || sl.bytes <= target {
		return
	}
	over := sl.bytes - target
	g := min(over, sl.garbage)
	sl.garbage -= g
	sl.bytes -= g
	over -= g
	if over <= 0 {
		return
	}
	if over*3 > sl.live() {
		delete(m.slots, repo)
		delete(m.warm, repo)
		return
	}
	sl.bytes -= over
}

// guard runs after time advanced: v2 reclaims synchronously below the
// reserve; a disk that is still over full fails the most recently
// started build (ENOSPC), which is retried.
func (s *sim) guard(m *machine) {
	if m.disk <= 0 || len(m.runs) == 0 {
		return
	}
	if s.o.Storage == StorageV2 {
		if res := s.reserve(m); m.disk-s.used(m) < res {
			s.reclaim(m, res, "", true)
		}
	}
	for s.used(m) > m.disk && len(m.runs) > 0 {
		k := len(m.runs) - 1
		r := m.runs[k]
		m.runs = m.runs[:k]
		s.r.ENOSPC++
		s.r.DiskWaits++
		s.r.DiskWaitTime += s.now.Sub(r.start)
		if r.j.attempts <= s.o.Retries {
			r.j.since = s.now
			s.enqueue(r.j)
		}
		if len(m.runs) == 0 {
			m.idle = s.now
		}
	}
}

// ledgerOf is the v2 worker's report of m's disk.
func (s *sim) ledgerOf(m *machine) *scheduler.DiskLedger {
	used := s.used(m)
	l := &scheduler.DiskLedger{Total: m.disk, Free: m.disk - used, Reserve: s.reserve(m)}
	var rec int64
	for _, e := range s.ledger(m, "", false) {
		c := scheduler.ReclaimGarbage
		if e.slot {
			c = scheduler.ReclaimSlot
		}
		l.Reclaim = append(l.Reclaim, scheduler.Reclaimable{Class: c, Repo: e.repo, Bytes: e.bytes, Cost: e.cost})
		rec += e.bytes
	}
	l.Pinned = max(used-rec, 0)
	return l
}

// slotBytes are m's slot sizes (the policy's warmth and size map).
func slotBytes(m *machine) map[string]int64 {
	out := map[string]int64{}
	for repo, sl := range m.slots {
		out[repo] = sl.bytes
	}
	return out
}

// percentile of sorted ds.
func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	return ds[min(len(ds)-1, int(math.Ceil(q*float64(len(ds))))-1)]
}

// forecast is the growth the orchestrator forecasts for j's build (the
// learning store's slot growth, doubled for a p90; 2 GB unknown).
func (s *sim) forecast(j *job) int64 {
	if g := s.task(j).Profile.SlotGrowth; g > 0 {
		return 2 * g
	}
	return 2 << 30
}

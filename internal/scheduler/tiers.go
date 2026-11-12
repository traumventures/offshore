package scheduler

import (
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// Tier stacks: a size's caches live in a stack of tiers (anyfs), e.g. the
// local disk over a cache volume over the object store. The scheduler
// reads three things from the composite profile:
//
//   - lazy restore: a slot restored or moved from the store is mounted
//     lazily; only the working set of a build must be faulted in before
//     (and while) it runs, the rest pages in in the background. The wait
//     is Profile.FaultTime(working set), bounded by the store bandwidth.
//   - thrashing: when the working set of the builds on a machine exceeds
//     its local capacity (the top tier), reclaimed pages fault back in
//     during the build: the refetch factor times the excess is faulted
//     in again from the tier below. The factor is learned per build key
//     from the sessions' refaulted bytes (Profile.Refetch), else
//     calibrated from the cache file systems' page_in/reclaim/thrash
//     events per provider and globally (Pool.Refetch, see
//     learning.Refetch), else DefaultRefetch.
//     Learned run times exclude the thrash time the runs observed
//     (Usage.ThrashSeconds, see profileOf): the penalty predicted for
//     the candidate machine is added once.
//     A machine reporting VM.Thrashing (or a working set
//     larger than its capacity) gets this penalty for every placement,
//     and the cost model weighs a disk resize or a size with more local
//     capacity against it.
//   - where the slot persists (its volume, else the top tier): storage
//     price, attach time and resize cooldown.

// DefaultRefetch is how often the part of a working set beyond the local
// capacity is expected to fault in again during one build when no
// calibration is known (refaulted bytes per byte of excess).
const DefaultRefetch = 2.0

// RefetchOf is the refetch factor for machines of provider prov: the
// calibrated one of the provider, else of all providers (key ""), else
// DefaultRefetch.
func (f Pool) RefetchOf(prov string) float64 {
	if r := f.Refetch[prov]; r > 0 {
		return r
	}
	if r := f.Refetch[""]; r > 0 {
		return r
	}
	return DefaultRefetch
}

// refetchOf is the refetch factor of t on machines of provider prov:
// learned for t's key, else the provider's or global calibration.
func refetchOf(f Pool, prov string, t Task) float64 {
	if r := t.Profile.Refetch; r > 0 {
		return r
	}
	return f.RefetchOf(prov)
}

// levelOf returns the profile of medium med in a stack (ok false: none).
func levelOf(p medium.Stack, med string) (medium.Profile, bool) {
	for _, l := range p.Levels {
		if l.Medium == med {
			return l, true
		}
	}
	return medium.Profile{}, false
}

// SlotMedium is the medium a slot persists on: the stack's volume if it
// has one, else its top tier.
func SlotMedium(p medium.Stack) medium.Profile {
	if l, ok := levelOf(p, medium.Volume); ok {
		return l
	}
	return p.Top()
}

// WithTop returns p with its top tier replaced by top (added when p is
// empty) holding capacity bytes (e.g. a size whose local disk turns out
// to be an instance-store NVMe disk).
func WithTop(p medium.Stack, top medium.Profile, capacity int64) medium.Stack {
	levels := append([]medium.Profile{top}, p.Levels...)
	if len(p.Levels) > 0 {
		levels = append([]medium.Profile{top}, p.Levels[1:]...)
	}
	out := medium.Stack{Levels: levels, Capacity: capacity}
	out.Name = out.String()
	return out
}

// StackOf returns a stack profile of media top first (tests, defaults).
func StackOf(capacity int64, levels ...medium.Profile) medium.Stack {
	p := medium.Stack{Levels: levels, Capacity: capacity}
	p.Name = p.String()
	return p
}

// pageInBytes is what a build must fault in before it proceeds when a
// slot of bytes is mounted lazily: its working set (the whole slot when
// unknown).
func pageInBytes(bytes, ws int64) int64 {
	if ws > 0 && ws < bytes {
		return ws
	}
	return bytes
}

// LazyRestore is the time until a build can proceed on a slot of bytes
// restored from a store with bandwidth bw into stack: the working set
// faulted in (per page latency and transfer of the stack), at least the
// transfer at bw.
func LazyRestore(stack medium.Stack, bytes, ws, bw int64) time.Duration {
	n := pageInBytes(bytes, ws)
	if n <= 0 {
		return 0
	}
	d := time.Duration(0)
	if bw > 0 {
		d = time.Duration(float64(n) / float64(bw) * float64(time.Second))
	}
	return max(d, stack.FaultTime(n))
}

// ThrashTime is the extra build time when a working set of ws bytes runs
// on a stack with local capacity capacity: refetch (<= 0: DefaultRefetch)
// times the excess faulted in again from the tier below the top (0 when
// it fits or is unknown).
func ThrashTime(stack medium.Stack, capacity, ws int64, refetch float64) time.Duration {
	if capacity <= 0 || ws <= capacity {
		return 0
	}
	if refetch <= 0 {
		refetch = DefaultRefetch
	}
	return time.Duration(refetch * float64(RefaultTime(stack, ws-capacity)))
}

// RefaultTime is the time to fault bytes in again from the tier below
// the top of stack (DefaultBandwidth without a stack): the build time a
// session's refaulted bytes cost (Usage.ThrashSeconds).
func RefaultTime(stack medium.Stack, bytes int64) time.Duration {
	if bytes <= 0 {
		return 0
	}
	below := stack
	if len(stack.Levels) >= 2 {
		below = medium.Stack{Levels: stack.Levels[:2]}
	}
	d := below.FaultTime(bytes)
	if d <= 0 {
		d = time.Duration(float64(bytes) / float64(DefaultBandwidth) * float64(time.Second))
	}
	return d
}

// RefetchBytes is what a build with working set ws refaults on a local
// tier of capacity bytes (0 when it fits).
func RefetchBytes(capacity, ws int64, refetch float64) int64 {
	if capacity <= 0 || ws <= capacity {
		return 0
	}
	if refetch <= 0 {
		refetch = DefaultRefetch
	}
	return int64(refetch * float64(ws-capacity))
}

// localCapacity is the bytes m's top tier may hold: reported by its
// cache file system, else the size's, else its disk budget (0: unknown).
func localCapacity(m Machine) int64 {
	switch {
	case m.VM.Capacity > 0:
		return m.VM.Capacity
	case m.Stack.Capacity > 0:
		return m.Stack.Capacity
	case m.DiskBudget > 0:
		return m.DiskBudget
	}
	return int64(diskFull * float64(m.DiskTotal))
}

// thrashing reports whether m's cache file system thrashes now.
func thrashing(m Machine) bool {
	return m.VM.Thrashing || m.VM.Capacity > 0 && m.VM.WorkingSet > m.VM.Capacity
}

// demand is the working set t needs locally on m: its learned working
// set, raised to the machine's when the machine thrashes (a thrashing
// machine penalises every placement).
func demand(m Machine, t Task, capacity int64) int64 {
	ws := t.Profile.WorkingSet
	if thrashing(m) {
		ws = max(ws, m.VM.WorkingSet)
		if capacity > 0 && ws <= capacity {
			ws = capacity + capacity/4
		}
	}
	return ws
}

// thrashOf is the thrash penalty of running t on m, the local capacity it
// would need to avoid it and the capacity m has.
func thrashOf(f Pool, m Machine, t Task) (pen time.Duration, need, capacity int64) {
	capacity = localCapacity(m)
	ws := demand(m, t, capacity)
	return ThrashTime(m.Stack, capacity, ws, refetchOf(f, m.Provider, t)), ws, capacity
}

// growsCapacity reports whether growing m's disk grows its local tier:
// the slot persists on the top tier (a single hot medium over the store),
// not on a volume below a fixed local disk.
func growsCapacity(m Machine) bool {
	return len(m.Stack.Levels) <= 2
}

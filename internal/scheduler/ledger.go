package scheduler

import "fmt"

// DiskLedger is the physical account of a cache disk
// (docs/design-storage-v2.md): Used = Pinned + Σ Reclaim[i].Bytes, and a
// new session may take Room() bytes.
type DiskLedger struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
	// Pinned cannot be freed now: leased slots, data not yet in the
	// store, images, system and unaccounted files.
	Pinned int64 `json:"pinned"`
	// Reserve is the headroom kept free: a floor plus the forecast growth
	// of the running sessions.
	Reserve int64 `json:"reserve"`
	// Reclaim lists what can be freed, in the worker's reclaim order
	// (ascending cost per byte).
	Reclaim []Reclaimable `json:"reclaim,omitempty"`
}

// Reclaim classes (Reclaimable.Class).
const (
	ReclaimGarbage   = "garbage"   // stale build units in a slot (trim)
	ReclaimResident  = "resident"  // clean pages and materialized files (refetched)
	ReclaimWriteback = "writeback" // written-back blobs
	ReclaimDormant   = "dormant"   // local tiers of repositories without slots
	ReclaimSource    = "source"    // faulted-in source contents
	ReclaimMachine   = "machine"   // machine caches over their max_size
	ReclaimSlot      = "slot"      // an idle, flushed slot (demoted to its store head)
)

// Reclaimable is one entry of the ledger: Bytes freed physically by
// reclaiming it and Cost, the build seconds that is expected to lose
// (rebuild time x probability of reuse).
type Reclaimable struct {
	Class string  `json:"class"`
	Repo  string  `json:"repo,omitempty"`
	Bytes int64   `json:"bytes"`
	Cost  float64 `json:"cost,omitempty"`
}

// Reclaimable returns the bytes the ledger can free.
func (l DiskLedger) Reclaimable() int64 {
	var n int64
	for _, r := range l.Reclaim {
		n += r.Bytes
	}
	return n
}

// Room is the bytes a new session may take: free plus reclaimable,
// less the reserve.
func (l DiskLedger) Room() int64 { return l.Free + l.Reclaimable() - l.Reserve }

// freshUsed is what a new machine's disk holds before its first session
// (system, images; production: 17.5 GB of 196-215 GB), at most a tenth.
const freshUsed = 18 << 30

// FreshDisk is the ledger a new machine (or a detached volume holding
// slots) is expected to report for a disk of total bytes (nil: unknown).
func FreshDisk(total int64, slots map[string]int64) *DiskLedger {
	if total <= 0 {
		return nil
	}
	l := &DiskLedger{Total: total, Reserve: MinReserve(total)}
	used := min(int64(freshUsed), total/10)
	for _, b := range slots {
		used += b
	}
	l.Free, l.Pinned = max(total-used, 0), used
	return l
}

// MinReserve is the worker's reserve without running sessions: 5 GB or
// a tenth of the disk (the default disk_budget of 90 %).
func MinReserve(total int64) int64 { return max(5<<30, total/10) }

// growAt is the share of a disk that pinned data plus the reserve may
// take before the disk grows: early, because EBS allows one change per
// 6 h and space must be there before it is needed.
const growAt = 0.6

// growDisks grows the disks of running machines whose pinned data and
// reserve exceed growAt, by half (at least 50 GB, at most the size's
// volume maximum), in the background: no placement waits for it.
func (p Default) growDisks(st *planState) {
	f := st.f
	for _, m := range f.Machines {
		l := m.Disk
		if m.State != Running || l == nil || l.Total <= 0 || !m.CanResize || f.Now.Before(m.ResizableAt) {
			continue
		}
		if float64(l.Pinned+l.Reserve) <= growAt*float64(l.Total) {
			continue
		}
		target := l.Total + max(l.Total/2, 50<<30)
		if s, ok := sizeOf(f, m); ok && s.Volume.Max > 0 {
			target = min(target, s.Volume.Max)
		}
		if target <= l.Total {
			continue
		}
		st.acts = append(st.acts, Action{Kind: Grow, Machine: m.ID, Bytes: target,
			Reason: fmt.Sprintf("pinned data and reserve above %.0f %% of the disk", 100*growAt)})
	}
}

package scheduler

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

package anyfs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// ErrNotFound means a tier does not hold a page (for an upper tier of a
// link: a page fault).
var ErrNotFound = errors.New("anyfs: page not found")

// ErrFull means a tier cannot take a page: everything resident is dirty
// or pinned.
var ErrFull = errors.New("anyfs: tier full")

// Tier stores pages. Implementations are safe for concurrent use.
type Tier interface {
	// Get returns a page's content (ErrNotFound if absent). Contents are
	// verified against the key by tiers that cross a trust boundary.
	Get(ctx context.Context, k Key) ([]byte, error)
	// Put stores a page. Dirty pages are not yet held below this tier.
	Put(ctx context.Context, k Key, b []byte, s State) error
	// Has reports whether the tier holds the page.
	Has(ctx context.Context, k Key) bool
	// Reclaim drops clean, unpinned pages until at most target bytes are
	// resident and returns the bytes freed. Tiers without residency
	// (store, peer) return 0.
	Reclaim(ctx context.Context, target int64) (int64, error)
	// Profile describes the tier (for a link: the composite of the stack).
	Profile() Profile
}

// Optional tier capabilities.
type (
	// Statter reports residency and VM counters.
	Statter interface{ Stats() Stats }
	// Flusher writes every dirty page back to the tier below.
	Flusher interface {
		Flush(ctx context.Context) error
	}
	// Discarder drops a dirty page that is no longer referenced before it
	// was written back (the file was deleted before flush).
	Discarder interface{ Discard(k Key) }
	// Pinner pins pages (never reclaimed) or unpins them.
	Pinner interface{ Pin(k Key, pinned bool) }
	// Remover deletes a page (exclusive links move pages up).
	Remover interface{ Remove(k Key) }
	// Cleaner marks a dirty page clean after it was written back.
	Cleaner interface{ MarkClean(k Key) }
	// Prefetcher pages in keys in the background (readahead).
	Prefetcher interface {
		Prefetch(ctx context.Context, keys []Key)
	}
)

// Profile is the capability profile of a tier or a stack of tiers: the
// media from top to bottom (each a medium.Profile) and the local
// capacity.
type Profile struct {
	Name     string           `json:"name"`
	Levels   []medium.Profile `json:"levels"`
	Capacity int64            `json:"capacity,omitempty"` // bytes of the top tier (0: unbounded)
}

// Top is the profile of the top medium.
func (p Profile) Top() medium.Profile {
	if len(p.Levels) == 0 {
		return medium.Profile{}
	}
	return p.Levels[0]
}

// Backing is the profile of the bottom (most durable) medium.
func (p Profile) Backing() medium.Profile {
	if len(p.Levels) == 0 {
		return medium.Profile{}
	}
	return p.Levels[len(p.Levels)-1]
}

// Durability is the widest durability of any level.
func (p Profile) Durability() medium.Reach {
	var d medium.Reach
	for _, l := range p.Levels {
		d = max(d, l.Durability)
	}
	return d
}

// FaultTime estimates the time to page in bytes from the backing medium
// in pages of PageSize: per page latency plus transfer, bounded by the
// slowest level on the way up.
func (p Profile) FaultTime(bytes int64) time.Duration {
	if bytes <= 0 || len(p.Levels) == 0 {
		return 0
	}
	var worst time.Duration
	for _, l := range p.Levels {
		worst = max(worst, l.TransferTime(bytes))
	}
	pages := (bytes + PageSize - 1) / PageSize
	return worst + time.Duration(pages)*p.Backing().Latency
}

// Stack returns the profile as a hot medium over a backing store (the
// scheduler's view).
func (p Profile) Stack() medium.Stack {
	return medium.Stack{Hot: p.Top(), Store: p.Backing()}
}

// String lists the stack, e.g. "local>volume>store".
func (p Profile) String() string {
	var s []string
	for _, l := range p.Levels {
		s = append(s, l.Medium)
	}
	return strings.Join(s, ">")
}

// Compose stacks two profiles (upper on top of lower).
func Compose(upper, lower Profile) Profile {
	return Profile{Name: upper.Name + ">" + lower.Name, Levels: append(append([]medium.Profile{}, upper.Levels...), lower.Levels...), Capacity: upper.Capacity}
}

// Stats are a tier's residency and VM counters (cumulative counters
// since the tier was opened).
type Stats struct {
	ResidentBytes   int64 `json:"resident_bytes"`
	DirtyBytes      int64 `json:"dirty_bytes"`
	PinnedBytes     int64 `json:"pinned_bytes,omitempty"`
	WorkingSetBytes int64 `json:"working_set_bytes"`
	Capacity        int64 `json:"capacity,omitempty"`
	Pages           int64 `json:"pages"`
	// Faults counts page faults (misses of the upper tier), PageIns the
	// pages fetched from below, PageOuts the pages written below
	// (writeback and demotion), Refaults faults of recently reclaimed
	// pages.
	Faults       int64 `json:"faults"`
	Refaults     int64 `json:"refaults,omitempty"`
	PageIns      int64 `json:"page_ins"`
	PageInBytes  int64 `json:"page_in_bytes"`
	PageOuts     int64 `json:"page_outs"`
	PageOutBytes int64 `json:"page_out_bytes"`
	Writebacks   int64 `json:"writebacks"`
	ReclaimBytes int64 `json:"reclaim_bytes"`
	// FaultRate is page faults per second (exponentially weighted).
	FaultRate float64 `json:"fault_rate"`
	// Thrashing is set while refaults show a working set larger than the
	// capacity.
	Thrashing bool `json:"thrashing,omitempty"`
}

// Add sums counters and residency of two tiers' stats.
func (s Stats) Add(o Stats) Stats {
	s.ResidentBytes += o.ResidentBytes
	s.DirtyBytes += o.DirtyBytes
	s.PinnedBytes += o.PinnedBytes
	s.WorkingSetBytes += o.WorkingSetBytes
	s.Capacity += o.Capacity
	s.Pages += o.Pages
	s.Faults += o.Faults
	s.Refaults += o.Refaults
	s.PageIns += o.PageIns
	s.PageInBytes += o.PageInBytes
	s.PageOuts += o.PageOuts
	s.PageOutBytes += o.PageOutBytes
	s.Writebacks += o.Writebacks
	s.ReclaimBytes += o.ReclaimBytes
	s.FaultRate += o.FaultRate
	s.Thrashing = s.Thrashing || o.Thrashing
	return s
}

// StatsOf returns t's stats (zero if it has none).
func StatsOf(t Tier) Stats {
	if s, ok := t.(Statter); ok {
		return s.Stats()
	}
	return Stats{}
}

package anyfs

import (
	"math"
	"sync"
	"time"
)

// Event types (also the names in the orchestrator's event log).
const (
	EvPageFault = "page_fault" // a lookup missed the upper tier
	EvPageIn    = "page_in"    // pages fetched from a lower tier
	EvPageOut   = "page_out"   // pages written to a lower tier (demotion)
	EvWriteback = "writeback"  // dirty pages written back
	EvReclaim   = "reclaim"    // resident pages dropped
	EvThrash    = "thrash"     // working set larger than the capacity
)

// Event is one VM event of a tier or file system.
type Event struct {
	Type    string  `json:"type"`
	Tier    string  `json:"tier,omitempty"`
	Path    string  `json:"path,omitempty"`
	Pages   int64   `json:"pages,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

// Observer receives events (must not block).
type Observer func(Event)

func (o Observer) emit(e Event) {
	if o != nil {
		o(e)
	}
}

// rate is an exponentially weighted events-per-second meter.
type rate struct {
	mu    sync.Mutex
	tau   time.Duration
	value float64
	last  time.Time
}

func (r *rate) add(now time.Time, n float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decay(now)
	r.value += n / r.tau.Seconds()
}

func (r *rate) decay(now time.Time) {
	if r.tau == 0 {
		r.tau = time.Minute
	}
	if !r.last.IsZero() {
		dt := now.Sub(r.last).Seconds()
		r.value *= math.Exp(-dt / r.tau.Seconds())
	}
	r.last = now
}

func (r *rate) get(now time.Time) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decay(now)
	return r.value
}

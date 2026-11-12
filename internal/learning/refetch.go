package learning

import (
	"sync"

	"github.com/traumventures/offshore/internal/events"
)

// Refetch calibrates the scheduler's refetch factor of thrashing builds
// (scheduler.Pool.Refetch) from the cache file system observations the
// orchestrator receives from workers (passed as events.Event values, the
// same observations it also records in the event log; the log itself is
// never read): while a
// machine thrashes (a thrash event, or a machine_sample whose VM reports
// thrashing or a working set beyond its capacity), the bytes it pages in
// that it reclaimed before are refaults. When the episode ends (a
// task_end on the machine, a sample that no longer thrashes, a stop or
// destroy) its refaulted bytes (min of page_in and reclaim bytes) divided
// by the excess of the working set over the local capacity is one sample
// of the factor. Samples feed a decaying average per provider and over
// all providers (key ""). Refetch is safe for concurrent use.
type Refetch struct {
	// Alpha is the weight of a new sample in the decaying average
	// (default 0.3).
	Alpha float64

	mu       sync.Mutex
	est      map[string]float64 // provider ("" = all) -> factor
	n        map[string]int     // samples per key
	episodes map[string]*episode
	// save persists the estimates (the learning store); nil: memory only.
	save func(map[string]Factor)
}

// episode is a machine's current thrashing period.
type episode struct {
	provider        string
	thrashing       bool
	ws, capacity    int64 // largest working set, last capacity
	pageIn, reclaim int64
}

// Sample bounds: a sample outside is clamped (noise, partial windows).
const (
	minRefetch = 0.1
	maxRefetch = 20
)

// NewRefetch returns an empty calibrator.
func NewRefetch() *Refetch {
	return &Refetch{est: map[string]float64{}, n: map[string]int{}, episodes: map[string]*episode{}}
}

// Observe feeds one event (other types are ignored).
func (r *Refetch) Observe(ev events.Event) {
	if r == nil || ev.Machine == "" {
		return
	}
	switch ev.Type {
	case events.Thrash, events.MachineSample, events.PageIn, events.Reclaim, events.TaskEnd, events.MachineStop, events.MachineDestroy:
	default:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.episodes[ev.Machine]
	if e == nil {
		e = &episode{}
		r.episodes[ev.Machine] = e
	}
	if ev.Provider != "" {
		e.provider = ev.Provider
	}
	switch ev.Type {
	case events.Thrash:
		e.thrashing = true
		if ev.VM != nil {
			e.note(ev.VM.WorkingSet, ev.VM.Capacity)
		}
		e.ws = max(e.ws, ev.Bytes)
	case events.MachineSample:
		if ev.VM == nil {
			return
		}
		t := ev.VM.Thrashing || ev.VM.Capacity > 0 && ev.VM.WorkingSet > ev.VM.Capacity
		if t {
			e.thrashing = true
			e.note(ev.VM.WorkingSet, ev.VM.Capacity)
		} else if e.thrashing {
			r.closeLocked(e)
			e.thrashing = false
		}
	case events.PageIn:
		if e.thrashing {
			e.pageIn += ev.Bytes
		}
	case events.Reclaim:
		if e.thrashing {
			e.reclaim += ev.Bytes
		}
	case events.TaskEnd:
		if e.thrashing {
			r.closeLocked(e) // one sample per build; the machine may thrash on
		}
	case events.MachineStop, events.MachineDestroy:
		if e.thrashing {
			r.closeLocked(e)
		}
		delete(r.episodes, ev.Machine)
	}
}

func (e *episode) note(ws, capacity int64) {
	e.ws = max(e.ws, ws)
	if capacity > 0 {
		e.capacity = capacity
	}
}

// closeLocked turns e's counters into a sample (when it has one) and
// resets them.
func (r *Refetch) closeLocked(e *episode) {
	defer func() { e.pageIn, e.reclaim, e.ws = 0, 0, 0 }()
	excess := e.ws - e.capacity
	refaulted := min(e.pageIn, e.reclaim)
	if e.capacity <= 0 || excess <= 0 || e.pageIn <= 0 {
		return
	}
	x := min(max(float64(refaulted)/float64(excess), minRefetch), maxRefetch)
	r.addLocked(e.provider, x)
	if e.provider != "" {
		r.addLocked("", x)
	}
}

// Add records one sample x of the factor for provider prov ("" = only
// the global estimate). Simulations feed known refaults directly.
func (r *Refetch) Add(prov string, x float64) {
	if r == nil || x <= 0 {
		return
	}
	x = min(max(x, minRefetch), maxRefetch)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addLocked(prov, x)
	if prov != "" {
		r.addLocked("", x)
	}
}

func (r *Refetch) addLocked(key string, x float64) {
	a := r.Alpha
	if a <= 0 || a > 1 {
		a = 0.3
	}
	if r.n[key] == 0 {
		r.est[key] = x
	} else {
		r.est[key] += a * (x - r.est[key])
	}
	r.n[key]++
	if r.save != nil {
		fs := make(map[string]Factor, len(r.est))
		for k, v := range r.est {
			fs[k] = Factor{Value: v, Samples: r.n[k]}
		}
		r.save(fs)
	}
}

// Factors returns the calibrated factor per provider ("" = all
// providers); nil without samples (the scheduler then uses its default).
func (r *Refetch) Factors() map[string]float64 {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.est) == 0 {
		return nil
	}
	out := make(map[string]float64, len(r.est))
	for k, v := range r.est {
		out[k] = v
	}
	return out
}

// Samples returns how many samples the estimate of key is based on.
func (r *Refetch) Samples(key string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[key]
}

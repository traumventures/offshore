package scheduler

import (
	"sort"
	"time"
)

// Profile is what the estimator learned about the runs of a key, used by
// the cost model. Zero fields are unknown; the model then uses defaults.
type Profile struct {
	// Warm and Cold are the median durations of runs with and without a
	// reused build slot, without restore and thrash time (base run times:
	// the model adds the restore and the thrashing predicted for the
	// candidate machine).
	Warm time.Duration `json:"warm,omitempty"`
	Cold time.Duration `json:"cold,omitempty"`
	// Spread is the ratio of the 80th percentile to the median of the run
	// durations (>= 1; 0 = unknown): release forecasts plan with the
	// pessimistic end, so that waiting for a busy machine is not chosen on
	// optimistic estimates.
	Spread float64 `json:"spread,omitempty"`
	// ColdBy is the median cold duration per provider: global result
	// caches (sccache, ...) live in a provider's store, so cold builds are
	// faster where the cache is.
	ColdBy map[string]time.Duration `json:"cold_by,omitempty"`
	// Restore is the median duration of a slot restore from a snapshot.
	Restore time.Duration `json:"restore,omitempty"`
	// SlotBytes is the median slot size after a run; SlotGrowth the median
	// growth of the slot per run (after - before, runs on an existing
	// slot).
	SlotBytes  int64 `json:"slot_bytes,omitempty"`
	SlotGrowth int64 `json:"slot_growth,omitempty"`
	// WorkingSet is the bytes a run touches (the 80th percentile of the
	// runs that reported it): what a lazy restore faults in before the
	// build proceeds and what must fit the local tier to avoid thrashing.
	WorkingSet int64 `json:"working_set,omitempty"`
	// Thrash is the median thrash time of the runs that thrashed (what
	// was subtracted from their durations; informational).
	Thrash time.Duration `json:"thrash,omitempty"`
	// Refetch is the refetch factor learned for the key: the median of
	// refaulted bytes per byte of working set beyond the local capacity
	// over its thrashing runs (0 = fewer than MinRefetchRuns samples; the
	// model then uses the provider's or global calibration).
	Refetch float64 `json:"refetch,omitempty"`
	Runs    int     `json:"runs,omitempty"`
}

// Defaults the cost model assumes for unknown profiles.
const (
	DefaultRun         = 2 * time.Minute
	DefaultColdPenalty = 0.5 // cold = warm * (1 + penalty)
	// MinRefetchRuns is how many thrashing runs a key needs before its
	// own refetch factor replaces the provider's.
	MinRefetchRuns = 2
)

// WarmRun is the expected duration with a warm slot.
func (p Profile) WarmRun() time.Duration {
	switch {
	case p.Warm > 0:
		return p.Warm
	case p.Cold > 0:
		return time.Duration(float64(p.Cold) / (1 + DefaultColdPenalty))
	}
	return DefaultRun
}

// ColdRun is the expected duration without a slot on a machine of prov.
func (p Profile) ColdRun(prov string) time.Duration {
	if d, ok := p.ColdBy[prov]; ok && d > 0 {
		return d
	}
	if p.Cold > 0 {
		return p.Cold
	}
	return time.Duration(float64(p.WarmRun()) * (1 + DefaultColdPenalty))
}

// High returns the pessimistic (80th percentile) duration for a median d.
func (p Profile) High(d time.Duration) time.Duration {
	sp := p.Spread
	if sp < 1 {
		sp = 1.5
	}
	return time.Duration(float64(d) * sp)
}

func medianDur(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[(len(ds)-1)/2]
}

func medianInt(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs[(len(xs)-1)/2]
}

// profileOf derives a profile from observations (oldest first).
func profileOf(rs []Usage) Profile {
	var warm, cold, restore, thrash []time.Duration
	coldBy := map[string][]time.Duration{}
	var slots, growth, ws []int64
	var refetch []float64
	p := Profile{}
	for _, u := range rs {
		if u.OOM || u.Wall <= 0 {
			continue
		}
		p.Runs++
		th := time.Duration(u.ThrashSeconds * float64(time.Second))
		if th > 0 {
			thrash = append(thrash, th)
		}
		if u.RefaultBytes > 0 && u.LocalCapacity > 0 && u.WorkingSetBytes > u.LocalCapacity {
			x := float64(u.RefaultBytes) / float64(u.WorkingSetBytes-u.LocalCapacity)
			refetch = append(refetch, min(max(x, 0.1), 20)) // noise, partial windows
		}
		// The base run: the wall time without what the machine added
		// (restore, thrashing); at least a tenth of the wall time against
		// overstated reports.
		base := func(d time.Duration) time.Duration { return max(d-th, u.Wall/10) }
		if u.Warm || u.Restored {
			warm = append(warm, base(u.Wall-time.Duration(u.RestoreSeconds*float64(time.Second))))
		} else {
			cold = append(cold, base(u.Wall))
			if u.Provider != "" {
				coldBy[u.Provider] = append(coldBy[u.Provider], base(u.Wall))
			}
		}
		if u.Restored && u.RestoreSeconds > 0 {
			restore = append(restore, time.Duration(u.RestoreSeconds*float64(time.Second)))
		}
		if u.WorkingSetBytes > 0 {
			ws = append(ws, u.WorkingSetBytes)
		}
		if u.SlotBytesAfter > 0 {
			slots = append(slots, u.SlotBytesAfter)
			if u.SlotBytesBefore > 0 {
				growth = append(growth, max(u.SlotBytesAfter-u.SlotBytesBefore, 0))
			}
		}
	}
	var spread []float64
	for _, ds := range [][]time.Duration{warm, cold} {
		if len(ds) >= 3 {
			sorted := append([]time.Duration(nil), ds...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			med := sorted[(len(sorted)-1)/2]
			p80 := sorted[min(len(sorted)-1, int(0.8*float64(len(sorted))))]
			if med > 0 {
				spread = append(spread, float64(p80)/float64(med))
			}
		}
	}
	for _, x := range spread {
		p.Spread = max(p.Spread, x)
	}
	p.Warm, p.Cold, p.Restore = medianDur(warm), medianDur(cold), medianDur(restore)
	p.Thrash = medianDur(thrash)
	if len(refetch) >= MinRefetchRuns {
		sort.Float64s(refetch)
		p.Refetch = refetch[(len(refetch)-1)/2]
	}
	for prov, ds := range coldBy {
		if p.ColdBy == nil {
			p.ColdBy = map[string]time.Duration{}
		}
		p.ColdBy[prov] = medianDur(ds)
	}
	p.SlotBytes, p.SlotGrowth = medianInt(slots), medianInt(growth)
	if len(ws) > 0 {
		sort.Slice(ws, func(i, j int) bool { return ws[i] < ws[j] })
		p.WorkingSet = ws[min(len(ws)-1, int(0.8*float64(len(ws))))]
	}
	return p
}

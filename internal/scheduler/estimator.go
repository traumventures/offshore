package scheduler

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"sync"
)

// History is the default Estimator: it keeps the last observations per key
// and reserves the p95 of the measured peak memory plus headroom. CPU is
// the average number of busy cores, capped by the configured value.
// Observations are persisted to a JSON file.
type History struct {
	Path       string  // persistence file; empty keeps history in memory
	Window     int     // observations kept per key (default 20)
	MinSamples int     // observations before estimates replace the fallback (default 3)
	Headroom   float64 // added to the memory percentile (default 0.25)
	MinMemory  int64   // lower bound for memory estimates (default 256 MiB)
	OOMBackoff float64 // memory factor after an OOM kill (default 1.5)

	mu   sync.Mutex
	runs map[string][]Usage
}

// NewHistory loads history from path (if it exists).
func NewHistory(path string) *History {
	h := &History{Path: path, runs: map[string][]Usage{}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			json.Unmarshal(b, &h.runs)
		}
	}
	if h.runs == nil {
		h.runs = map[string][]Usage{}
	}
	return h
}

func (h *History) defaults() (window, min int, headroom float64, minMem int64, oom float64) {
	window, min, headroom, minMem, oom = h.Window, h.MinSamples, h.Headroom, h.MinMemory, h.OOMBackoff
	if window <= 0 {
		window = 20
	}
	if min <= 0 {
		min = 3
	}
	if headroom <= 0 {
		headroom = 0.25
	}
	if minMem <= 0 {
		minMem = 256 << 20
	}
	if oom <= 1 {
		oom = 1.5
	}
	return
}

// Observe implements Estimator.
func (h *History) Observe(key string, u Usage) {
	window, _, _, _, _ := h.defaults()
	u.Cache = nil // not used for estimates; keeps the history small
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := append(h.runs[key], u)
	if len(rs) > window {
		rs = rs[len(rs)-window:]
	}
	h.runs[key] = rs
	if h.Path != "" {
		if b, err := json.Marshal(h.runs); err == nil {
			tmp := h.Path + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				os.Rename(tmp, h.Path)
			}
		}
	}
}

// Estimate implements Estimator.
func (h *History) Estimate(key string, fallback Resources) Resources {
	_, minSamples, headroom, minMem, oomFactor := h.defaults()
	h.mu.Lock()
	rs := append([]Usage{}, h.runs[key]...)
	h.mu.Unlock()
	if len(rs) == 0 {
		return fallback
	}
	// An OOM kill in the last run overrides everything: grow from the
	// reservation that failed (or the fallback).
	if last := rs[len(rs)-1]; last.OOM {
		base := max(last.PeakMemory, fallback.Memory)
		return Resources{CPU: fallback.CPU, Memory: int64(float64(base) * oomFactor)}
	}
	if len(rs) < minSamples {
		return fallback
	}
	var peaks []int64
	var cores []float64
	for _, u := range rs {
		if u.PeakMemory > 0 {
			peaks = append(peaks, u.PeakMemory)
		}
		if u.Wall > 0 && u.CPUSeconds > 0 {
			cores = append(cores, u.CPUSeconds/u.Wall.Seconds())
		}
	}
	out := fallback
	if len(peaks) >= minSamples {
		sort.Slice(peaks, func(i, j int) bool { return peaks[i] < peaks[j] })
		p95 := peaks[int(math.Ceil(0.95*float64(len(peaks))))-1]
		out.Memory = max(int64(float64(p95)*(1+headroom)), minMem)
	}
	if len(cores) >= minSamples && fallback.CPU > 0 {
		sum := 0.0
		for _, c := range cores {
			sum += c
		}
		out.CPU = math.Min(math.Max(sum/float64(len(cores)), 0.25), fallback.CPU)
	}
	return out
}

// Static is an Estimator that always returns the configured value.
type Static struct{}

func (Static) Estimate(_ string, fallback Resources) Resources { return fallback }
func (Static) Observe(string, Usage)                           {}

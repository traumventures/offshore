package scheduler

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
)

// History is the default Estimator: it keeps the last observations per key
// and reserves the p90 of the measured peak memory plus headroom (a robust
// quantile: one outlier among few runs does not inflate every later
// reservation). Before MinSamples runs it reserves the fallback, raised to
// the largest observed peak plus headroom (a key that needed 12G once must
// not be packed with a 4G default). CPU is the average number of busy
// cores, capped by the configured value.
// Observations are persisted to a JSON file.
type History struct {
	Path       string  // persistence file; empty keeps history in memory
	Window     int     // observations kept per key (default 20)
	MinSamples int     // observations before estimates replace the fallback (default 3)
	Headroom   float64 // added to the memory percentile (default 0.25)
	MinMemory  int64   // lower bound for memory estimates (default 256 MiB)
	OOMBackoff float64 // memory factor after an OOM kill (default 1.5)

	// Persist, when set, receives a key's runs after every observation
	// instead of the file at Path (the learning store, see
	// internal/learning).
	Persist func(key string, runs []Usage)

	mu   sync.Mutex
	runs map[string][]Usage
}

// NewHistoryWith returns a History starting from runs that hands every
// key's updated runs to persist (the learning store).
func NewHistoryWith(runs map[string][]Usage, persist func(key string, runs []Usage)) *History {
	if runs == nil {
		runs = map[string][]Usage{}
	}
	return &History{runs: runs, Persist: persist}
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
	if h.Persist != nil {
		h.Persist(key, append([]Usage(nil), rs...))
		return
	}
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
	return h.Explain(key, fallback).Need
}

// Explain implements Estimator.
func (h *History) Explain(key string, fallback Resources) Estimate {
	_, minSamples, _, _, _ := h.defaults()
	h.mu.Lock()
	n := len(h.runs[key])
	parent := ""
	if n < minSamples {
		// Too few runs of the key: the closest parent key with enough
		// runs (same tool and subcommand, other package scope or
		// profile) knows more than the configured fallback.
		for _, pk := range ParentKeys(key) {
			if len(h.runs[pk]) >= minSamples && !h.runs[pk][len(h.runs[pk])-1].OOM {
				parent = pk
				break
			}
		}
	}
	own := append([]Usage{}, h.runs[key]...)
	h.mu.Unlock()
	if parent != "" && (n == 0 || !own[n-1].OOM) {
		_, _, headroom, _, _ := h.defaults()
		e := h.explain(parent, fallback)
		e.Requested = key
		for _, u := range own {
			if m := int64(float64(u.PeakMemory) * (1 + headroom)); m > e.Need.Memory {
				e.Need.Memory, e.PeakMemory = m, u.PeakMemory
			}
		}
		if e.MemorySource == SourceLearned {
			e.MemorySource = SourceLearnedParent
		}
		if e.CPUSource == SourceLearned {
			e.CPUSource = SourceLearnedParent
		}
		return e
	}
	e := h.explain(key, fallback)
	e.Requested = key
	return e
}

// ParentKeys returns the more general usage keys of key, closest first:
// without the package scope ("?p=..." or "?workspace"), then also
// without the release profile ("/release").
func ParentKeys(key string) []string {
	var out []string
	if i := strings.IndexByte(key, '?'); i >= 0 {
		key = key[:i]
		out = append(out, key)
	}
	if k, ok := strings.CutSuffix(key, "/release"); ok {
		out = append(out, k)
	}
	return out
}

// explain estimates key from its own runs.
func (h *History) explain(key string, fallback Resources) Estimate {
	_, minSamples, headroom, minMem, oomFactor := h.defaults()
	h.mu.Lock()
	rs := append([]Usage{}, h.runs[key]...)
	h.mu.Unlock()
	e := Estimate{Key: key, Need: fallback, Fallback: fallback, Samples: len(rs), MinSamples: minSamples,
		CPUSource: SourceFallback, MemorySource: SourceFallback, PeakMemory: fallback.Memory, Cores: fallback.CPU}
	if len(rs) == 0 {
		e.Statistic = "no runs"
		return e
	}
	// An OOM kill in the last run overrides everything: grow from the
	// reservation that failed (or the fallback).
	if last := rs[len(rs)-1]; last.OOM {
		base := max(last.PeakMemory, fallback.Memory)
		e.Need = Resources{CPU: fallback.CPU, Memory: int64(float64(base) * oomFactor)}
		e.MemorySource, e.Statistic = SourceOOMBackoff, fmt.Sprintf("last run OOM: max(peak, fallback) x %.2g", oomFactor)
		e.PeakMemory = e.Need.Memory
		return e
	}
	if len(rs) < minSamples {
		for _, u := range rs {
			if m := int64(float64(u.PeakMemory) * (1 + headroom)); m > e.Need.Memory {
				e.Need.Memory, e.PeakMemory = m, u.PeakMemory
				e.MemorySource = SourceLearnedMax
			}
		}
		e.Statistic = fmt.Sprintf("max(fallback, max peak x %.2g) until %d runs", 1+headroom, minSamples)
		return e
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
	var stats []string
	if len(peaks) >= minSamples {
		sort.Slice(peaks, func(i, j int) bool { return peaks[i] < peaks[j] })
		p90 := peaks[int(math.Ceil(0.9*float64(len(peaks))))-1]
		e.Need.Memory = max(int64(float64(p90)*(1+headroom)), minMem)
		e.PeakMemory, e.MemorySource = p90, SourceLearned
		stats = append(stats, fmt.Sprintf("memory p90 of %d peaks x %.2g", len(peaks), 1+headroom))
	}
	if len(cores) >= minSamples && fallback.CPU > 0 {
		sum := 0.0
		for _, c := range cores {
			sum += c
		}
		e.Cores = sum / float64(len(cores))
		e.Need.CPU = math.Min(math.Max(e.Cores, 0.25), fallback.CPU)
		e.CPUSource = SourceLearned
		stats = append(stats, fmt.Sprintf("cpu mean of %d runs capped by fallback", len(cores)))
	}
	e.Statistic = strings.Join(stats, "; ")
	return e
}

// Profile implements Estimator.
func (h *History) Profile(key string) Profile {
	h.mu.Lock()
	rs := append([]Usage{}, h.runs[key]...)
	h.mu.Unlock()
	return profileOf(rs)
}

// Static is an Estimator that always returns the configured value.
type Static struct{}

func (Static) Estimate(_ string, fallback Resources) Resources { return fallback }
func (Static) Explain(key string, fallback Resources) Estimate {
	return Estimate{Key: key, Need: fallback, Fallback: fallback, CPUSource: SourceFallback, MemorySource: SourceFallback,
		PeakMemory: fallback.Memory, Cores: fallback.CPU, Statistic: "static"}
}
func (Static) Observe(string, Usage)  {}
func (Static) Profile(string) Profile { return Profile{} }

// Sources of an estimate (per resource).
const (
	// SourceFallback: the repository's configured resources (fewer runs
	// than needed, or none).
	SourceFallback = "repo_config"
	// SourceLearnedMax: before enough runs, the largest observed peak plus
	// headroom raised the fallback.
	SourceLearnedMax = "learned_max"
	// SourceLearned: the statistic over the learned runs of the key.
	SourceLearned = "learned"
	// SourceOOMBackoff: the last run was OOM killed; memory grows.
	SourceOOMBackoff = "oom_backoff"
	// SourceLearnedParent: the key has too few runs; the statistic of a
	// more general key (ParentKeys) is used, raised by the key's own
	// peaks.
	SourceLearnedParent = "learned_parent"
)

// Estimate is a need with where it came from (traceable reservations):
// the estimator key, the runs known for it and the statistic used.
type Estimate struct {
	// Key is the usage key whose runs the statistic used; Requested the
	// task's own key (they differ for SourceLearnedParent).
	Key       string    `json:"key"`
	Requested string    `json:"requested,omitempty"`
	Need      Resources `json:"need"`
	// Fallback is the configured value (repository resources).
	Fallback     Resources `json:"fallback"`
	CPUSource    string    `json:"cpu_source"`
	MemorySource string    `json:"memory_source"`
	Samples      int       `json:"samples"`
	MinSamples   int       `json:"min_samples,omitempty"`
	Statistic    string    `json:"statistic,omitempty"`
	// PeakMemory and Cores are the predicted peak memory (without
	// headroom) and mean busy cores of a run (task_end compares them).
	PeakMemory int64   `json:"peak_memory,omitempty"`
	Cores      float64 `json:"cores,omitempty"`
}

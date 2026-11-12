// Package learning is the scheduler's self-improvement store: compact
// statistics the estimator and the policy learn from (per-key run time,
// memory and slot usage; the refetch factor of thrashing builds per
// provider; request arrivals per environment). The orchestrator writes it as sessions end and workers
// report; the estimator and the policy read it. It is persisted under the
// state directory (learning.json), bounded in size and versioned.
//
// It is deliberately separate from the event log (internal/events): the
// event log is for observability (humans, agents, the simulator's traces)
// and is never read by the scheduler; what the scheduler needs from the
// past lives here.
package learning

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

// Version is the schema version of learning.json. A file of another
// version is not loaded (the store starts empty and relearns).
const Version = 1

// FileName is the store's file in the state directory.
const FileName = "learning.json"

// legacyUsage is the estimator's file before the learning store; it is
// imported once when learning.json does not exist.
const legacyUsage = "usage-history.json"

// MaxKeys bounds the per-key usage statistics: beyond it the least
// recently updated keys are dropped.
const MaxKeys = 4096

// Factor is a calibrated factor and the samples it is based on.
type Factor struct {
	Value   float64 `json:"value"`
	Samples int     `json:"samples"`
}

type data struct {
	Version int `json:"version"`
	// Usage is the last runs per learning key (the estimator's window).
	Usage map[string][]scheduler.Usage `json:"usage"`
	// Touched is when a key's usage was last updated (eviction order).
	Touched map[string]time.Time `json:"touched"`
	// Refetch is the refetch factor per provider ("" = all providers).
	Refetch map[string]Factor `json:"refetch,omitempty"`
	// Arrivals are the last request arrivals per environment (lifecycle
	// policy: keep, stop or destroy idle machines).
	Arrivals map[string][]time.Time `json:"arrivals,omitempty"`
}

// Store is the learning store. It is safe for concurrent use.
type Store struct {
	path string // "" keeps it in memory
	now  func() time.Time

	mu sync.Mutex
	d  data
}

// Open loads the store of stateDir ("" = in memory only). A missing or
// unreadable file, or one of another schema version, starts empty; the
// estimator's legacy usage-history.json is imported once.
func Open(stateDir string) *Store {
	s := &Store{now: time.Now, d: data{Version: Version}}
	if stateDir == "" {
		s.init()
		return s
	}
	s.path = filepath.Join(stateDir, FileName)
	if b, err := os.ReadFile(s.path); err == nil {
		var d data
		if json.Unmarshal(b, &d) == nil && d.Version == Version {
			s.d = d
		}
	} else if b, err := os.ReadFile(filepath.Join(stateDir, legacyUsage)); err == nil {
		var runs map[string][]scheduler.Usage
		if json.Unmarshal(b, &runs) == nil {
			s.d.Usage = runs
		}
	}
	s.init()
	return s
}

func (s *Store) init() {
	if s.d.Usage == nil {
		s.d.Usage = map[string][]scheduler.Usage{}
	}
	if s.d.Touched == nil {
		s.d.Touched = map[string]time.Time{}
	}
	now := s.now()
	for k := range s.d.Usage {
		if _, ok := s.d.Touched[k]; !ok {
			s.d.Touched[k] = now
		}
	}
	s.evictLocked()
}

// Path is the store's file ("" in memory).
func (s *Store) Path() string { return s.path }

// History returns the estimator on the store's usage statistics: it
// starts from them and writes every observation back.
func (s *Store) History() *scheduler.History {
	s.mu.Lock()
	runs := make(map[string][]scheduler.Usage, len(s.d.Usage))
	for k, v := range s.d.Usage {
		runs[k] = append([]scheduler.Usage(nil), v...)
	}
	s.mu.Unlock()
	return scheduler.NewHistoryWith(runs, s.setUsage)
}

// Refetch returns a refetch calibrator starting from the stored factors
// and persisting every new estimate.
func (s *Store) Refetch() *Refetch {
	r := NewRefetch()
	s.mu.Lock()
	for k, f := range s.d.Refetch {
		r.est[k], r.n[k] = f.Value, f.Samples
	}
	s.mu.Unlock()
	r.save = s.setRefetch
	return r
}

// Arrivals returns an arrival recorder starting from the stored arrivals
// and persisting every new one.
func (s *Store) Arrivals() *Arrivals {
	a := NewArrivals()
	s.mu.Lock()
	for env, ts := range s.d.Arrivals {
		a.byEnv[env] = append([]time.Time(nil), ts...)
	}
	s.mu.Unlock()
	a.save = s.setArrivals
	return a
}

func (s *Store) setArrivals(byEnv map[string][]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Arrivals = byEnv
	s.saveLocked()
}

// Keys returns how many keys have usage statistics.
func (s *Store) Keys() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.d.Usage)
}

func (s *Store) setUsage(key string, runs []scheduler.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Usage[key] = append([]scheduler.Usage(nil), runs...)
	s.d.Touched[key] = s.now()
	s.evictLocked()
	s.saveLocked()
}

func (s *Store) setRefetch(fs map[string]Factor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Refetch = fs
	s.saveLocked()
}

// evictLocked drops the least recently updated keys beyond MaxKeys.
func (s *Store) evictLocked() {
	if len(s.d.Usage) <= MaxKeys {
		return
	}
	keys := make([]string, 0, len(s.d.Usage))
	for k := range s.d.Usage {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.d.Touched[keys[i]], s.d.Touched[keys[j]]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys[:len(keys)-MaxKeys] {
		delete(s.d.Usage, k)
		delete(s.d.Touched, k)
	}
}

// saveLocked writes the store atomically (temporary file and rename).
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(s.d)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.path)
	}
}

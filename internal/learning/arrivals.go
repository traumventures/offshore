package learning

import (
	"sync"
	"time"
)

// MaxArrivals bounds the arrival times kept per environment.
const MaxArrivals = 512

// Arrivals records when requests of each environment arrived (the last
// MaxArrivals): the scheduler's lifecycle policy derives the distribution
// of the time until the next request needs a machine from them (keep an
// idle machine running, stop it, destroy a stopped one). Safe for
// concurrent use.
type Arrivals struct {
	mu    sync.Mutex
	byEnv map[string][]time.Time
	save  func(map[string][]time.Time)
}

// NewArrivals returns an empty in-memory recorder.
func NewArrivals() *Arrivals { return &Arrivals{byEnv: map[string][]time.Time{}} }

// Observe records an arrival of env at t (times are kept in order; an
// arrival older than the newest one is inserted in place).
func (a *Arrivals) Observe(env string, t time.Time) {
	if env == "" {
		return
	}
	a.mu.Lock()
	ts := a.byEnv[env]
	i := len(ts)
	for i > 0 && ts[i-1].After(t) {
		i--
	}
	ts = append(ts, time.Time{})
	copy(ts[i+1:], ts[i:])
	ts[i] = t
	if len(ts) > MaxArrivals {
		ts = append([]time.Time(nil), ts[len(ts)-MaxArrivals:]...)
	}
	a.byEnv[env] = ts
	var snap map[string][]time.Time
	if a.save != nil {
		snap = a.copyLocked()
	}
	a.mu.Unlock()
	if snap != nil {
		a.save(snap)
	}
}

// Times returns a copy of the recorded arrivals per environment.
func (a *Arrivals) Times() map[string][]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.copyLocked()
}

func (a *Arrivals) copyLocked() map[string][]time.Time {
	out := make(map[string][]time.Time, len(a.byEnv))
	for env, ts := range a.byEnv {
		out[env] = append([]time.Time(nil), ts...)
	}
	return out
}

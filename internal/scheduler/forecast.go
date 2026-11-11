package scheduler

import (
	"sync"
	"time"
)

// DefaultForecastWindow is how far back Forecast looks for the peak.
const DefaultForecastWindow = 5 * time.Minute

// LoadWindow is how far back Forecast averages the offered load (the
// runaway guard of scale-out, see Default.guard).
const LoadWindow = 15 * time.Minute

// Forecast tracks recent demand per environment for consolidation and
// scale-in decisions: the peak reservation of running and queued sessions
// in a sliding window, and when the pool last scaled up. The orchestrator
// and the simulator feed it every pool snapshot and every plan, so that
// the policy itself stays a pure function of its input.
//
// A short lull between builds no longer looks like low demand: a machine
// is only drained when the peak of the whole window fits on the others.
type Forecast struct {
	Window time.Duration // default DefaultForecastWindow

	mu      sync.Mutex
	samples map[string][]demandSample
	up      map[string]time.Time
	down    map[string]time.Time // env -> last idle or consolidation stop
}

type demandSample struct {
	at  time.Time
	res Resources
}

// NewForecast returns an empty forecast with the default window.
func NewForecast() *Forecast { return &Forecast{} }

func (fc *Forecast) window() time.Duration {
	if fc.Window <= 0 {
		return DefaultForecastWindow
	}
	return fc.Window
}

// keep is how long samples are kept: the peak and the load window.
func (fc *Forecast) keep() time.Duration { return max(fc.window(), LoadWindow) }

// Observe records the current demand of every environment in f.
func (fc *Forecast) Observe(f Pool) {
	cur := map[string]Resources{}
	for _, m := range f.Machines {
		if m.State == Running {
			cur[m.EnvHash] = cur[m.EnvHash].Add(m.Reserved)
		}
	}
	for _, t := range f.Queue {
		cur[t.EnvHash] = cur[t.EnvHash].Add(t.Request)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.samples == nil {
		fc.samples = map[string][]demandSample{}
	}
	for env, ss := range fc.samples { // demand ended: the load is 0 from now on
		if _, ok := cur[env]; !ok && len(ss) > 0 && ss[len(ss)-1].res != (Resources{}) {
			cur[env] = Resources{}
		}
	}
	for env, r := range cur {
		ss := fc.samples[env]
		if n := len(ss); n > 0 && f.Now.Sub(ss[n-1].at) < time.Second {
			// At most one sample per second: keep the larger.
			ss[n-1].res = Resources{CPU: max(ss[n-1].res.CPU, r.CPU), Memory: max(ss[n-1].res.Memory, r.Memory)}
			continue
		}
		fc.samples[env] = append(ss, demandSample{f.Now, r})
	}
	fc.prune(f.Now)
}

// Note records the scale-up actions of a plan.
func (fc *Forecast) Note(f Pool, acts []Action) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.up == nil {
		fc.up = map[string]time.Time{}
	}
	if fc.down == nil {
		fc.down = map[string]time.Time{}
	}
	env := map[string]string{}
	for _, m := range f.Machines {
		env[m.ID] = m.EnvHash
	}
	for _, a := range acts {
		switch a.Kind {
		case Create:
			fc.up[a.EnvHash] = f.Now
		case Start, Undrain:
			if e, ok := env[a.Machine]; ok {
				fc.up[e] = f.Now
			}
		case Stop:
			if e, ok := env[a.Machine]; ok && (a.Reason == "idle" || a.Reason == "consolidated") {
				fc.down[e] = f.Now
			}
		}
	}
}

func (fc *Forecast) prune(now time.Time) {
	w := fc.keep()
	for env, ss := range fc.samples {
		i := 0
		for i < len(ss) && now.Sub(ss[i].at) > w {
			i++
		}
		ss = ss[i:]
		if len(ss) > int(w/time.Second)+1 {
			ss = ss[len(ss)-int(w/time.Second)-1:]
		}
		if len(ss) == 0 {
			delete(fc.samples, env)
		} else {
			fc.samples[env] = ss
		}
	}
}

// Apply sets f.Demand from the recorded window.
func (fc *Forecast) Apply(f *Pool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	f.Demand = map[string]Demand{}
	w := fc.window()
	for env, ss := range fc.samples {
		var d Demand
		for _, s := range ss {
			if f.Now.Sub(s.at) > w {
				continue
			}
			d.Peak.CPU = max(d.Peak.CPU, s.res.CPU)
			d.Peak.Memory = max(d.Peak.Memory, s.res.Memory)
		}
		d.Load = load(ss, f.Now, LoadWindow)
		f.Demand[env] = d
	}
	for env, t := range fc.up {
		d := f.Demand[env]
		d.LastScaleUp = t
		f.Demand[env] = d
	}
	for env, t := range fc.down {
		d := f.Demand[env]
		d.LastStop = t
		f.Demand[env] = d
	}
}

// load is the time average of the samples (each holding until the next,
// the last until now) over the window before now: by Little's law the
// arrival rate times the resource-seconds per session.
func load(ss []demandSample, now time.Time, w time.Duration) Resources {
	from := now.Add(-w)
	var cpu, mem float64
	for i, s := range ss {
		end := now
		if i+1 < len(ss) {
			end = ss[i+1].at
		}
		start := s.at
		if start.Before(from) {
			start = from
		}
		if d := end.Sub(start).Seconds(); d > 0 {
			cpu += s.res.CPU * d
			mem += float64(s.res.Memory) * d
		}
	}
	return Resources{CPU: cpu / w.Seconds(), Memory: int64(mem / w.Seconds())}
}

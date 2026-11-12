package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// tlMachine is a machine in the scale-out timeline.
type tlMachine struct {
	m       Machine
	ready   time.Time
	deltas  []Release // future changes of the free capacity
	free    Resources // free capacity at ready
	price   float64
	created bool          // hypothetical new or started machine
	latency time.Duration // its start or boot latency
	lastEnd time.Time     // end of its last planned session
	planned int           // sessions the last timeline planned on it
	guess   *Release      // release guessed (DefaultRun) for reservations without a lease estimate
}

// earliest returns the first time >= tm.ready at which need fits for run.
func (tm *tlMachine) earliest(need Resources, run time.Duration) (time.Time, bool) {
	ds := tm.deltas
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].At.Before(ds[j].At) })
	// after[i] is the free capacity after the deltas up to i.
	after := make([]Resources, len(ds))
	free := tm.free
	for i, d := range ds {
		free = free.Add(d.Res)
		after[i] = free
	}
	at := func(t time.Time) Resources { // free capacity at t
		r := tm.free
		for i, d := range ds {
			if d.At.After(t) {
				break
			}
			r = after[i]
		}
		return r
	}
	try := func(s time.Time, from int) bool {
		if !at(s).Fits(need) {
			return false
		}
		end := s.Add(run)
		for i := from; i < len(ds) && ds[i].At.Before(end); i++ {
			if ds[i].At.After(s) && !after[i].Fits(need) {
				return false
			}
		}
		return true
	}
	if try(tm.ready, 0) {
		return tm.ready, true
	}
	for i, d := range ds {
		if d.At.After(tm.ready) && try(d.At, i) {
			return d.At, true
		}
	}
	return time.Time{}, false
}

// quickRun is the expected run of t on a timeline machine, including
// thrashing when t's working set exceeds the machine's local tier (so new
// machines of sizes with more local capacity win against thrashing ones).
func (p Default) quickRun(f Pool, tm *tlMachine, t Task) time.Duration {
	pen, _, _ := thrashOf(f, tm.m, t)
	if tm.m.Warm(t.RepoID) {
		return t.Profile.WarmRun() + pen
	}
	run := t.Profile.ColdRun(tm.m.Provider)
	if s, ok := p.bestSnapshot(f, tm.m.Provider, t); ok {
		_, bw := p.egress(f, s.Store, tm.m.Provider)
		if r := restoreTime(tm.m, t, s.Bytes, bw) + t.Profile.WarmRun(); r < run {
			run = r
		}
	}
	return run + pen
}

// hung reports whether a starting machine overran its expected readiness
// by so much (twice its boot, at least 3 minutes) that it no longer counts
// as capacity on its way.
func (p Default) hung(f Pool, m Machine) bool {
	return m.State == Starting && !m.ReadyAt.IsZero() && f.Now.Sub(m.ReadyAt) > max(2*p.boot(f, m.Provider), 3*time.Minute)
}

// candOption describes a start or create candidate of scale-out.
func candOption(a Action, tm *tlMachine) *Option {
	o := &Option{Kind: OptStart, Machine: a.Machine, Provider: tm.m.Provider, Size: tm.m.Size, Volume: a.Volume,
		LatencySeconds: secs(tm.latency), Reason: a.Reason}
	if a.Kind == Create {
		o.Kind, o.Provider, o.Size = OptCreate, a.Size.Provider, a.Size.Name
	}
	return o
}

// warmVolumes returns the detached volumes a new machine of size s could
// attach that hold a warm slot of one of tasks (not yet taken by a create
// of this plan).
func (p Default) warmVolumes(st *planState, s Size, tasks []Task) []Volume {
	if s.Medium != medium.Volume {
		return nil
	}
	var out []Volume
	for _, v := range st.f.Volumes {
		if v.Provider != s.Provider || (!v.ExpiresAt.IsZero() && !st.f.Now.Before(v.ExpiresAt)) {
			continue
		}
		if slicesAny(st.acts, func(a Action) bool { return a.Volume == v.ID }) {
			continue
		}
		for _, t := range tasks {
			if v.Warm(t.RepoID) {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

func slicesAny(as []Action, fn func(Action) bool) bool {
	for _, a := range as {
		if fn(a) {
			return true
		}
	}
	return false
}

// timeValue is what an hour of turnaround is worth in money for env's
// decisions: the price of env's cheapest size times (1-r)/r
// (r = cost_optimization). At r = 0.5 an hour of waiting weighs as much
// as an hour of the cheapest machine, at 0.2 four times as much; r = 0
// values only time (+Inf), r = 1 only money (0). ok is false when no size
// has a price. (The cost model normalised money and time against the
// cheapest and fastest option of each decision: then a few cents more for
// a start weighed like minutes of waiting.)
func (p Default) timeValue(f Pool, env string) (float64, bool) {
	pmin := 0.0
	for _, s := range f.ArchSizes(env) {
		if pr := sizePrice(s); pr > 0 && (pmin == 0 || pr < pmin) {
			pmin = pr
		}
	}
	if pmin <= 0 {
		return 0, false
	}
	r := p.costOptimization()
	if r <= 0 {
		return math.Inf(1), true
	}
	return pmin * (1 - r) / r, true
}

// physical reports whether t may run on a machine of capacity c: the
// option policy never places a task whose request exceeds a machine's
// physical cores or memory (overcommit is for the sum of reservations),
// unless nothing of its environment fits it physically.
func (p Default) physical(f Pool, c Resources, t Task) bool {
	if c.Fits(t.Request) {
		return true
	}
	for _, s := range f.ArchSizes(t.EnvHash) {
		if s.Capacity.Fits(t.Request) {
			return false
		}
	}
	for _, m := range f.Machines {
		if m.EnvHash == t.EnvHash && m.State != Gone && m.Capacity.Fits(t.Request) {
			return false
		}
	}
	return true
}

// hedgeMargin is how much longer than getting a machine (start or create)
// the option policy lets a task wait for capacity: 30s / (1 - r), i.e. 30s
// at r = 0, 1m at 0.5, 5m at 0.9 and unbounded at 1.
func (p Default) hedgeMargin() (time.Duration, bool) {
	r := p.costOptimization()
	if r >= 1 {
		return 0, false
	}
	return time.Duration(float64(30*time.Second) / (1 - r)), true
}

// quantileEnd is the q-quantile of r's end given that it still runs at
// now: log-normal run time with median r.Run and 80th percentile
// r.At - r.Start.
func quantileEnd(r Release, q float64, now time.Time) time.Time {
	med := r.Run.Seconds()
	sigma := math.Log(r.At.Sub(r.Start).Seconds()/med) / 0.8416
	sigma = math.Min(math.Max(sigma, 0.05), 1.5)
	ran := now.Sub(r.Start).Seconds()
	f0 := 0.0
	if ran > 0 {
		f0 = 0.5 * math.Erfc(-(math.Log(ran/med))/(sigma*math.Sqrt2))
	}
	u := math.Min(f0+q*(1-f0), 1-1e-9)
	d := med * math.Exp(sigma*math.Sqrt2*math.Erfinv(2*u-1))
	end := r.Start.Add(time.Duration(d * float64(time.Second)))
	if !end.After(now) {
		end = now.Add(time.Second)
	}
	return end
}

// copyCost is the storage of a second warm copy of t's slot (bytes) on m
// while another machine keeps its own (default policy): the disk until m
// expires, at most a day.
func (p Default) copyCost(f Pool, m Machine, t Task, bytes int64) float64 {
	if bytes <= 0 {
		return 0
	}
	for _, o := range f.Machines {
		if o.ID != m.ID && o.State != Gone && o.Warm(t.RepoID) {
			left := m.ExpiresAt.Sub(f.Now)
			if m.ExpiresAt.IsZero() || left <= 0 || left > 24*time.Hour {
				left = 24 * time.Hour
			}
			return float64(bytes) / 1e9 * p.diskPriceOf(m) * hours(left)
		}
	}
	return 0
}

// owner is who a task belongs to for fair queueing.
func owner(t Task) string {
	if t.Owner != "" {
		return t.Owner
	}
	return t.RepoID
}

// prioritize orders tasks for the default policy: shortest predicted run
// first, aged by waiting (a task that waited as long as it runs counts
// half), owners served round robin (deficit by tasks taken), oldest first
// on ties. A 20 s check does not sit behind a 15 min archive, and no
// owner starves.
func prioritize(now time.Time, ts []Task) []Task {
	key := func(t Task) float64 {
		run := t.Profile.WarmRun().Seconds()
		return run / (1 + now.Sub(t.Since).Seconds()/math.Max(run, 1))
	}
	rest := append([]Task(nil), ts...)
	served := map[string]int{}
	out := make([]Task, 0, len(ts))
	for len(rest) > 0 {
		best := 0
		for i := 1; i < len(rest); i++ {
			a, b := rest[i], rest[best]
			sa, sb := served[owner(a)], served[owner(b)]
			switch {
			case sa != sb:
				if sa < sb {
					best = i
				}
			case math.Abs(key(a)-key(b)) > 1e-9:
				if key(a) < key(b) {
					best = i
				}
			case a.Since.Before(b.Since):
				best = i
			}
		}
		served[owner(rest[best])]++
		out = append(out, rest[best])
		rest = append(rest[:best:best], rest[best+1:]...)
	}
	return out
}

// Queue confidence (default policy): a request waits for capacity (a busy
// warm machine, a starting machine, the capacity already on its way) only
// when the scheduler is confident that waiting finishes it sooner than
// running it now (a cold or restored slot on a machine with room, a start
// or a create):
//
//	P(finish_wait < finish_now + slack) >= queue_confidence (0.8)
//
// finish_wait is the wait (the remaining run times of the sessions it
// waits for: log-normal with the key's median and 80th percentile,
// conditional on having run until now, comonotone across sessions) plus
// the request's run there; finish_now the latency of the alternative
// (start/boot, deterministic) plus its run. The request's own run time is
// uncertain too, but in the same way for both (one build, comonotone).
// A session whose remaining time is unknown (no lease estimate) is not
// counted on to end. slack = (cost_now - cost_wait) / time_value lets
// cost_optimization bias the decision: at 0 only time counts, at 1 a
// cheaper wait is always taken.

// DefaultQueueConfidence is queue_confidence when unset.
const DefaultQueueConfidence = 0.8

// releaseQ is r's end at quantile q of its remaining time; ok is false
// when its remaining time is unknown (no lease estimate).
func releaseQ(r Release, q float64, now time.Time) (time.Time, bool) {
	if r.Unknown || r.At.IsZero() {
		return time.Time{}, false
	}
	if r.Run <= 0 || r.Start.IsZero() || !r.At.After(r.Start.Add(r.Run)) {
		// No spread known: the point ETA, until the session overruns it
		// (then nothing is known about its end).
		if r.At.Before(now) {
			return time.Time{}, false
		}
		return r.At, true
	}
	return quantileEnd(r, q, now), true
}

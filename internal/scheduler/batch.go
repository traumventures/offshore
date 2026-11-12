package scheduler

import (
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// The default policy decides the queue of an environment as a batch: a
// request's placement depends on the others waiting with it. Placing
// requests one by one in arrival order lets the first request take the
// machine another one is warm on (it runs cold there, the other one
// waits or runs cold elsewhere) or the last free slot a request without
// any other option needed. placeBatch orders the queued requests of the
// environment by regret instead (the score lost when a request does not
// get its best option: the gap between its best and its best option on
// another machine or waiting), lets each take its best option on the
// state the requests before it left (their reservations added, their
// sessions as releases), and returns the requested task's decision.
// Every request is decided with the options and invariants of
// placeScored; only the order and the state differ.

// StopDecay is the least time between two idle stops of an
// environment's machines (default policy).
const StopDecay = time.Minute

// batchSize bounds the requests decided together (the oldest of the
// environment's queue plus the requested task).
const batchSize = 8

// placeBatch returns t's placement in the regret-ordered batch of the
// queued requests of its environment (false: t is alone).
func (p Default) placeBatch(f Pool, t Task) (Placement, bool) {
	// Only requests that could run now compete: the others wait anyway
	// (scale-out decides for them).
	runnable := func(q Task) bool {
		for _, m := range f.Machines {
			if p.accepting(m, q.EnvHash, f.Now) && p.physical(f, m.Capacity, q) && p.admits(f, m, q) {
				return true
			}
		}
		return false
	}
	if !runnable(t) {
		return Placement{}, false
	}
	var others []Task
	scanned := 0
	for _, q := range f.Queue {
		if q.ID == t.ID || q.EnvHash != t.EnvHash {
			continue
		}
		if scanned++; scanned > 4*batchSize {
			break // bounded work per placement on long queues
		}
		if runnable(q) {
			others = append(others, q)
		}
	}
	if len(others) == 0 {
		return Placement{}, false
	}
	sort.SliceStable(others, func(i, j int) bool { return others[i].Since.Before(others[j].Since) })
	if len(others) > batchSize-1 {
		others = others[:batchSize-1]
	}
	tasks := append([]Task{t}, others...)
	if !exp("regret") {
		// Priority order (SRPT, aging, owners round robin): every task
		// takes its best option on the state the ones before it left.
		st := f
		st.Machines = make([]Machine, len(f.Machines))
		for i, m := range f.Machines {
			m.Releases = append([]Release(nil), m.Releases...)
			m.WarmRepos = append([]string(nil), m.WarmRepos...)
			st.Machines[i] = m
		}
		for _, q := range prioritize(f.Now, tasks) {
			pl, _, _ := p.placeScored(st, q)
			if q.ID == t.ID {
				return pl, true
			}
			p.book(&st, q, pl)
		}
	}
	st := f
	st.Machines = make([]Machine, len(f.Machines))
	for i, m := range f.Machines {
		m.Releases = append([]Release(nil), m.Releases...)
		m.WarmRepos = append([]string(nil), m.WarmRepos...)
		st.Machines[i] = m
	}
	lam, ok := p.timeValue(f, t.EnvHash)
	if !ok {
		return Placement{}, false
	}
	score := absScore(lam)
	for len(tasks) > 0 {
		best, bestRegret := -1, -1.0
		var bestPl Placement
		for i, q := range tasks {
			pl, opts, chosen := p.placeScored(st, q)
			r := regret(opts, chosen, score)
			if best < 0 || r > bestRegret+1e-12 || math.Abs(r-bestRegret) <= 1e-12 && q.Since.Before(tasks[best].Since) {
				best, bestRegret, bestPl = i, r, pl
			}
		}
		q := tasks[best]
		if q.ID == t.ID {
			return bestPl, true
		}
		p.book(&st, q, bestPl)
		tasks = append(tasks[:best:best], tasks[best+1:]...)
	}
	return Placement{}, false
}

// book adds an assigned placement of q to the machine in st.
func (p Default) book(st *Pool, q Task, pl Placement) {
	if pl.Kind != Assign {
		return
	}
	for i := range st.Machines {
		m := &st.Machines[i]
		if m.ID != pl.Machine {
			continue
		}
		m.Reserved = m.Reserved.Add(q.Request)
		m.Sessions++
		m.IdleSince = time.Time{}
		m.Releases = append(m.Releases, Release{At: st.Now.Add(pl.Expected), Res: q.Request, Start: st.Now, Run: pl.Run,
			MemMean: q.Profile.PeakMean, MemSD: q.Profile.PeakSD})
		if !m.Warm(q.RepoID) {
			m.WarmRepos = append(m.WarmRepos, q.RepoID)
		}
	}
}

// abl disables parts of the default policy for ablations in the
// simulator (OFFSHORE_ABLATE=nogate).
func abl(part string) bool {
	return strings.Contains(","+os.Getenv("OFFSHORE_ABLATE")+",", ","+part+",")
}

// exp enables experimental parts of the default policy that did not win
// in the simulator (OFFSHORE_EXPERIMENT=batch,regret,dist,stat,decay; see
// docs/scheduling.md, round 2): batch placement, its regret order, the
// remaining-time distribution of waits, statistical memory admission, one
// idle stop per environment and StopDecay.
func exp(part string) bool {
	return strings.Contains(","+os.Getenv("OFFSHORE_EXPERIMENT")+",", ","+part+",")
}

// regret is what a request loses when it does not get its chosen option:
// the score of its best option on another machine (or a wait, if the
// chosen one runs now) minus the chosen one's; +Inf without an
// alternative, 0 without options (it waits for scale-out anyway).
func regret(opts []option, chosen int, score func(option) float64) float64 {
	if chosen < 0 || len(opts) == 0 {
		return 0
	}
	c := opts[chosen]
	alt := math.Inf(1)
	for i, o := range opts {
		if i == chosen || o.info != nil && c.info != nil && o.info.Machine == c.info.Machine && o.info.Kind == c.info.Kind {
			continue
		}
		alt = math.Min(alt, score(o))
	}
	return alt - score(c)
}

// waitDist is the expected time until need fits on m: the mean over
// quantiles of the remaining run times of its sessions (log-normal with
// the median Release.Run and the 80th percentile Release.At, conditional
// on having run until now; comonotone across sessions). ok is false when
// a release has no distribution (the caller keeps the point ETA).
func (p Default) waitDist(f Pool, m Machine, need Resources) (time.Duration, bool) {
	if m.State != Running || p.Free(m).Fits(need) {
		return 0, false
	}
	for _, r := range m.Releases {
		if r.Run <= 0 || r.Start.IsZero() || !r.At.After(r.Start.Add(r.Run)) {
			return 0, false
		}
	}
	const n = 9
	var sum time.Duration
	for k := 0; k < n; k++ {
		q := (float64(k) + 0.5) / n
		mm := m
		mm.Releases = make([]Release, len(m.Releases))
		for i, r := range m.Releases {
			r.At = quantileEnd(r, q, f.Now)
			mm.Releases[i] = r
		}
		at, ok := p.freeAt(f, mm, need)
		if !ok {
			return 0, false
		}
		sum += max(at.Sub(f.Now), 0)
	}
	return sum / n, true
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
	if !p.global() || bytes <= 0 {
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

// memBelief is a session's peak memory belief: mean and standard
// deviation (unknown: the reservation, no spread).
func memBelief(res Resources, mean, sd int64) (float64, float64) {
	if mean <= 0 {
		return float64(res.Memory), 0
	}
	return float64(mean), float64(sd)
}

// admitZ is the risk factor of statistical admission: about 3.1 standard
// deviations (1e-3) when time counts, 2.3 (1e-2) when only money does.
func (p Default) admitZ() float64 { return 3.1 - 0.8*p.costOptimization() }

// admits reports whether t may run on m now. The default policy admits
// memory by reservation, and beyond that statistically: the sessions' and t's mean peaks plus admitZ
// standard deviations of their sum fit the machine's memory (peaks are
// short and rarely coincide; summing per-session upper quantiles wastes
// memory), and every reservation fits physically (physical). CPU is
// admitted by reservation (overcommit).
func (p Default) admits(f Pool, m Machine, t Task) bool {
	if !p.global() || !exp("stat") {
		return p.Free(m).Fits(t.Request)
	}
	if p.Free(m).Fits(t.Request) { // the reservations fit: never stricter
		return true
	}
	adm := p.admissible(m)
	if m.Reserved.CPU+t.Request.CPU > adm.CPU+1e-9 {
		return false
	}
	mean, v := memBelief(t.Request, t.Profile.PeakMean, t.Profile.PeakSD)
	v *= v
	covered := int64(0)
	for _, r := range m.Releases {
		mu, sd := memBelief(r.Res, r.MemMean, r.MemSD)
		mean += mu
		v += sd * sd
		covered += r.Res.Memory
	}
	if rest := m.Reserved.Memory - covered; rest > 0 { // pending leases
		mean += float64(rest)
	}
	return mean+p.admitZ()*math.Sqrt(v) <= float64(adm.Memory)
}

// project list-schedules tasks (in order) on the timeline machines and
// returns each task's projected start (missing: never within the
// machines' capacity). Reservations without a lease estimate are not
// counted on to end: a request starts soon only if that is known. Each task takes the slot with the earliest
// completion (warm, restore or cold run on that machine).
func (p Default) project(f Pool, ms []*tlMachine, tasks []Task) map[string]time.Time {
	out := map[string]time.Time{}
	for id, pr := range p.projectRuns(f, ms, tasks) {
		out[id] = pr.at
	}
	return out
}

// projectRuns is project with each task's run on the machine it takes.
func (p Default) projectRuns(f Pool, ms []*tlMachine, tasks []Task) map[string]projection {
	saved := make([][]Release, len(ms))
	for i, tm := range ms {
		saved[i] = tm.deltas
		tm.deltas = append([]Release(nil), tm.deltas...)
		if g := tm.guess; g != nil { // unknown remaining time: no release to count on
			for j, d := range tm.deltas {
				if d == *g {
					tm.deltas[j].At = f.Now.Add(24 * time.Hour)
					break
				}
			}
		}
	}
	starts := map[string]projection{}
	for _, t := range tasks {
		var bestTM *tlMachine
		var bestAt time.Time
		var bestRun time.Duration
		for _, tm := range ms {
			if !p.admissible(tm.m).Fits(t.Request) || !p.physical(f, tm.m.Capacity, t) {
				continue
			}
			run := p.quickRun(f, tm, t)
			at, ok := tm.earliest(t.Request, run)
			if ok && (bestTM == nil || at.Add(run).Before(bestAt.Add(bestRun))) {
				bestTM, bestAt, bestRun = tm, at, run
			}
		}
		if bestTM == nil {
			continue
		}
		starts[t.ID] = projection{bestAt, bestRun}
		bestTM.deltas = append(bestTM.deltas, Release{At: bestAt, Res: Resources{}.Sub(t.Request)}, Release{At: bestAt.Add(bestRun), Res: t.Request})
	}
	for i, tm := range ms {
		tm.deltas = saved[i]
	}
	return starts
}

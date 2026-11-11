package scheduler

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// placeModel decides with the cost model: run now on a machine with free
// capacity (warm slot, restored snapshot or cold, after pruning or
// resizing a full disk), or wait for a machine (a warm slot freeing soon, a
// machine starting) when that scores better.
func (p Default) placeModel(f Pool, t Task) Placement {
	if !p.fitsAnySize(f, t) {
		return Placement{Kind: Reject, Reason: "task larger than every machine size"}
	}
	var opts []option
	var pls []Placement
	consider := func(o option, cand Placement) {
		opts, pls = append(opts, o), append(pls, cand)
	}
	assignable := false
	for _, m := range f.Machines {
		if !p.accepting(m, t.EnvHash, f.Now) || !p.Free(m).Fits(t.Request) {
			continue
		}
		for _, rc := range p.runChoices(f, m, t) {
			assignable = true
			o := option{cost: p.machineCost(f, m, t, f.Now, rc.run) + rc.cost, time: rc.run, tie: Score(t.RepoID, m.ID),
				fit: leftover(p.Free(m), t.Request, p.admissible(m))}
			consider(o, Placement{Kind: Assign, Machine: m.ID, Grant: p.Grant(t), Reason: rc.reason, Slot: rc.slot, Expected: t.Profile.High(rc.run)})
		}
	}
	if !assignable {
		return Placement{Kind: Wait, Reason: "no machine with free capacity"}
	}
	// Waiting for a busy or starting machine of the environment.
	for _, m := range f.Machines {
		if m.EnvHash != t.EnvHash || m.Draining || !p.usable(m, f.Now) {
			continue
		}
		if !(m.State == Starting || m.State == Running && !p.Free(m).Fits(t.Request)) {
			continue
		}
		at, ok := p.freeAt(f, m, t.Request)
		if !ok {
			continue
		}
		wait := at.Sub(f.Now)
		if p.Cfg.QueueTimeout > 0 && f.Now.Sub(t.Since)+wait > p.Cfg.QueueTimeout*8/10 {
			continue
		}
		mm := m
		mm.Reserved = Resources{} // runs after the sessions it waits for
		for _, rc := range p.runChoices(f, mm, t) {
			o := option{cost: p.machineCost(f, m, t, at, rc.run) + rc.cost, time: wait + rc.run, tie: Score(t.RepoID, m.ID), fit: math.Inf(1)}
			consider(o, Placement{Kind: Wait, Machine: m.ID, Reason: fmt.Sprintf("waiting %s for %s on %s", wait.Round(time.Second), rc.reason, m.ID)})
		}
	}
	pl := pls[p.pick(opts)]
	if pl.Kind == Wait {
		pl.Machine = ""
	}
	return pl
}

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

// quickRun is the expected run of t on a timeline machine.
func (p Default) quickRun(f Pool, tm *tlMachine, t Task) time.Duration {
	if tm.m.Warm(t.RepoID) {
		return t.Profile.WarmRun()
	}
	run := t.Profile.ColdRun(tm.m.Provider)
	if s, ok := p.bestSnapshot(f, tm.m.Provider, t); ok {
		_, bw := p.egress(f, s.Store, tm.m.Provider)
		if r := restoreTime(tm.m, s.Bytes, bw) + t.Profile.WarmRun(); r < run {
			run = r
		}
	}
	return run
}

// ownMachine is the cost of the cheapest size fitting t for its cold run.
func (p Default) ownMachine(f Pool, t Task) float64 {
	best := 0.0
	for _, s := range f.ArchSizes(t.EnvHash) {
		if p.sizeAdmissible(s).Fits(t.Request) && s.PricePerHour > 0 && (best == 0 || s.PricePerHour < best) {
			best = s.PricePerHour
		}
	}
	return best * hours(t.Profile.ColdRun("")+DefaultBoot)
}

// infWait stands in for a wait that never ends when scoring.
func (p Default) infWait() time.Duration {
	return max(2*p.Cfg.QueueTimeout, 2*time.Hour)
}

// timeline assigns tasks (in order) to the earliest fitting slot on the
// machines and returns the total cost and time of the plan and whether
// every task gets capacity within the queue timeout. New machines cost
// their whole price from now until their last session ends plus tail.
func (p Default) timeline(f Pool, ms []*tlMachine, tasks []Task, tail time.Duration) (cost float64, total time.Duration, feasible bool) {
	feasible = true
	for _, tm := range ms {
		tm.planned = 0
		tm.lastEnd = f.Now
		if tm.created {
			tm.lastEnd = tm.ready
		}
		for _, d := range tm.deltas {
			if d.At.After(tm.lastEnd) {
				tm.lastEnd = d.At
			}
		}
	}
	saved := make([][]Release, len(ms))
	for i, tm := range ms {
		saved[i] = tm.deltas
		tm.deltas = append([]Release(nil), tm.deltas...)
	}
	for _, t := range tasks {
		var bestTM *tlMachine
		var bestAt time.Time
		var bestRun time.Duration
		for _, tm := range ms {
			if !tm.m.Capacity.Scale(p.cfg().CPUOvercommit, p.cfg().MemoryOvercommit).Fits(t.Request) {
				continue
			}
			run := p.quickRun(f, tm, t)
			at, ok := tm.earliest(t.Request, run)
			if !ok {
				continue
			}
			if bestTM == nil || at.Add(run).Before(bestAt.Add(bestRun)) {
				bestTM, bestAt, bestRun = tm, at, run
			}
		}
		wait := time.Duration(0)
		if bestTM == nil {
			// Never served by these machines: it will need a machine of its
			// own (the cheapest fitting size for its run) after a long wait.
			feasible = false
			total += p.infWait()
			cost += p.ownMachine(f, t)
			continue
		}
		wait = bestAt.Sub(f.Now)
		if p.Cfg.QueueTimeout > 0 && f.Now.Sub(t.Since)+wait > p.Cfg.QueueTimeout*8/10 {
			feasible = false
		}
		total += wait + bestRun
		end := bestAt.Add(bestRun)
		bestTM.deltas = append(bestTM.deltas, Release{At: bestAt, Res: Resources{}.Sub(t.Request)}, Release{At: end, Res: t.Request})
		if end.After(bestTM.lastEnd) {
			bestTM.lastEnd = end
		}
		bestTM.planned++
	}
	// The pool pays every machine until its last session ends; new ones
	// also for their latency and idle tail (at least a minute).
	for i, tm := range ms {
		span := tm.lastEnd.Sub(f.Now)
		if tm.created {
			span = max(span+tail, time.Minute)
		}
		cost += tm.price * hours(span)
		tm.deltas = saved[i]
	}
	return cost, total, feasible
}

// tlOf builds the timeline view of an existing machine.
func (p Default) tlOf(f Pool, m Machine) *tlMachine {
	tm := &tlMachine{m: m, ready: f.Now, price: priceOf(f, m)}
	if m.State == Starting {
		tm.ready = m.ReadyAt
		if tm.ready.IsZero() || tm.ready.Before(f.Now) {
			tm.ready = f.Now.Add(p.startLatency(f, m.Provider) / 2)
		}
	}
	tm.free = p.admissible(m).Sub(m.Reserved)
	covered := Resources{}
	for _, r := range m.Releases {
		at := r.Expected(f.Now)
		tm.deltas = append(tm.deltas, Release{At: at, Res: r.Res})
		covered = covered.Add(r.Res)
	}
	if rest := m.Reserved.Sub(covered); rest.CPU > 1e-9 || rest.Memory > 0 {
		tm.deltas = append(tm.deltas, Release{At: f.Now.Add(DefaultRun), Res: Resources{CPU: math.Max(rest.CPU, 0), Memory: max(rest.Memory, 0)}})
	}
	return tm
}

// scaleOutModel gets capacity for the queued tasks of env: it undrains
// consolidated machines first (free and immediate), then starts stopped
// machines or creates new ones while that scores better than waiting for
// the capacity already there or on its way. Waiting for sessions that end
// soon beats a machine that boots longer: no create right after a drain.
func (p Default) scaleOutModel(st *planState, env string) {
	c, f := p.cfg(), st.f
	tasks := append([]Task(nil), st.queued[env]...)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Since.Before(tasks[j].Since) })
	// Bounded work per plan: the oldest tasks, a few machines per plan
	// (plans run every few seconds while tasks wait).
	if len(tasks) > 16 {
		tasks = tasks[:16]
	}
	tail := time.Duration(float64(c.IdleStop) * DefaultTail)
	var ms []*tlMachine
	for _, m := range f.Machines {
		if m.EnvHash != env || !p.usable(m, f.Now) || (m.State != Running && m.State != Starting) {
			continue
		}
		if m.State == Running && m.Draining {
			st.acts = append(st.acts, Action{Kind: Undrain, Machine: m.ID, Reason: "queued demand"})
			m.Draining = false
		} else if m.Draining {
			continue
		}
		ms = append(ms, p.tlOf(f, m))
	}
	type cand struct {
		act Action
		tm  *tlMachine
	}
	stopped := st.stopped[env]
	// Stopped machines with the slots of the first tasks first.
	rv := map[string]string{}
	for _, m := range stopped {
		rv[m.ID] = Score(tasks[0].RepoID, m.ID)
	}
	sort.SliceStable(stopped, func(i, j int) bool { return rv[stopped[i].ID] > rv[stopped[j].ID] })
	for round := 0; round < min(len(tasks), 4); round++ {
		cost, total, feasible := p.timeline(f, ms, tasks, tail)
		var opts []option
		var optC []*cand
		if feasible {
			opts, optC = append(opts, option{cost: cost, time: total, tie: "~"}), append(optC, nil)
		}
		var cands []cand
		if st.active < c.MaxMachines || p.freeSlotPossible(st, env) {
			for _, m := range stopped[:min(len(stopped), 3)] {
				tm := p.tlOf(f, m)
				tm.m.State, tm.created, tm.price = Starting, true, priceOf(f, m)
				tm.latency = p.startLatency(f, m.Provider)
				tm.ready, tm.free, tm.deltas = f.Now.Add(tm.latency), p.admissible(m), nil
				cands = append(cands, cand{Action{Kind: Start, Machine: m.ID, Reason: "queued demand"}, tm})
			}
			for _, s := range p.allowedSizes(f.SizesFor(env), st.perProvider) {
				if s.Capacity.CPU <= 0 || s.Capacity.Memory <= 0 {
					continue
				}
				m := Machine{ID: "new:" + s.Provider + "/" + s.Name, Provider: s.Provider, EnvHash: env, Size: s.Name, State: Starting, Capacity: s.Capacity,
					Medium: s.Medium, Stack: s.Stack, DiskTotal: max(s.Volume.Min, s.Capacity.Disk)}
				lat := p.boot(f, s.Provider)
				tm := &tlMachine{m: m, ready: f.Now.Add(lat), free: p.sizeAdmissible(s), price: sizePrice(s), created: true, latency: lat}
				cands = append(cands, cand{Action{Kind: Create, EnvHash: env, Size: s, Reason: "queued demand"}, tm})
				// A detached volume with warm slots of the queued tasks:
				// a machine in its zone gets them for the attach time.
				for _, v := range p.warmVolumes(st, s, tasks) {
					vm := m
					vm.ID, vm.Zone, vm.Volume, vm.DiskTotal = "new:"+s.Provider+"/"+s.Name+"/"+v.ID, v.Zone, v.ID, v.Bytes
					vm.SlotBytes = v.SlotBytes
					for repo := range v.SlotBytes {
						vm.WarmRepos = append(vm.WarmRepos, repo)
					}
					sort.Strings(vm.WarmRepos)
					vlat := lat + s.Stack.Hot.Attach
					vtm := &tlMachine{m: vm, ready: f.Now.Add(vlat), free: p.sizeAdmissible(s), price: priceOf(f, vm), created: true, latency: vlat}
					cands = append(cands, cand{Action{Kind: Create, EnvHash: env, Size: s, Volume: v.ID, Reason: "queued demand, warm volume " + v.ID}, vtm})
				}
			}
		}
		for i := range cands {
			cd := &cands[i]
			cost, total, _ := p.timeline(f, append(ms, cd.tm), tasks, tail)
			if cd.tm.planned == 0 {
				continue // takes none of the tasks
			}
			opts, optC = append(opts, option{cost: cost, time: total, tie: Score(env, cd.tm.m.ID)}), append(optC, cd)
		}
		var bestC *cand
		if i := p.pick(opts); i >= 0 {
			bestC = optC[i]
		}
		if bestC == nil {
			if !feasible && len(cands) == 0 && st.active < c.MaxMachines {
				// Providers at their machine limit: stopped machines of
				// other environments hold their slots.
				p.makeRoom(st, env)
			}
			return
		}
		if st.active >= c.MaxMachines && !p.freeSlot(st, env, tasks[0].Request) {
			return
		}
		st.acts = append(st.acts, bestC.act)
		st.active++
		if bestC.act.Kind == Create {
			st.perProvider[bestC.act.Size.Provider]++
		} else {
			for i, m := range stopped {
				if m.ID == bestC.act.Machine {
					stopped = append(stopped[:i:i], stopped[i+1:]...)
					break
				}
			}
		}
		bestC.tm.created = true
		ms = append(ms, bestC.tm)
	}
	st.stopped[env] = stopped
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

// freeSlotPossible reports whether freeSlot could stop a machine for env.
func (p Default) freeSlotPossible(st *planState, env string) bool {
	for _, m := range st.f.Machines {
		if m.State == Running && m.Sessions == 0 && m.Reserved == (Resources{}) {
			return true
		}
	}
	return false
}

// flapGuard is how much consolidation guards against draining capacity the
// next minutes need again (forecast peak, hold after a scale-up): fully up
// to cost_optimization 0.5, fading out towards 1. A drain followed by a
// create or start costs boot latency and a cold cache (time), while keeping
// a lightly used machine costs its price (money): at 1 only money counts
// and the pool consolidates on the present reservation.
func (p Default) flapGuard() float64 {
	return math.Min(1, 2*(1-p.costOptimization()))
}

// forecastReserved is the reservation consolidation plans for: the present
// reservation raised towards the peak of the forecast window by the flap
// guard.
func (p Default) forecastReserved(f Pool, env string, now Resources) Resources {
	d, ok := f.Demand[env]
	if !ok {
		return now
	}
	g := p.flapGuard()
	peak := Resources{CPU: math.Max(now.CPU, d.Peak.CPU), Memory: max(now.Memory, d.Peak.Memory)}
	return Resources{CPU: now.CPU + g*(peak.CPU-now.CPU), Memory: now.Memory + int64(g*float64(peak.Memory-now.Memory))}
}

// othersAbsorb reports whether the other running machines of m's
// environment can take the forecast demand (below ConsolidateTarget).
func (p Default) othersAbsorb(f Pool, m Machine) bool {
	c := p.cfg()
	var reserved, capacity Resources
	for _, o := range f.Machines {
		if o.EnvHash != m.EnvHash || o.State != Running || !p.usable(o, f.Now) {
			continue
		}
		reserved = reserved.Add(o.Reserved)
		if o.ID != m.ID && !o.Draining {
			capacity = capacity.Add(p.admissible(o))
		}
	}
	if capacity.CPU <= 0 {
		return false
	}
	return share(p.forecastReserved(f, m.EnvHash, reserved), capacity) <= c.ConsolidateTarget
}

// consolidateModel is consolidate on the forecast: it drains only when the
// peak demand of the window fits on the remaining machines, and not right
// after the environment scaled up.
func (p Default) consolidateModel(st *planState, running map[string]int) {
	c, f := p.cfg(), st.f
	if c.ConsolidateBelow <= 0 {
		return
	}
	byEnv := map[string][]Machine{}
	var envs []string
	draining := map[string]bool{}
	guarded := p.flapGuard() > 0
	for _, m := range f.Machines {
		if m.State == Starting && guarded {
			draining[m.EnvHash] = true // still scaling up
		}
		if m.State != Running || !p.usable(m, f.Now) {
			continue
		}
		if m.Draining {
			draining[m.EnvHash] = true
			continue
		}
		if _, ok := byEnv[m.EnvHash]; !ok {
			envs = append(envs, m.EnvHash)
		}
		byEnv[m.EnvHash] = append(byEnv[m.EnvHash], m)
	}
	sort.Strings(envs)
	for _, env := range envs {
		ms := byEnv[env]
		if len(ms) < 2 || draining[env] || len(st.queued[env]) > 0 || running[env]-1 < p.warmTarget(f, env) {
			continue
		}
		hold := time.Duration(p.flapGuard() * float64(c.ConsolidateHold))
		if d, ok := f.Demand[env]; ok && !d.LastScaleUp.IsZero() && f.Now.Sub(d.LastScaleUp) < hold {
			continue
		}
		if guarded && slicesAny(st.acts, func(a Action) bool { return a.Kind == Undrain || a.Kind == Start || a.Kind == Create }) {
			continue
		}
		var reserved, capacity Resources
		for _, m := range ms {
			reserved = reserved.Add(m.Reserved)
			capacity = capacity.Add(p.admissible(m))
		}
		demand := p.forecastReserved(f, env, reserved)
		if share(demand, capacity) >= c.ConsolidateBelow {
			continue
		}
		var victim *Machine
		for i := range ms {
			m := &ms[i]
			if m.Sessions == 0 {
				continue // empty machines idle stop on their own
			}
			if victim == nil || share(m.Reserved, p.admissible(*m)) < share(victim.Reserved, p.admissible(*victim)) ||
				(share(m.Reserved, p.admissible(*m)) == share(victim.Reserved, p.admissible(*victim)) && m.ID < victim.ID) {
				victim = m
			}
		}
		if victim == nil {
			continue
		}
		if share(demand, capacity.Sub(p.admissible(*victim))) > c.ConsolidateTarget {
			continue
		}
		st.acts = append(st.acts, Action{Kind: Drain, Machine: victim.ID, Reason: "consolidate"})
	}
}

func slicesAny(as []Action, fn func(Action) bool) bool {
	for _, a := range as {
		if fn(a) {
			return true
		}
	}
	return false
}

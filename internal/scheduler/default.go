package scheduler

import (
	"math"
	"slices"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/manifest"
)

// Policy variants of Default.
const (
	// VariantDefault is the option policy (see options.go): every way to
	// serve a task (run now, wait for a machine, start, create) is scored
	// by cost + value of time x time to result, waits are bounded by the
	// time a new machine takes, and tasks only run on machines physically
	// large enough for them. "" selects it too.
	VariantDefault = "default"
	// VariantCostModel is the cost model before the option policy: the
	// same candidates scored relative to the cheapest and fastest of each
	// decision. Kept for comparison in the simulator.
	VariantCostModel = "costmodel"
	// VariantOption is the option policy of c5191aa: per-request greedy
	// placement, waits scored at their pessimistic ETA, no copy cost for
	// restoring a slot that is warm elsewhere. Kept for comparison.
	VariantOption = "option"
	// VariantBinpack is the policy before the cost model: warm cache
	// first, best-fit packing, sizes by cost per unit of queued demand and
	// consolidation by the present reservation. Kept for comparison in the
	// simulator.
	VariantBinpack = "binpack"
	// VariantClassic is the policy before scheduler tuning: rendezvous
	// placement, cheapest fitting size for the aggregate demand and no
	// consolidation. Kept for comparison in the simulator.
	VariantClassic = "classic"
)

// Config tunes the default policy.
type Config struct {
	// Variant selects the placement/sizing strategy: "default" (also ""),
	// "binpack" or "classic".
	Variant string
	// CostOptimization chooses the point on the cost/time curve: 1
	// minimises cost, 0 time to result; 0.5 trades one hour of waiting for
	// one machine hour of the cheapest size (default variant only).
	CostOptimization float64
	// QueueTimeout is how long a task may wait (0 = unbounded); the model
	// does not let tasks wait for capacity that frees later than that.
	QueueTimeout time.Duration
	// QueueConfidence: a request waits for capacity only when
	// P(finish_wait < finish_now) is at least this (0 = 0.8; default
	// variant only, see queueconf.go).
	QueueConfidence float64
	// Egress are transfer prices and bandwidths between stores and
	// providers (snapshot restores); unlisted pairs use defaults.
	Egress []Egress
	// DiskPricePerGBHour prices extra disk of a resize (0 = default).
	DiskPricePerGBHour float64
	// ConsolidateHold: no consolidation within this time after the pool
	// scaled an environment up (default 2m; default variant only).
	ConsolidateHold time.Duration
	// Overcommit factors: reservations may exceed physical capacity by this
	// factor. CPU overcommit only slows tasks down. Memory overcommit relies
	// on soft limits and swap; without swap it risks OOM kills.
	CPUOvercommit    float64
	MemoryOvercommit float64
	// New sessions are not placed on machines whose pressure exceeds these
	// limits (PSI some avg10, 0..1). 0 disables the check.
	MaxCPUPressure    float64
	MaxMemoryPressure float64
	// SoftLimit sets memory.high to Request.Memory * SoftLimit (0 = off).
	SoftLimit float64
	// Swap allows sessions to swap instead of being OOM killed.
	Swap bool
	// LimitJobs passes the reserved CPU as build parallelism hint.
	LimitJobs bool

	MinMachines  int           // warm pool per recently used environment
	WarmPool     []WarmWindow  // scheduled warm pool (e.g. working hours), in addition to MinMachines
	IdleStop     time.Duration // stop machines idle that long
	ExpiryMargin time.Duration // machines expiring sooner get no new sessions
	// DisabledProviders are providers with enabled = false: no machine of
	// them is created or started (running ones are destroyed when idle).
	DisabledProviders map[string]bool
	// MaxSpendPerHour is a circuit breaker on the hourly price of the
	// running and starting machines (0 = off): no create or start beyond
	// it, whatever the queue.
	MaxSpendPerHour float64

	// ConsolidateBelow drains the least loaded busy machine of an
	// environment when the reserved share of the environment's machines is
	// below this fraction and the remaining machines stay below
	// ConsolidateTarget after absorbing it (0 = off; default variant only).
	ConsolidateBelow float64
	// ConsolidateTarget is the maximum reserved share the remaining machines
	// may reach by consolidation (default 0.75).
	ConsolidateTarget float64
}

// Default is the built-in policy: warm cache first, best-fit packing with
// rendezvous hashing as tie breaker, overcommit-aware admission, scale out
// by queued demand with cost-aware sizing, consolidation, idle stop and a
// warm pool per recently used environment.
type Default struct {
	Cfg Config
}

func (p Default) cfg() Config {
	c := p.Cfg
	if c.CPUOvercommit <= 0 {
		c.CPUOvercommit = 1
	}
	if c.MemoryOvercommit <= 0 {
		c.MemoryOvercommit = 1
	}
	if c.ConsolidateTarget <= 0 {
		c.ConsolidateTarget = 0.75
	}
	if c.ConsolidateHold <= 0 {
		c.ConsolidateHold = 2 * time.Minute
	}
	return c
}

func (p Default) classic() bool { return p.Cfg.Variant == VariantClassic }

// model reports whether the cost model decides (default variant).
func (p Default) model() bool {
	return p.next() || p.Cfg.Variant == VariantCostModel
}

// next reports whether the option policy decides (see options.go).
func (p Default) next() bool { return p.global() || p.Cfg.Variant == VariantOption }

// global reports whether the default policy's batch decisions apply
// (see batch.go): queue-wide assignment, wait ETAs as distributions,
// the copy cost of restoring a slot warm elsewhere.
func (p Default) global() bool { return p.Cfg.Variant == VariantDefault || p.Cfg.Variant == "" }

// Admissible capacity of a machine under overcommit.
func (p Default) admissible(m Machine) Resources {
	c := p.cfg()
	return m.Capacity.Scale(c.CPUOvercommit, c.MemoryOvercommit)
}

func (p Default) sizeAdmissible(s Size) Resources {
	c := p.cfg()
	return s.Capacity.Scale(c.CPUOvercommit, c.MemoryOvercommit)
}

// Free returns the capacity left for new reservations.
func (p Default) Free(m Machine) Resources { return p.admissible(m).Sub(m.Reserved) }

func (p Default) accepting(m Machine, env string, now time.Time) bool {
	c := p.cfg()
	if m.State != Running || m.Draining || m.EnvHash != env {
		return false
	}
	if !m.ExpiresAt.IsZero() && m.ExpiresAt.Sub(now) <= c.ExpiryMargin {
		return false
	}
	if c.MaxCPUPressure > 0 && m.Pressure.CPU > c.MaxCPUPressure {
		return false
	}
	if c.MaxMemoryPressure > 0 && m.Pressure.Memory > c.MaxMemoryPressure {
		return false
	}
	return true
}

// Score is the rendezvous hash of a repository on a machine.
func Score(repo, machine string) string {
	return manifest.HashBytes([]byte(repo + "\x00" + machine))
}

// share is the larger of the CPU and memory fractions of r in capacity c
// (the dominant resource share).
func share(r, c Resources) float64 {
	s := 0.0
	if c.CPU > 0 {
		s = r.CPU / c.CPU
	}
	if c.Memory > 0 {
		s = math.Max(s, float64(r.Memory)/float64(c.Memory))
	}
	return s
}

// leftover is the normalised capacity a machine keeps after placing need:
// the smaller, the tighter the fit.
func leftover(free, need, capacity Resources) float64 {
	rest := free.Sub(need)
	l := 0.0
	if capacity.CPU > 0 {
		l += rest.CPU / capacity.CPU
	}
	if capacity.Memory > 0 {
		l += float64(rest.Memory) / float64(capacity.Memory)
	}
	return l
}

// rank orders candidate machines for t. Warm build cache for the
// repository comes first. The default variant then prefers machines with
// physical room (overcommit slows tasks down, so it is the last resort) and
// among those the tightest fit (best-fit bin packing keeps other machines
// empty so they can idle stop); rendezvous score breaks ties so builds of a repository keep
// landing on the same machine.
func (p Default) rank(ms []Machine, t Task) {
	fit := map[string]float64{}
	physical := map[string]bool{}
	if !p.classic() {
		for _, m := range ms {
			fit[m.ID] = leftover(p.Free(m), t.Request, p.admissible(m))
			physical[m.ID] = m.Capacity.Sub(m.Reserved).Fits(t.Request)
		}
	}
	sort.SliceStable(ms, func(i, j int) bool {
		wi, wj := ms[i].Warm(t.RepoID), ms[j].Warm(t.RepoID)
		if wi != wj {
			return wi
		}
		if pi, pj := physical[ms[i].ID], physical[ms[j].ID]; pi != pj {
			return pi // overcommit only when no machine has physical room
		}
		if fi, fj := fit[ms[i].ID], fit[ms[j].ID]; math.Abs(fi-fj) > 1e-9 {
			return fi < fj
		}
		return Score(t.RepoID, ms[i].ID) > Score(t.RepoID, ms[j].ID)
	})
}

// Grant computes the session limits for a task.
func (p Default) Grant(t Task) Grant {
	c := p.cfg()
	g := Grant{Reserve: t.Request, CPUWeight: t.Request.CPU, MemoryMax: t.Limit.Memory}
	if c.SoftLimit > 0 && t.Request.Memory > 0 {
		g.MemoryHigh = int64(float64(t.Request.Memory) * c.SoftLimit)
		if g.MemoryMax > 0 && g.MemoryHigh > g.MemoryMax {
			g.MemoryHigh = g.MemoryMax
		}
	}
	if c.Swap {
		g.Swap = -1
	}
	if c.LimitJobs && t.Request.CPU > 0 {
		g.Jobs = int(math.Ceil(t.Request.CPU))
	}
	return g
}

// Place implements Policy.
func (p Default) Place(f Pool, t Task) Placement {
	if p.model() {
		return p.placeModel(f, t)
	}
	if !p.fitsAnySize(f, t) {
		return Placement{Kind: Reject, Reason: "task larger than every machine size"}
	}
	var cands []Machine
	for _, m := range f.Machines {
		if p.accepting(m, t.EnvHash, f.Now) && p.Free(m).Fits(t.Request) {
			cands = append(cands, m)
		}
	}
	if len(cands) == 0 {
		return Placement{Kind: Wait, Reason: "no machine with free capacity"}
	}
	p.rank(cands, t)
	m := cands[0]
	reason := "rendezvous"
	switch {
	case m.Warm(t.RepoID):
		reason = "warm cache"
	case !p.classic() && len(cands) > 1:
		reason = "best fit"
	}
	return Placement{Kind: Assign, Machine: m.ID, Grant: p.Grant(t), Reason: reason}
}

func (p Default) fitsAnySize(f Pool, t Task) bool {
	for _, s := range f.ArchSizes(t.EnvHash) {
		if p.sizeAdmissible(s).Fits(t.Request) {
			return true
		}
	}
	for _, m := range f.Machines {
		if m.EnvHash == t.EnvHash && p.admissible(m).Fits(t.Request) {
			return true
		}
	}
	return false
}

// Disabled reports whether prov is disabled (enabled = false): no machine
// of it is created or started.
func (c Config) Disabled(prov string) bool { return c.DisabledProviders[prov] }

// smallestSize returns the cheapest size that fits need, or the largest,
// among the allowed sizes (see allowedSizes).
func (p Default) smallestSize(sizes []Size, need Resources, st *planState) (Size, bool) {
	ss := p.allowedSizes(sizes, st)
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].PricePerHour != ss[j].PricePerHour {
			return ss[i].PricePerHour < ss[j].PricePerHour
		}
		return ss[i].Capacity.Memory < ss[j].Capacity.Memory
	})
	for _, s := range ss {
		if p.sizeAdmissible(s).Fits(need) {
			return s, true
		}
	}
	if len(ss) == 0 {
		return Size{}, false
	}
	return ss[len(ss)-1], true
}

// pack places tasks first fit into a bin of free capacity and returns the
// packed tasks' total and the tasks that did not fit.
func pack(free Resources, tasks []Task) (packed Resources, rest []Task) {
	for _, t := range tasks {
		if free.Fits(t.Request) {
			free = free.Sub(t.Request)
			packed = packed.Add(t.Request)
		} else {
			rest = append(rest, t)
		}
	}
	return packed, rest
}

// bestSize chooses the size for the next machine for queued tasks (sorted
// largest first): the lowest price per fraction of the queued demand it
// absorbs, measured in the dominant resource. A single small task gets the
// cheapest size that fits; several queued tasks may justify a larger size
// when it is cheaper per unit. Unknown prices (0) prefer the size that
// absorbs the most demand, then the smaller one.
//
// slots is the number of machines that may still be created (runaway
// guard). Sizes that would need more machines than that to absorb the
// demand are only chosen when no size can: then the size absorbing the
// most wins.
func (p Default) bestSize(sizes []Size, tasks []Task, st *planState, slots int) (Size, []Task, bool) {
	total := Resources{}
	for _, t := range tasks {
		total = total.Add(t.Request)
	}
	type cand struct {
		s        Size
		served   float64
		cost     float64
		rest     []Task
		feasible bool
	}
	var cs []cand
	for _, s := range p.allowedSizes(sizes, st) {
		if s.Capacity.CPU <= 0 || s.Capacity.Memory <= 0 {
			continue
		}
		packed, rest := pack(p.sizeAdmissible(s), tasks)
		if len(rest) == len(tasks) {
			continue // fits none of the queued tasks
		}
		served := share(packed, total)
		if total.CPU <= 0 && total.Memory <= 0 {
			served = 1
		}
		needed := int(math.Ceil(1/served - 1e-9))
		cs = append(cs, cand{s: s, served: served, cost: s.PricePerHour / served, rest: rest, feasible: needed <= slots})
	}
	if len(cs) == 0 {
		return Size{}, nil, false
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.feasible != b.feasible {
			return a.feasible
		}
		if !a.feasible && math.Abs(a.served-b.served) > 1e-9 {
			return a.served > b.served
		}
		if math.Abs(a.cost-b.cost) > 1e-12 {
			return a.cost < b.cost
		}
		if math.Abs(a.served-b.served) > 1e-9 {
			return a.served > b.served
		}
		if a.s.PricePerHour != b.s.PricePerHour {
			return a.s.PricePerHour < b.s.PricePerHour
		}
		return a.s.Capacity.Memory < b.s.Capacity.Memory
	})
	return cs[0].s, cs[0].rest, true
}

// planState is the bookkeeping shared by the steps of Plan.
type planState struct {
	f           Pool
	acts        []Action
	active      int                  // running and starting machines (runaway guard)
	perProvider map[string]int       // machines per provider but gone (Limit.Machines)
	cpu         map[string]float64   // vCPUs of running and starting machines per provider (Limit.CPU)
	spend       float64              // hourly price of running and starting machines
	guard       int                  // runaway guard: active machines without a capacity exception
	exception   map[string]string    // environments whose requests wait too long: reason
	excess      map[string]int       // machines env may add beyond the guard: overdue requests without a machine on its way
	stopped     map[string][]Machine // usable stopped machines per environment
	queued      map[string][]Task    // queued tasks per environment
	envs        []string             // environments with queued tasks, in queue order
}

func (p Default) usable(m Machine, now time.Time) bool {
	return m.ExpiresAt.IsZero() || m.ExpiresAt.Sub(now) > p.cfg().ExpiryMargin
}

// Plan implements Policy.
func (p Default) Plan(f Pool) []Action {
	st := p.newPlanState(f)
	for _, env := range st.envs {
		switch {
		case p.classic():
			p.scaleOutClassic(st, env)
		case p.model():
			p.scaleOutModel(st, env)
		default:
			p.scaleOut(st, env)
		}
	}
	running := p.idleStop(st)
	switch {
	case p.model():
		p.consolidateModel(st, running)
	case !p.classic():
		p.consolidate(st, running)
	}
	p.warmPool(st, running)
	p.enforceLimits(st)
	p.capQueuedDemand(st)
	return st.acts
}

// scaleOutClassic covers the aggregate queued demand of env.
func (p Default) scaleOutClassic(st *planState, env string) {
	f := st.f
	demand, largest := Resources{}, Resources{}
	for _, t := range st.queued[env] {
		demand = demand.Add(t.Request)
		largest = Resources{CPU: math.Max(largest.CPU, t.Request.CPU), Memory: max(largest.Memory, t.Request.Memory)}
	}
	firstRepo := st.queued[env][0].RepoID
	incoming := Resources{}
	for _, m := range f.Machines {
		if m.EnvHash != env || !p.usable(m, f.Now) || m.Draining {
			continue
		}
		if m.State == Starting {
			incoming = incoming.Add(p.admissible(m))
		}
		if m.State == Running {
			if free := p.Free(m); free.CPU > 0 && free.Memory > 0 {
				incoming = incoming.Add(free)
			}
		}
	}
	need := demand.Sub(incoming)
	for need.Memory > 0 || need.CPU > 0 {
		if !p.room(st, env) {
			break
		}
		if s := st.stopped[env]; len(s) > 0 {
			sort.SliceStable(s, func(i, j int) bool { return Score(firstRepo, s[i].ID) > Score(firstRepo, s[j].ID) })
			st.acts = append(st.acts, p.note(st, env, Action{Kind: Start, Machine: s[0].ID, Reason: "queued demand"}))
			need = need.Sub(p.admissible(s[0]))
			st.grow(s[0].Provider, s[0].Capacity.CPU, priceOf(f, s[0]))
			st.stopped[env] = s[1:]
			continue
		}
		want := Resources{CPU: math.Max(need.CPU, largest.CPU), Memory: max(need.Memory, largest.Memory)}
		size, ok := p.smallestSize(f.SizesFor(env), want, st)
		if !ok || size.Capacity.CPU <= 0 || size.Capacity.Memory <= 0 {
			p.makeRoom(st, env)
			break
		}
		st.acts = append(st.acts, p.note(st, env, Action{Kind: Create, EnvHash: env, Size: size, Reason: "queued demand"}))
		need = need.Sub(p.sizeAdmissible(size))
		st.create(size)
	}
}

// makeRoom destroys a stopped machine of another environment when a
// provider is at its machine limit.
func (p Default) makeRoom(st *planState, env string) {
	envs := make([]string, 0, len(st.stopped))
	for e := range st.stopped {
		envs = append(envs, e)
	}
	sort.Strings(envs)
	for _, e := range envs {
		if s := st.stopped[e]; e != env && len(s) > 0 {
			st.acts = append(st.acts, Action{Kind: Destroy, Machine: s[0].ID, Reason: "make room"})
			st.stopped[e] = s[1:]
			return
		}
	}
}

// scaleOut packs the queued tasks of env (largest first) into capacity on
// its way, then into machines it undrains or starts, and creates machines
// for the rest, choosing sizes by cost per unit of absorbed demand.
func (p Default) scaleOut(st *planState, env string) {
	f := st.f
	tasks := append([]Task(nil), st.queued[env]...)
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].Request.Memory != tasks[j].Request.Memory {
			return tasks[i].Request.Memory > tasks[j].Request.Memory
		}
		return tasks[i].Request.CPU > tasks[j].Request.CPU
	})
	firstRepo := st.queued[env][0].RepoID

	// Capacity already there or on its way.
	var drained []Machine
	for _, m := range f.Machines {
		if m.EnvHash != env || !p.usable(m, f.Now) {
			continue
		}
		switch {
		case m.State == Running && m.Draining:
			drained = append(drained, m)
		case m.State == Running:
			_, tasks = pack(p.Free(m), tasks)
		case m.State == Starting && !m.Draining:
			_, tasks = pack(p.admissible(m).Sub(m.Reserved), tasks)
		}
	}
	// Undraining a consolidated machine is cheaper than starting one.
	sort.SliceStable(drained, func(i, j int) bool { return drained[i].ID < drained[j].ID })
	for _, m := range drained {
		if len(tasks) == 0 {
			return
		}
		var packed Resources
		if packed, tasks = pack(p.Free(m), tasks); packed != (Resources{}) {
			st.acts = append(st.acts, Action{Kind: Undrain, Machine: m.ID, Reason: "queued demand"})
		}
	}
	s := st.stopped[env]
	sort.SliceStable(s, func(i, j int) bool { return Score(firstRepo, s[i].ID) > Score(firstRepo, s[j].ID) })
	for len(tasks) > 0 && len(s) > 0 {
		if !p.room(st, env) && !p.freeSlot(st, env, tasks[0].Request) {
			break
		}
		m := s[0]
		s = s[1:]
		packed, rest := pack(p.admissible(m), tasks)
		if packed == (Resources{}) {
			continue
		}
		// With few machine slots left, a small stopped machine must not
		// take a slot a larger new machine needs.
		if slots := p.slots(st, env); p.machinesNeeded(f.SizesFor(env), rest, st) > slots-1 {
			continue
		}
		tasks = rest
		st.acts = append(st.acts, p.note(st, env, Action{Kind: Start, Machine: m.ID, Reason: "queued demand"}))
		st.grow(m.Provider, m.Capacity.CPU, priceOf(f, m))
	}
	st.stopped[env] = s
	for len(tasks) > 0 {
		if !p.room(st, env) && !p.freeSlot(st, env, tasks[0].Request) {
			return
		}
		size, rest, ok := p.bestSize(f.SizesFor(env), tasks, st, p.slots(st, env))
		if !ok {
			// Providers at their machine limit: stopped machines of other
			// environments hold their slots.
			p.makeRoom(st, env)
			return
		}
		st.acts = append(st.acts, p.note(st, env, Action{Kind: Create, EnvHash: env, Size: size, Reason: "queued demand"}))
		tasks = rest
		st.create(size)
	}
}

// machinesNeeded estimates how many machines of the largest allowed size
// tasks need.
func (p Default) machinesNeeded(sizes []Size, tasks []Task, st *planState) int {
	if len(tasks) == 0 {
		return 0
	}
	var largest Resources
	for _, s := range p.allowedSizes(sizes, st) {
		if a := p.sizeAdmissible(s); a.CPU*float64(a.Memory) > largest.CPU*float64(largest.Memory) {
			largest = a
		}
	}
	var total Resources
	for _, t := range tasks {
		total = total.Add(t.Request)
	}
	if largest.CPU <= 0 || largest.Memory <= 0 {
		return math.MaxInt32
	}
	return int(math.Ceil(share(total, largest) - 1e-9))
}

// freeSlot stops an empty running machine to make room for a new one when
// the pool is at its runaway guard: first one of an environment without queued
// work, then one of env that is too small for need. Machines kept by the
// warm pool are spared.
func (p Default) freeSlot(st *planState, env string, need Resources) bool {
	f := st.f
	running := map[string]int{}
	for _, m := range f.Machines {
		if m.State == Running || m.State == Starting {
			running[m.EnvHash]++
		}
	}
	for _, sameEnv := range []bool{false, true} {
		for _, m := range f.Machines {
			if m.State != Running || m.Sessions > 0 || m.Reserved != (Resources{}) || (m.EnvHash == env) != sameEnv {
				continue
			}
			if sameEnv && p.admissible(m).Fits(need) {
				continue
			}
			if !sameEnv && (len(st.queued[m.EnvHash]) > 0 || running[m.EnvHash] <= p.warmTarget(f, m.EnvHash)) {
				continue
			}
			if slices.ContainsFunc(st.acts, func(a Action) bool { return a.Machine == m.ID }) {
				continue
			}
			st.acts = append(st.acts, Action{Kind: Stop, Machine: m.ID, Reason: "make room"})
			st.active--
			return true
		}
	}
	return false
}

// warmTarget is the number of machines env keeps running.
func (p Default) warmTarget(f Pool, env string) int {
	c := p.cfg()
	used, ok := f.Recent[env]
	if !ok {
		return 0
	}
	n := 0
	if f.Now.Sub(used) < 24*time.Hour {
		n = c.MinMachines
	}
	return max(n, scheduledWarm(c.WarmPool, f.Now))
}

// idleStop stops idle machines beyond the warm pool and returns the
// running machines per environment afterwards.
func (p Default) idleStop(st *planState) map[string]int {
	c, f := p.cfg(), st.f
	stoppedNow := map[string]bool{}
	running := map[string]int{}
	for _, m := range f.Machines {
		if (m.State == Running || m.State == Starting) && p.usable(m, f.Now) {
			running[m.EnvHash]++
		}
	}
	for _, m := range f.Machines {
		// Queued tasks keep idle machines of their environment, unless the
		// cost model let them wait for other machines (then the idle one
		// was the worse choice for them and may stop).
		if m.State != Running || m.Sessions > 0 || m.Reserved.Memory > 0 || (len(st.queued[m.EnvHash]) > 0 && !p.model()) {
			continue
		}
		if slices.ContainsFunc(st.acts, func(a Action) bool { return a.Machine == m.ID }) {
			continue // already stopped or undrained by this plan
		}
		// A machine drained by consolidation stops as soon as it is empty.
		consolidated := !p.classic() && m.Draining && p.usable(m, f.Now)
		if !consolidated && (m.IdleSince.IsZero() || f.Now.Sub(m.IdleSince) < c.IdleStop) {
			continue
		}
		keep, why := running[m.EnvHash] <= p.warmTarget(f, m.EnvHash), "warm pool"
		if !keep && consolidated && p.model() && p.flapGuard() > 0 && !p.othersAbsorb(f, m) {
			// The demand of the window needs it: undrain instead of
			// stopping now and starting again in a minute.
			keep, why = true, "demand forecast"
		}
		if p.usable(m, f.Now) && keep {
			if consolidated {
				st.acts = append(st.acts, Action{Kind: Undrain, Machine: m.ID, Reason: why})
			}
			continue
		}
		// Experiment "decay": an environment's idle machines stop one at
		// a time, at most one a minute, so that after a burst the pool
		// shrinks gradually (cost +0.5 % in the simulator: off).
		if !consolidated && p.global() && exp("decay") && p.usable(m, f.Now) {
			last := f.Demand[m.EnvHash].LastStop
			if stoppedNow[m.EnvHash] || !last.IsZero() && f.Now.Sub(last) < StopDecay {
				continue
			}
			stoppedNow[m.EnvHash] = true
		}
		running[m.EnvHash]--
		reason := "idle"
		if consolidated {
			reason = "consolidated"
		}
		st.acts = append(st.acts, Action{Kind: Stop, Machine: m.ID, Reason: reason})
	}
	return running
}

// consolidate drains the least loaded busy machine of an environment whose
// machines are lightly reserved, when the others can take its share. New
// work then lands on the remaining machines and the drained one stops as
// soon as its sessions end instead of lingering at low utilisation.
func (p Default) consolidate(st *planState, running map[string]int) {
	c, f := p.cfg(), st.f
	if c.ConsolidateBelow <= 0 {
		return
	}
	byEnv := map[string][]Machine{}
	var envs []string
	draining := map[string]bool{}
	for _, m := range f.Machines {
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
		var reserved, capacity Resources
		for _, m := range ms {
			reserved = reserved.Add(m.Reserved)
			capacity = capacity.Add(p.admissible(m))
		}
		if share(reserved, capacity) >= c.ConsolidateBelow {
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
		rest := capacity.Sub(p.admissible(*victim))
		if share(reserved, rest) > c.ConsolidateTarget {
			continue
		}
		st.acts = append(st.acts, Action{Kind: Drain, Machine: victim.ID, Reason: "consolidate"})
	}
}

// warmPool starts or creates machines up to the warm target of every
// recently used environment.
func (p Default) warmPool(st *planState, running map[string]int) {
	f := st.f
	var recent []string
	for env := range f.Recent {
		recent = append(recent, env)
	}
	sort.Strings(recent)
	for _, env := range recent {
		for running[env] < p.warmTarget(f, env) {
			if s := st.stopped[env]; len(s) > 0 {
				st.acts = append(st.acts, Action{Kind: Start, Machine: s[0].ID, Reason: "warm pool"})
				st.stopped[env] = s[1:]
				st.grow(s[0].Provider, s[0].Capacity.CPU, priceOf(f, s[0]))
			} else {
				size, ok := p.smallestSize(f.SizesFor(env), Resources{}, st)
				if !ok {
					break
				}
				st.acts = append(st.acts, Action{Kind: Create, EnvHash: env, Size: size, Reason: "warm pool"})
				st.create(size)
			}
			running[env]++
		}
	}
}

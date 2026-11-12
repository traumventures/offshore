package scheduler

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// The pool has no configured machine count limit. Scale-out is bounded by
//
//   - the policy itself: the default policy only starts or creates a
//     machine for a request whose projected start is later than the
//     machine would be ready (see scaleOutModel);
//   - a runaway guard against bugs and feedback loops: at most
//     ⌈R + β√R⌉ running and starting machines plus one per active
//     environment and the warm pool, R being the offered load in machines
//     (guard); a request that waits longer than a create takes plus the
//     hedge margin lifts it for its environment (capacity exception);
//   - hard limits of the provider accounts (Pool.ProviderLimit: vCPU
//     quotas, observed limit errors), disabled providers and the optional
//     spend circuit breaker (Config.MaxSpendPerHour), which nothing lifts.

// newPlanState counts the pool for a plan (or for Place's hedge bound).
func (p Default) newPlanState(f Pool) *planState {
	st := &planState{f: f, perProvider: map[string]int{}, cpu: map[string]float64{}, exception: map[string]string{},
		stopped: map[string][]Machine{}, queued: map[string][]Task{}}
	base := p.newCounts(f)
	st.active, st.perProvider, st.cpu, st.spend = base.active, base.perProvider, base.cpu, base.spend
	for _, m := range f.Machines {
		if m.State == Stopped && p.usable(m, f.Now) && !f.Now.Before(m.StartBlocked) &&
			p.limitBlocks(st, m.Provider, m.Capacity.CPU, priceOf(f, m), false) == "" {
			st.stopped[m.EnvHash] = append(st.stopped[m.EnvHash], m)
		}
	}
	for _, t := range f.Queue {
		if _, ok := st.queued[t.EnvHash]; !ok {
			st.envs = append(st.envs, t.EnvHash)
		}
		st.queued[t.EnvHash] = append(st.queued[t.EnvHash], t)
	}
	st.guard = p.guard(f, st)
	st.excess = map[string]int{}
	for _, env := range st.envs {
		r, n := p.exceptionFor(f, st.queued[env])
		// One extra machine per overdue request, and none while machines
		// already on their way cover them: the exception fires once per
		// request, not on every plan tick while its machine boots.
		if n -= p.onWay(f, env, st.queued[env]); r != "" && n > 0 {
			st.exception[env], st.excess[env] = r, n
		}
	}
	return st
}

// guardBeta is the runaway guard's headroom factor: 2 standard deviations
// of Poisson arrivals when time counts, 0.5 when only money does.
func (p Default) guardBeta() float64 { return 2 - 1.5*p.costOptimization() }

// guard is the runaway guard: ⌈R + β√R⌉ plus one machine per environment
// with machines or queued requests plus the warm pool. R is the offered
// load in machines: per environment the larger of the time-averaged
// reservation over LoadWindow (Demand.Load: arrival rate × resource-
// seconds per session) and the present reservation plus queue, divided
// by the mean admissible capacity of its sizes, in the dominant resource.
func (p Default) guard(f Pool, st *planState) int {
	cur := map[string]Resources{}
	caps := map[string][]Resources{}
	for _, m := range f.Machines {
		if m.State == Running || m.State == Starting {
			cur[m.EnvHash] = cur[m.EnvHash].Add(m.Reserved)
			caps[m.EnvHash] = append(caps[m.EnvHash], p.admissible(m))
		}
	}
	for env, ts := range st.queued {
		for _, t := range ts {
			cur[env] = cur[env].Add(t.Request)
		}
	}
	r, n, warm := 0.0, 0, 0
	for env, now := range cur {
		n++
		load := f.Demand[env].Load
		load = Resources{CPU: math.Max(load.CPU, now.CPU), Memory: max(load.Memory, now.Memory)}
		var sum Resources
		k := 0
		for _, s := range f.ArchSizes(env) {
			sum, k = sum.Add(p.sizeAdmissible(s)), k+1
		}
		if k == 0 {
			for _, a := range caps[env] {
				sum, k = sum.Add(a), k+1
			}
		}
		if k == 0 || sum.CPU <= 0 || sum.Memory <= 0 {
			continue
		}
		mean := Resources{CPU: sum.CPU / float64(k), Memory: sum.Memory / int64(k)}
		r += share(load, mean)
	}
	for env := range f.Recent {
		warm += p.warmTarget(f, env)
	}
	return int(math.Ceil(r+p.guardBeta()*math.Sqrt(r)-1e-9)) + n + warm
}

// onWay counts the machines of env on their way (starting or being
// created) that fit one of the queued tasks ts: each serves at least one
// of them, so a queued request needs no further machine for it.
func (p Default) onWay(f Pool, env string, ts []Task) int {
	n := 0
	for _, m := range f.Machines {
		if m.EnvHash != env || m.State != Starting || m.Draining || !p.usable(m, f.Now) || p.hung(f, m) {
			continue
		}
		free := p.admissible(m).Sub(m.Reserved)
		for _, t := range ts {
			if free.Fits(t.Request) {
				n++
				break
			}
		}
	}
	return n
}

// capQueuedDemand drops the plan's scale-out for queued demand beyond one
// machine per queued request of an environment, counting the machines
// already on their way: whatever a policy step decided, a request never
// gets a second machine started or created for it.
func (p Default) capQueuedDemand(st *planState) {
	f := st.f
	envOf := map[string]string{}
	for _, m := range f.Machines {
		envOf[m.ID] = m.EnvHash
	}
	budget := map[string]int{}
	for _, env := range st.envs {
		budget[env] = len(st.queued[env]) - p.onWay(f, env, st.queued[env])
	}
	kept := st.acts[:0]
	for _, a := range st.acts {
		if (a.Kind == Create || a.Kind == Start) && strings.HasPrefix(a.Reason, "queued demand") {
			env := a.EnvHash
			if a.Kind == Start {
				env = envOf[a.Machine]
			}
			if budget[env] <= 0 {
				continue
			}
			budget[env]--
		}
		kept = append(kept, a)
	}
	st.acts = kept
}

// exceptionFor returns why queued requests of an environment may lift the
// runaway guard ("": they may not) and how many of them do: those that
// have waited longer than the fastest create plus the hedge margin (at
// cost_optimization 1: 80 % of the queue timeout, else 30 minutes).
func (p Default) exceptionFor(f Pool, ts []Task) (string, int) {
	if len(ts) == 0 {
		return "", 0
	}
	margin, ok := p.hedgeMargin()
	if !ok {
		margin = 30 * time.Minute
		if q := p.cfg().QueueTimeout; q > 0 {
			margin = q * 8 / 10
		}
	}
	boot := time.Duration(-1)
	for _, s := range f.SizesFor(ts[0].EnvHash) {
		if b := p.boot(f, s.Provider); boot < 0 || b < boot {
			boot = b
		}
	}
	if boot < 0 {
		return "", 0
	}
	reason, n := "", 0
	for _, t := range ts {
		if w := f.Now.Sub(t.Since); w > boot+margin {
			if n++; reason == "" {
				reason = fmt.Sprintf("%s waited %s > create %s + margin %s", t.ID, w.Round(time.Second), boot.Round(time.Second), margin.Round(time.Second))
			}
		}
	}
	return reason, n
}

// room reports whether the runaway guard lets env get one more machine.
func (p Default) room(st *planState, env string) bool {
	return st.active < st.guard || st.excess[env] > 0
}

// slots is how many machines env may still add under the runaway guard.
func (p Default) slots(st *planState, env string) int {
	return max(st.guard-st.active, 0) + st.excess[env]
}

// note marks a scale-out action beyond the runaway guard as a capacity
// exception (logged by the orchestrator) and uses up one overdue
// request's exception.
func (p Default) note(st *planState, env string, a Action) Action {
	if st.active >= st.guard && st.excess[env] > 0 {
		a.Reason += fmt.Sprintf("; capacity exception (guard %d): %s", st.guard, st.exception[env])
		st.excess[env]--
	}
	return a
}

// grow books a start or create.
func (st *planState) grow(prov string, cpu, price float64) {
	st.active++
	st.cpu[prov] += cpu
	st.spend += price
}

// create books a create of size s.
func (st *planState) create(s Size) {
	st.grow(s.Provider, s.Capacity.CPU, sizePrice(s))
	st.perProvider[s.Provider]++
}

// allowedSizes returns the sizes a create may use: provider enabled and
// within its hard limits, price within the spend circuit breaker.
func (p Default) allowedSizes(sizes []Size, st *planState) []Size {
	var ss []Size
	for _, s := range sizes {
		if p.limitBlocks(st, s.Provider, s.Capacity.CPU, sizePrice(s), true) == "" {
			ss = append(ss, s)
		}
	}
	return ss
}

// limitBlocks returns which hard limit forbids one more running machine
// of prov with cpu vCPUs and price ("": none). create: a new machine
// (counts against Limit.Machines), else a start.
func (p Default) limitBlocks(st *planState, prov string, cpu, price float64, create bool) string {
	c := p.cfg()
	if c.Disabled(prov) {
		return "provider disabled"
	}
	lim := st.f.ProviderLimit[prov]
	if create && lim.Machines > 0 && st.perProvider[prov] >= lim.Machines {
		return fmt.Sprintf("provider limit: %d machines (%s)", lim.Machines, lim.Source)
	}
	if lim.CPU > 0 && st.cpu[prov]+cpu > lim.CPU+1e-9 {
		return fmt.Sprintf("provider limit: %g vCPUs (%s)", lim.CPU, lim.Source)
	}
	if c.MaxSpendPerHour > 0 && st.spend+price > c.MaxSpendPerHour+1e-9 {
		return fmt.Sprintf("max_spend_per_hour %g", c.MaxSpendPerHour)
	}
	return ""
}

// enforceLimits drops the plan's creates, starts and undrains that a hard
// limit forbids (whichever step of the plan proposed them): disabled
// providers, provider limits, the spend circuit breaker.
func (p Default) enforceLimits(st *planState) {
	f := st.f
	base := p.newCounts(f)
	byID := map[string]Machine{}
	for _, m := range f.Machines {
		byID[m.ID] = m
	}
	kept := st.acts[:0]
	for _, a := range st.acts {
		switch a.Kind {
		case Create:
			if p.limitBlocks(base, a.Size.Provider, a.Size.Capacity.CPU, sizePrice(a.Size), true) != "" {
				continue
			}
			base.create(a.Size)
		case Start:
			m, ok := byID[a.Machine]
			if ok && p.limitBlocks(base, m.Provider, m.Capacity.CPU, priceOf(f, m), false) != "" {
				continue
			}
			if ok {
				base.grow(m.Provider, m.Capacity.CPU, priceOf(f, m))
			}
		case Undrain:
			if m, ok := byID[a.Machine]; ok && p.cfg().Disabled(m.Provider) {
				continue
			}
		case Stop, Destroy:
			if m, ok := byID[a.Machine]; ok && (m.State == Running || m.State == Starting) {
				base.active--
				base.cpu[m.Provider] -= m.Capacity.CPU
				base.spend -= priceOf(f, m)
			}
		}
		kept = append(kept, a)
	}
	st.acts = kept
}

// newCounts is the machine bookkeeping of f without queue or guard.
func (p Default) newCounts(f Pool) *planState {
	st := &planState{f: f, perProvider: map[string]int{}, cpu: map[string]float64{}}
	for _, m := range f.Machines {
		if m.State == Running || m.State == Starting {
			st.active++
			st.cpu[m.Provider] += m.Capacity.CPU
			st.spend += priceOf(f, m)
		}
		if m.State != Gone {
			st.perProvider[m.Provider]++
		}
	}
	return st
}

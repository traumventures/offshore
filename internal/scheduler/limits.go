package scheduler

import "fmt"

// Hard limits are the only bound on the pool besides the policy's own
// choices (there is no machine count and no runaway guard: the solve
// books every start and create, including the machines on their way):
// providers with enabled = false, the provider accounts' limits
// (Pool.ProviderLimit: vCPU quotas, observed limit errors) and the optional
// spend circuit breaker (Config.MaxSpendPerHour). Nothing lifts them.

// newPlanState counts the pool for a plan: running and starting machines
// per provider and the usable stopped machines per environment.
func (p Default) newPlanState(f Pool) *planState {
	st := p.newCounts(f)
	st.stopped = map[string][]Machine{}
	for _, m := range f.Machines {
		if m.State == Stopped && p.usable(m, f.Now) && !f.Now.Before(m.StartBlocked) &&
			p.limitBlocks(st, m.Provider, m.Capacity.CPU, priceOf(f, m), false) == "" {
			st.stopped[m.EnvHash] = append(st.stopped[m.EnvHash], m)
		}
	}
	return st
}

// grow books a start or create.
func (st *planState) grow(prov string, cpu, price float64) {
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
			st.cpu[m.Provider] += m.Capacity.CPU
			st.spend += priceOf(f, m)
		}
		if m.State != Gone {
			st.perProvider[m.Provider]++
		}
	}
	return st
}

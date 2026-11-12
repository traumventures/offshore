package scheduler

import (
	"math"
	"sort"
	"time"
)

// Lifecycle of idle and stopped machines (docs/design-scheduler-v2.md):
// a rent-or-buy (ski-rental) decision per machine. An idle running
// machine costs its price per hour; stopping it saves that and costs a
// start (latency, valued at time_value + price) when a request needs it
// again. A stopped machine costs its disk per hour; destroying it saves
// that and costs the difference between a create and a start when it is
// needed again. When it is needed comes from the learned arrivals of its
// environment (Pool.Arrivals): the k-th machine of an environment (busy
// machines first) is needed when a request arrives while k-1 others
// arrived within the concurrency window before it. The machine is kept
// exactly as long as the expected cost of keeping it, given the time
// since the last such arrival, is below the cost of letting it go now.

// MinIdle is the least time an idle machine is kept running.
const MinIdle = time.Minute

// ConcurrencyWindow is how far back arrivals count as concurrent when
// deciding whether a second, third ... machine is needed.
const ConcurrencyWindow = 5 * time.Minute

// RestorePenalty is the time the builds after a destroy lose because the
// recreated machine lost its warm slots: they restore lazily from the
// store and run cold until then (several builds, not one restore; the
// sim gate's prod traces put it near 10 minutes).
const RestorePenalty = 10 * time.Minute

// stopPenalty is the cost of having to start a stopped machine for a
// request: the start latency valued at time_value plus the price paid
// while it starts.
func (p Default) stopPenalty(f Pool, env, prov string, price float64) float64 {
	lam, _ := p.timeValue(f, env)
	if math.IsInf(lam, 1) {
		lam = 1e6
	}
	return (lam + price) * hours(p.startLatency(f, prov))
}

// breakEven is how long keeping an idle machine costs as much as starting
// it again (the deterministic ski-rental threshold without arrivals).
func (p Default) breakEven(f Pool, env, prov string, price float64) time.Duration {
	if price <= 0 {
		return 20 * time.Minute
	}
	d := time.Duration(p.stopPenalty(f, env, prov, price) / price * float64(time.Hour))
	return min(d, 2*time.Hour)
}

// demandEvents are the arrivals of env at which at least k requests
// arrived within ConcurrencyWindow (k = 1: every arrival).
func demandEvents(ts []time.Time, k int) []time.Time {
	if k <= 1 {
		return ts
	}
	var out []time.Time
	j := 0
	for i, t := range ts {
		for ts[j].Before(t.Add(-ConcurrencyWindow)) {
			j++
		}
		if i-j+1 >= k {
			out = append(out, t)
		}
	}
	return out
}

// todNear reports whether a and b are within two hours of each other in
// time of day, on the same kind of day (weekday or weekend).
func todNear(a, b time.Time) bool {
	we := func(t time.Time) bool { return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday }
	if we(a) != we(b) {
		return false
	}
	da := a.Sub(time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, a.Location()))
	db := b.Sub(time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, b.Location()))
	d := (da - db) % (24 * time.Hour)
	if d < 0 {
		d = -d
	}
	return min(d, 24*time.Hour-d) <= 2*time.Hour
}

// residualGaps returns the gaps between env's demand events of level k
// that are longer than the time since the last one (the possible
// remaining waits until the machine is needed, as gap lengths), those
// starting at a similar time of day when there are enough, and that
// elapsed time. ok is false without at least two events.
func residualGaps(f Pool, env string, k int) (gaps []time.Duration, elapsed time.Duration, ok bool) {
	ev := demandEvents(f.Arrivals[env], k)
	if len(ev) < 2 {
		return nil, 0, false
	}
	last := ev[len(ev)-1]
	elapsed = max(f.Now.Sub(last), 0)
	var near, all []time.Duration
	for i := 1; i < len(ev); i++ {
		g := ev[i].Sub(ev[i-1])
		if g <= elapsed {
			continue
		}
		all = append(all, g)
		if todNear(ev[i-1], last) {
			near = append(near, g)
		}
	}
	if len(near) >= 5 {
		return near, elapsed, true
	}
	return all, elapsed, true
}

// letGo decides the rent-or-buy question for a machine that costs rate
// per hour to keep and penalty when it is needed again after being let
// go: true when the expected cost of keeping it for any further time is
// at least penalty, given the remaining gaps (each equally likely) and
// the time already elapsed in the current gap.
func letGo(gaps []time.Duration, elapsed time.Duration, rate, penalty float64) bool {
	if len(gaps) == 0 {
		return true // longer than every gap seen
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	n := float64(len(gaps))
	best := penalty // let go now
	// Keep until the i-th gap ends: gaps up to it are served while kept.
	var kept float64 // sum of (g - elapsed) of the gaps served so far
	for i, g := range gaps {
		kept += hours(g - elapsed)
		served := float64(i + 1)
		cost := (rate*kept + rate*hours(g-elapsed)*(n-served) + penalty*(n-served)) / n
		if cost < best-1e-12 {
			return false
		}
	}
	return true
}

// stopIdle decides whether the idle running machine m, the k-th machine
// of its environment, stops now.
func (p Default) stopIdle(f Pool, m Machine, k int) bool {
	idle := f.Now.Sub(m.IdleSince)
	if m.IdleSince.IsZero() {
		idle = 0
	}
	if d := p.Cfg.IdleStop; d > 0 {
		return idle >= d
	}
	// Hysteresis: a machine idles at least MinIdle (a start bills a
	// minute anyway; requests often come in pairs).
	if idle < MinIdle {
		return false
	}
	price := priceOf(f, m)
	if price <= 0 {
		return idle >= 20*time.Minute
	}
	gaps, elapsed, ok := residualGaps(f, m.EnvHash, k)
	if !ok {
		return idle >= p.breakEven(f, m.EnvHash, m.Provider, price)
	}
	return letGo(gaps, elapsed, price, p.stopPenalty(f, m.EnvHash, m.Provider, price))
}

// destroyStopped decides whether the stopped machine m, the k-th machine
// of its environment, is destroyed now: its disk costs more until it is
// needed than a create instead of a start (and restoring its slots) then.
func (p Default) destroyStopped(f Pool, m Machine, k int) bool {
	disk := max(m.DiskTotal, m.Capacity.Disk)
	if s, ok := sizeOf(f, m); ok && disk == 0 {
		disk = s.Capacity.Disk
	}
	if disk <= 0 || m.Volume != "" {
		return false // no disk cost known (volumes have their own policy)
	}
	rate := float64(disk) / 1e9 * p.diskPriceOf(m)
	price := priceOf(f, m)
	lam, _ := p.timeValue(f, m.EnvHash)
	if math.IsInf(lam, 1) {
		return false
	}
	delay := max(p.boot(f, m.Provider)-p.startLatency(f, m.Provider), 0)
	penalty := (lam+price)*hours(delay) + lam*hours(RestorePenalty)
	gaps, elapsed, ok := residualGaps(f, m.EnvHash, k)
	if !ok {
		return false
	}
	return letGo(gaps, elapsed, rate, penalty)
}

// lifecycle stops idle machines and destroys stopped ones the rent-or-buy
// rule lets go, keeping the warm pool, and returns the running machines
// per environment afterwards. Machines booked by the plan stay.
func (p Default) lifecycle(st *planState, booked map[string]bool) map[string]int {
	f := st.f
	running := map[string]int{}
	busy := map[string]int{}
	var idle, stopped []Machine
	for _, m := range f.Machines {
		if (m.State == Running || m.State == Starting) && p.usable(m, f.Now) {
			running[m.EnvHash]++
		}
		switch {
		case m.State == Starting || m.State == Running && (m.Sessions > 0 || m.Reserved.Memory > 0 || m.Reserved.CPU > 0) || booked[m.ID]:
			if m.State != Stopped {
				busy[m.EnvHash]++
			}
		case m.State == Running:
			idle = append(idle, m)
		case m.State == Stopped:
			stopped = append(stopped, m)
		}
	}
	for _, a := range st.acts {
		if a.Kind == Start || a.Kind == Create {
			env := a.EnvHash
			for _, m := range f.Machines {
				if m.ID == a.Machine {
					env = m.EnvHash
				}
			}
			running[env]++
			busy[env]++
		}
	}
	// The machines most worth keeping first: cheapest, then most
	// recently busy.
	rank := func(ms []Machine, at func(Machine) time.Time) {
		sort.SliceStable(ms, func(i, j int) bool {
			pi, pj := priceOf(f, ms[i]), priceOf(f, ms[j])
			if pi != pj {
				return pi < pj
			}
			if ai, aj := at(ms[i]), at(ms[j]); !ai.Equal(aj) {
				return ai.After(aj)
			}
			return ms[i].ID < ms[j].ID
		})
	}
	rank(idle, func(m Machine) time.Time { return m.IdleSince })
	rank(stopped, func(m Machine) time.Time { return m.CreatedAt })
	k := map[string]int{}
	for env, n := range busy {
		k[env] = n
	}
	for _, m := range idle {
		if booked[m.ID] {
			continue
		}
		k[m.EnvHash]++
		consolidated := m.Draining && p.usable(m, f.Now)
		switch {
		case !p.usable(m, f.Now) || consolidated:
		case running[m.EnvHash] <= p.warmTarget(f, m.EnvHash):
			continue
		case !p.stopIdle(f, m, k[m.EnvHash]):
			continue
		}
		running[m.EnvHash]--
		reason := "idle"
		if consolidated {
			reason = "consolidated"
		}
		st.acts = append(st.acts, Action{Kind: Stop, Machine: m.ID, Reason: reason})
	}
	for _, m := range stopped {
		if booked[m.ID] {
			continue
		}
		k[m.EnvHash]++
		// The environment's first machine stays (until max_lifetime): a
		// create instead of its start is the next first session's wait.
		if k[m.EnvHash] >= 2 && p.Cfg.IdleStop == 0 && p.usable(m, f.Now) && p.destroyStopped(f, m, k[m.EnvHash]) {
			st.acts = append(st.acts, Action{Kind: Destroy, Machine: m.ID, Reason: "not needed soon enough to keep its disk"})
		}
	}
	p.renew(st, running)
	return running
}

// renew replaces the last machine of an environment that is about to
// expire (max_lifetime) with a new one that boots and then stops: the
// environment's next first request starts a stopped machine instead of
// waiting for a create. It is done when the time saved (create minus
// start latency, at time_value) is worth more than the boot, the minimum
// idle time and the disk until the next request is expected.
func (p Default) renew(st *planState, running map[string]int) {
	f := st.f
	lastSize := map[string]Machine{}
	has := map[string]bool{}
	for _, m := range f.Machines {
		if m.State == Gone {
			continue
		}
		if p.usable(m, f.Now) {
			has[m.EnvHash] = true
			continue
		}
		if o, ok := lastSize[m.EnvHash]; !ok || m.CreatedAt.After(o.CreatedAt) {
			lastSize[m.EnvHash] = m
		}
	}
	for _, a := range st.acts {
		if a.Kind == Create {
			has[a.EnvHash] = true
		}
	}
	for _, t := range f.Queue {
		has[t.EnvHash] = true
	}
	envs := make([]string, 0, len(lastSize))
	for env := range lastSize {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		ts := f.Arrivals[env]
		if has[env] || len(ts) == 0 || f.Now.Sub(ts[len(ts)-1]) > 24*time.Hour {
			continue
		}
		old := lastSize[env]
		s, ok := sizeOf(f, old)
		if !ok || !f.CanCreate(env, s.Provider) || p.limitBlocks(st, s.Provider, s.Capacity.CPU, sizePrice(s), true) != "" {
			continue
		}
		lam, _ := p.timeValue(f, env)
		if math.IsInf(lam, 1) {
			lam = 1e6
		}
		next := 24 * time.Hour
		if gaps, elapsed, ok := residualGaps(f, env, 1); ok && len(gaps) > 0 {
			sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
			next = gaps[len(gaps)/2] - elapsed
		}
		boot, start := p.boot(f, s.Provider), p.startLatency(f, s.Provider)
		disk := float64(s.Capacity.Disk) / 1e9 * p.diskPrice()
		cost := sizePrice(s)*hours(boot+p.minIdle()) + disk*hours(next)
		if lam*hours(boot-start) <= cost {
			continue
		}
		st.acts = append(st.acts, Action{Kind: Create, EnvHash: env, Size: s, Reason: "renew expiring machine " + old.ID})
		st.create(s)
		running[env]++
	}
}

// minIdle is the least time an idle machine runs: idle_stop when set.
func (p Default) minIdle() time.Duration {
	if d := p.Cfg.IdleStop; d > 0 {
		return d
	}
	return MinIdle
}

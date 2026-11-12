package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/manifest"
)

// Config tunes the default policy.
type Config struct {
	// CostOptimization chooses the point on the cost/time curve: 1
	// minimises cost, 0 time to result; 0.5 trades one hour of waiting for
	// one machine hour of the cheapest size.
	CostOptimization float64
	// QueueTimeout is how long a task may wait (0 = unbounded); the model
	// does not let tasks wait for capacity that frees later than that.
	QueueTimeout time.Duration
	// QueueConfidence is the quantile of the running sessions' remaining
	// times at which a wait for their capacity is scored: waiting wins only
	// when it finishes sooner with this probability (0 = 0.8).
	QueueConfidence float64
	// Egress are transfer prices and bandwidths between stores and
	// providers (snapshot restores); unlisted pairs use defaults.
	Egress []Egress
	// DiskPricePerGBHour prices extra disk of a resize (0 = default).
	DiskPricePerGBHour float64
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
	IdleStop     time.Duration // stop machines idle that long (0: derived by the lifecycle policy)
	ExpiryMargin time.Duration // machines expiring sooner get no new sessions
	// DisabledProviders are providers with enabled = false: no machine of
	// them is created or started (running ones are destroyed when idle).
	DisabledProviders map[string]bool
	// MaxSpendPerHour is a circuit breaker on the hourly price of the
	// running and starting machines (0 = off): no create or start beyond
	// it, whatever the queue.
	MaxSpendPerHour float64
}

// Default is the scheduler (docs/design-scheduler-v2.md): one option
// model for placement and scale-out (core.go) and a rent-or-buy lifecycle
// of idle and stopped machines (lifecycle.go).
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
	return c
}

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

// Place implements Policy: the solve of the queue with t (see core.go).
func (p Default) Place(f Pool, t Task) Placement { return p.placeCore(f, t) }

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

// planState is the bookkeeping of a plan: the actions so far and the
// running and starting machines they add up to (hard limits).
type planState struct {
	f           Pool
	acts        []Action
	perProvider map[string]int       // machines per provider but gone (Limit.Machines)
	cpu         map[string]float64   // vCPUs of running and starting machines per provider (Limit.CPU)
	spend       float64              // hourly price of running and starting machines
	stopped     map[string][]Machine // usable stopped machines per environment (warm pool)
}

func (p Default) usable(m Machine, now time.Time) bool {
	return m.ExpiresAt.IsZero() || m.ExpiresAt.Sub(now) > p.cfg().ExpiryMargin
}

// Plan implements Policy: the solve's starts and creates, then the
// lifecycle of idle and stopped machines and the warm pool.
func (p Default) Plan(f Pool) []Action { return p.planCore(f) }

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

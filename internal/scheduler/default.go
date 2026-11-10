package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/traumventures/cloudsourced/internal/manifest"
)

// Config tunes the default policy.
type Config struct {
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

	MinMachines  int            // warm pool per recently used environment
	MaxMachines  int            // fleet size limit across all providers
	ProviderMax  map[string]int // per-provider machine limit (0 / missing = no own limit)
	IdleStop     time.Duration  // stop machines idle that long
	ExpiryMargin time.Duration  // machines expiring sooner get no new sessions
}

// Default is the built-in policy: warm cache first, rendezvous hashing for
// affinity, overcommit-aware admission, scale out by queued demand, idle
// stop and a warm pool per recently used environment.
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

// rank orders machines: warm build cache for the repository first, then
// rendezvous score, so builds of a repository keep landing on one machine.
func rank(ms []Machine, repo string) {
	sort.SliceStable(ms, func(i, j int) bool {
		wi, wj := ms[i].Warm(repo), ms[j].Warm(repo)
		if wi != wj {
			return wi
		}
		return Score(repo, ms[i].ID) > Score(repo, ms[j].ID)
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
func (p Default) Place(f Fleet, t Task) Placement {
	if !p.fitsAnySize(f, t) {
		return Placement{Kind: Reject, Reason: "task larger than every machine size"}
	}
	var cands []Machine
	for _, m := range f.Machines {
		if p.accepting(m, t.EnvHash, f.Now) {
			cands = append(cands, m)
		}
	}
	rank(cands, t.RepoID)
	for _, m := range cands {
		if p.Free(m).Fits(t.Request) {
			reason := "rendezvous"
			if m.Warm(t.RepoID) {
				reason = "warm cache"
			}
			return Placement{Kind: Assign, Machine: m.ID, Grant: p.Grant(t), Reason: reason}
		}
	}
	return Placement{Kind: Wait, Reason: "no machine with free capacity"}
}

func (p Default) fitsAnySize(f Fleet, t Task) bool {
	c := p.cfg()
	for _, s := range f.Sizes {
		if s.Capacity.Scale(c.CPUOvercommit, c.MemoryOvercommit).Fits(t.Request) {
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

// smallestSize returns the cheapest size that fits need, or the largest,
// among sizes whose provider is below its machine limit.
func (p Default) smallestSize(sizes []Size, need Resources, perProvider map[string]int) (Size, bool) {
	c := p.cfg()
	var ss []Size
	for _, s := range sizes {
		if lim := c.ProviderMax[s.Provider]; lim > 0 && perProvider[s.Provider] >= lim {
			continue
		}
		ss = append(ss, s)
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].PricePerHour != ss[j].PricePerHour {
			return ss[i].PricePerHour < ss[j].PricePerHour
		}
		return ss[i].Capacity.Memory < ss[j].Capacity.Memory
	})
	for _, s := range ss {
		if s.Capacity.Scale(c.CPUOvercommit, c.MemoryOvercommit).Fits(need) {
			return s, true
		}
	}
	if len(ss) == 0 {
		return Size{}, false
	}
	return ss[len(ss)-1], true
}

// Plan implements Policy.
func (p Default) Plan(f Fleet) []Action {
	c := p.cfg()
	var acts []Action
	usable := func(m Machine) bool {
		return m.ExpiresAt.IsZero() || m.ExpiresAt.Sub(f.Now) > c.ExpiryMargin
	}
	active := 0                     // machines that count against MaxMachines
	perProvider := map[string]int{} // machines that count against ProviderMax
	for _, m := range f.Machines {
		if m.State == Running || m.State == Starting {
			active++
		}
		if m.State != Gone {
			perProvider[m.Provider]++ // stopped machines keep their slot
		}
	}
	stopped := map[string][]Machine{}
	for _, m := range f.Machines {
		if m.State == Stopped && usable(m) {
			stopped[m.EnvHash] = append(stopped[m.EnvHash], m)
		}
	}

	// Scale out: queued demand per environment beyond capacity on its way.
	demand := map[string]Resources{}
	largest := map[string]Resources{}
	firstRepo := map[string]string{}
	var envs []string
	for _, t := range f.Queue {
		if _, ok := demand[t.EnvHash]; !ok {
			envs = append(envs, t.EnvHash)
			firstRepo[t.EnvHash] = t.RepoID
		}
		demand[t.EnvHash] = demand[t.EnvHash].Add(t.Request)
		l := largest[t.EnvHash]
		largest[t.EnvHash] = Resources{CPU: math.Max(l.CPU, t.Request.CPU), Memory: max(l.Memory, t.Request.Memory)}
	}
	for _, env := range envs {
		incoming := Resources{}
		for _, m := range f.Machines {
			if m.EnvHash != env || !usable(m) || m.Draining {
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
		need := demand[env].Sub(incoming)
		for need.Memory > 0 || need.CPU > 0 {
			if s := stopped[env]; len(s) > 0 {
				sort.SliceStable(s, func(i, j int) bool {
					return Score(firstRepo[env], s[i].ID) > Score(firstRepo[env], s[j].ID)
				})
				acts = append(acts, Action{Kind: Start, Machine: s[0].ID, Reason: "queued demand"})
				need = need.Sub(p.admissible(s[0]))
				stopped[env] = s[1:]
				active++
				continue
			}
			if active >= c.MaxMachines {
				// Make room: destroy a stopped machine of another environment.
				for e, s := range stopped {
					if e != env && len(s) > 0 {
						acts = append(acts, Action{Kind: Destroy, Machine: s[0].ID, Reason: "make room"})
						stopped[e] = s[1:]
						break
					}
				}
				break
			}
			want := Resources{CPU: math.Max(need.CPU, largest[env].CPU), Memory: max(need.Memory, largest[env].Memory)}
			size, ok := p.smallestSize(f.Sizes, want, perProvider)
			if !ok || size.Capacity.CPU <= 0 || size.Capacity.Memory <= 0 {
				break
			}
			acts = append(acts, Action{Kind: Create, EnvHash: env, Size: size, Reason: "queued demand"})
			need = need.Sub(size.Capacity.Scale(c.CPUOvercommit, c.MemoryOvercommit))
			active++
			perProvider[size.Provider]++
		}
	}

	// Idle stop, keeping the warm pool.
	warmTarget := func(env string) int {
		if used, ok := f.Recent[env]; ok && f.Now.Sub(used) < 24*time.Hour {
			return c.MinMachines
		}
		return 0
	}
	running := map[string]int{}
	for _, m := range f.Machines {
		if (m.State == Running || m.State == Starting) && usable(m) {
			running[m.EnvHash]++
		}
	}
	queued := map[string]bool{}
	for _, t := range f.Queue {
		queued[t.EnvHash] = true
	}
	for _, m := range f.Machines {
		if m.State != Running || m.Sessions > 0 || m.Reserved.Memory > 0 || m.IdleSince.IsZero() || queued[m.EnvHash] {
			continue
		}
		if f.Now.Sub(m.IdleSince) < c.IdleStop {
			continue
		}
		if usable(m) && running[m.EnvHash] <= warmTarget(m.EnvHash) {
			continue
		}
		running[m.EnvHash]--
		acts = append(acts, Action{Kind: Stop, Machine: m.ID, Reason: "idle"})
	}

	// Warm pool.
	var recent []string
	for env := range f.Recent {
		recent = append(recent, env)
	}
	sort.Strings(recent)
	for _, env := range recent {
		for running[env] < warmTarget(env) {
			if s := stopped[env]; len(s) > 0 {
				acts = append(acts, Action{Kind: Start, Machine: s[0].ID, Reason: "warm pool"})
				stopped[env] = s[1:]
			} else if active < c.MaxMachines {
				size, ok := p.smallestSize(f.Sizes, Resources{}, perProvider)
				if !ok {
					break
				}
				acts = append(acts, Action{Kind: Create, EnvHash: env, Size: size, Reason: "warm pool"})
				active++
				perProvider[size.Provider]++
			} else {
				break
			}
			running[env]++
		}
	}
	return acts
}

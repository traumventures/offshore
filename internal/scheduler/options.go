package scheduler

import (
	"math"
	"time"
)

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

// choose returns the index of the best option (-1 for none). The option
// policy scores cost + timeValue x time in absolute terms; the cost model
// normalises both against the decision's cheapest and fastest option
// (pick).
func (p Default) choose(f Pool, env string, opts []option) int {
	if !p.next() {
		return p.pick(opts)
	}
	lam, ok := p.timeValue(f, env)
	if !ok {
		return p.pick(opts)
	}
	if len(opts) == 0 {
		return -1
	}
	score := absScore(lam)
	best := 0
	for i := 1; i < len(opts); i++ {
		a, b := opts[i], opts[best]
		sa, sb := score(a), score(b)
		switch {
		case math.Abs(sa-sb) > 1e-9*math.Max(1e-6, math.Abs(sb)):
			if sa < sb {
				best = i
			}
		case a.time != b.time:
			if a.time < b.time {
				best = i
			}
		case math.Abs(a.cost-b.cost) > 1e-12:
			if a.cost < b.cost {
				best = i
			}
		case math.Abs(a.fit-b.fit) > 1e-9:
			if a.fit < b.fit {
				best = i
			}
		case a.tie > b.tie:
			best = i
		}
	}
	return best
}

// physical reports whether t may run on a machine of capacity c: the
// option policy never places a task whose request exceeds a machine's
// physical cores or memory (overcommit is for the sum of reservations),
// unless nothing of its environment fits it physically.
func (p Default) physical(f Pool, c Resources, t Task) bool {
	if !p.next() || c.Fits(t.Request) {
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

// hedgeBound is the longest wait for capacity the option policy accepts
// for t: the fastest way to get a machine that fits it (start a stopped
// machine of its environment, create an allowed size) plus hedgeMargin.
// ok is false when no machine can be started or created (limits, provider
// outages) or the knob is 1: then waiting is only bounded by the queue
// timeout. Starting a machine does not commit the task to it: it takes
// whatever frees first.
func (p Default) hedgeBound(f Pool, t Task, active int, perProvider map[string]int, roomForMore bool) (time.Duration, bool) {
	margin, ok := p.hedgeMargin()
	if !p.next() || !ok {
		return 0, false
	}
	c := p.cfg()
	if active >= c.MaxMachines && c.MaxMachines > 0 && !roomForMore {
		return 0, false
	}
	best := time.Duration(-1)
	for _, m := range f.Machines {
		if m.EnvHash != t.EnvHash || m.State != Stopped || !p.usable(m, f.Now) || f.Now.Before(m.StartBlocked) ||
			!p.admissible(m).Fits(t.Request) || !p.physical(f, m.Capacity, t) {
			continue
		}
		if l := p.startLatency(f, m.Provider); best < 0 || l < best {
			best = l
		}
	}
	for _, s := range p.allowedSizes(f.SizesFor(t.EnvHash), perProvider) {
		if !p.sizeAdmissible(s).Fits(t.Request) || !p.physical(f, s.Capacity, t) {
			continue
		}
		if l := p.boot(f, s.Provider); best < 0 || l < best {
			best = l
		}
	}
	if best < 0 {
		return 0, false
	}
	return best + margin, true
}

// poolCounts returns the machines that count against MaxMachines
// (running, starting) and against each provider's limit (all but gone).
func poolCounts(f Pool) (int, map[string]int) {
	active, per := 0, map[string]int{}
	for _, m := range f.Machines {
		if m.State == Running || m.State == Starting {
			active++
		}
		if m.State != Gone {
			per[m.Provider]++
		}
	}
	return active, per
}

// idleMachine reports whether f has an empty running machine that could
// be stopped to make room (see freeSlot).
func idleMachine(f Pool) bool {
	for _, m := range f.Machines {
		if m.State == Running && m.Sessions == 0 && m.Reserved == (Resources{}) {
			return true
		}
	}
	return false
}

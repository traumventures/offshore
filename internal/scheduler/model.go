package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// The cost model scores every candidate of a decision (assign to a
// machine now, wait for a machine, start or create one, prune, resize or
// move a build slot) by its money cost and its time to result:
//
//	score = r * cost/min_cost + (1-r) * time/min_time     (r = cost_optimization)
//
// Both terms are normalised against the cheapest and the fastest candidate
// of the same decision, so r behaves the same for small and large
// decisions: r = 1 minimises cost (time breaks ties), r = 0 minimises time
// to result (cost breaks ties), r = 0.5 accepts a candidate that is 10%
// more expensive when it is more than 10% faster. Rendezvous hashing
// breaks the remaining ties.
//
// cost is the money the pool spends for the decision: the machine time a
// placement occupies (its share of the machine, the whole machine if it
// would otherwise idle stop), machines started or created (latency, runs,
// idle tail), the longer running of existing machines while work waits for
// them, egress of snapshot restores and extra disk. time is the time to
// result of the affected tasks: queue wait, start or boot latency, slot
// preparation (prune, resize, restore) and the run (warm, restored, cold,
// slowed down by CPU overcommit).

// Model defaults.
const (
	DefaultCostOptimization = 0.5
	DefaultTail             = 0.0 // idle tail charged to a new machine, as a fraction of idle_stop
	DefaultBoot             = 90 * time.Second
	DefaultStart            = 30 * time.Second
	DefaultPrune            = 20 * time.Second
	DefaultResize           = 60 * time.Second
	DefaultBandwidth        = 200 << 20 // bytes/s between a provider and its own store
	DefaultCrossBW          = 50 << 20  // bytes/s between a provider and another store
	DefaultCrossEgress      = 0.09      // price per GB leaving a store for another provider
	// DefaultDiskPrice is the price of one GB of machine disk per hour
	// (~0.08 per GB-month, EBS gp3).
	DefaultDiskPrice = 0.08 / 730
	// diskFull is the fraction of the disk the scheduler fills at most
	// when the worker does not report its budget.
	diskFull = 0.9
)

// Egress is the transfer price and bandwidth from an object store to the
// machines of a provider ([[scheduler.egress]]).
type Egress struct {
	From       string  // store name ("" = any)
	To         string  // provider name ("" = any)
	PricePerGB float64 // currency per GB
	Bandwidth  int64   // bytes per second (0 = default)
}

// option is one candidate of a decision.
type option struct {
	cost float64       // money
	time time.Duration // time to result
	tie  string        // rendezvous tie breaker (larger first)
	fit  float64       // leftover after a fit (smaller first)
	info *Option       // description for the decision log (nil: none)
}

func (p Default) costOptimization() float64 {
	return math.Min(math.Max(p.Cfg.CostOptimization, 0), 1)
}

// pick returns the index of the best option (-1 for none).
func (p Default) pick(opts []option) int {
	if len(opts) == 0 {
		return -1
	}
	r := p.costOptimization()
	score := p.pickScore(opts)
	best := 0
	for i := 1; i < len(opts); i++ {
		a, b := opts[i], opts[best]
		sa, sb := score(a), score(b)
		switch {
		case math.Abs(sa-sb) > 1e-9*math.Max(1, math.Abs(sb)):
			if sa < sb {
				best = i
			}
		case r >= 0.5 && math.Abs(a.cost-b.cost) > 1e-12:
			if a.cost < b.cost {
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

// pickScore is the cost model's score: cost and time normalised against
// the cheapest and fastest of opts, weighted by r = cost_optimization.
func (p Default) pickScore(opts []option) func(option) float64 {
	minC, minT := math.Inf(1), math.Inf(1)
	for _, o := range opts {
		if o.cost > 0 && o.cost < minC {
			minC = o.cost
		}
		if t := o.time.Seconds(); t > 0 && t < minT {
			minT = t
		}
	}
	r := p.costOptimization()
	return func(o option) float64 {
		s := 0.0
		if !math.IsInf(minC, 1) {
			s += r * o.cost / minC
		}
		if !math.IsInf(minT, 1) {
			s += (1 - r) * o.time.Seconds() / minT
		}
		return s
	}
}

// priceOf returns the hourly price of a machine: its size's, plus the
// storage of its cache volume.
func priceOf(f Pool, m Machine) float64 {
	price := 0.0
	if s, ok := sizeOf(f, m); ok {
		price = s.PricePerHour
	}
	if m.Medium == medium.Volume {
		price += storagePrice(m.Stack, m.DiskTotal)
	}
	return price
}

// sizeOf returns the size of a machine.
func sizeOf(f Pool, m Machine) (Size, bool) {
	for _, s := range f.Sizes {
		if s.Provider == m.Provider && s.Name == m.Size {
			return s, true
		}
	}
	return Size{}, false
}

// sizePrice is the hourly price of a new machine of s, including the
// storage of a fresh cache volume.
func sizePrice(s Size) float64 {
	if s.Medium != medium.Volume {
		return s.PricePerHour
	}
	return s.PricePerHour + storagePrice(s.Stack, max(s.Volume.Min, s.Capacity.Disk))
}

// storagePrice is the hourly price of bytes on the medium the stack's
// slots persist on.
func storagePrice(st medium.Stack, bytes int64) float64 {
	return float64(max(bytes, 0)) / 1e9 * SlotMedium(st).PricePerGBHour()
}

// Mobility: what it takes to use a warm build slot on another machine
// depends on where the slot lives (its medium's reach):
//
//   - on a machine's local disk (reach machine): a snapshot to the store
//     (the dirty part) and a lazy restore of the working set, bounded by
//     the store and the stack's fault time (SlotRestore, SlotMove);
//   - on a detached volume (reach zone): a new machine of a volume size in
//     the volume's zone attaches it, the profile's attach time
//     (Action.Volume, see scaleOutModel);
//   - on a volume in another zone or provider: like local, via the store;
//   - in the store (snapshots, global caches): restore from anywhere.

// restoreTime is the time until t's build can proceed on a slot of bytes
// restored lazily from a store with bandwidth bw into m's stack: its
// working set faulted in (see LazyRestore).
func restoreTime(m Machine, t Task, bytes, bw int64) time.Duration {
	return LazyRestore(m.Stack, bytes, t.Profile.WorkingSet, bw)
}

func hours(d time.Duration) float64 { return d.Hours() }

// latencies of a provider.
func (p Default) boot(f Pool, prov string) time.Duration {
	if ps, ok := f.Providers[prov]; ok && ps.Boot > 0 {
		return ps.Boot
	}
	return DefaultBoot
}

func (p Default) startLatency(f Pool, prov string) time.Duration {
	ps := f.Providers[prov]
	switch {
	case ps.Start > 0:
		return ps.Start
	case ps.Boot > 0:
		return ps.Boot / 2 // a start keeps the disk and image: faster than a create
	}
	return DefaultStart
}

// providerDown reports whether prov is in a capacity cooldown.
func providerDown(f Pool, prov string) bool {
	ps, ok := f.Providers[prov]
	return ok && f.Now.Before(ps.Down)
}

func storeOf(f Pool, prov string) string {
	if ps, ok := f.Providers[prov]; ok && ps.Store != "" {
		return ps.Store
	}
	return prov
}

// egress returns price per GB and bandwidth from store to provider prov.
func (p Default) egress(f Pool, store, prov string) (float64, int64) {
	best, bestScore := Egress{}, -1
	for _, e := range p.Cfg.Egress {
		if (e.From != "" && e.From != store) || (e.To != "" && e.To != prov) {
			continue
		}
		sc := 0
		if e.From != "" {
			sc += 2
		}
		if e.To != "" {
			sc++
		}
		if sc > bestScore {
			best, bestScore = e, sc
		}
	}
	same := store == storeOf(f, prov)
	price, bw := best.PricePerGB, best.Bandwidth
	if bestScore < 0 && !same {
		price = DefaultCrossEgress
	}
	if bw <= 0 {
		bw = DefaultBandwidth
		if !same {
			bw = DefaultCrossBW
		}
	}
	return price, bw
}

// slowdown is the expected run time factor of t on m from CPU overcommit.
func slowdown(m Machine, t Task) float64 {
	if m.Capacity.CPU <= 0 {
		return 1
	}
	return math.Max(1, (m.Reserved.CPU+t.Request.CPU)/m.Capacity.CPU)
}

// runChoice is how a task would run on a machine: with its warm slot,
// from a restored snapshot or cold, after optional slot preparation.
type runChoice struct {
	run    time.Duration // preparation + run
	thrash time.Duration // predicted page-in (refault) time of a working set beyond the local tier
	prep   time.Duration // the preparation part of run (restore, move, prune, resize, thrash)
	cost   float64       // egress, extra disk
	slot   SlotAction
	reason string
}

// slotNeed is the disk a task needs on m: the growth of an existing slot,
// else the size of a new one.
func slotNeed(m Machine, t Task, restoreBytes int64) int64 {
	if b, ok := m.SlotBytes[t.RepoID]; ok && m.Warm(t.RepoID) && b > 0 {
		return t.Profile.SlotGrowth
	}
	return max(t.Profile.SlotBytes, restoreBytes)
}

// prunable estimates what pruning frees on m for t: other repositories'
// slots (least recently used first) and a third of t's own slot.
func prunable(m Machine, t Task) int64 {
	var n int64
	for repo, b := range m.SlotBytes {
		if repo == t.RepoID {
			n += b / 3
		} else {
			n += b
		}
	}
	return n
}

// slotsToPrune is how many other repositories' slots a prune of missing
// bytes removes (largest first).
func slotsToPrune(m Machine, t Task, missing int64) int {
	var bs []int64
	for repo, b := range m.SlotBytes {
		if repo != t.RepoID {
			bs = append(bs, b)
		}
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i] > bs[j] })
	n := 0
	for _, b := range bs {
		if missing <= 0 {
			break
		}
		missing -= b
		n++
	}
	return n
}

// prepare returns the ways to run rc on m with the disk handling it needs:
// as is (when the slot fits), after pruning other slots, or after growing
// the disk to one of several targets (room for the slot, the working set
// in the local tier, twice that). Each way carries the thrash penalty of
// the local capacity it leaves, so the caller scores slot handling,
// restore and thrashing jointly. A restore or move keeps its kind and
// grows the disk in the same step (SlotAction.Grow); it never prunes:
// evicting another warm slot for a restored one ping-pongs between
// repositories (sim: 3x the thrashing, 5x the restores on team-week).
// The result is empty when nothing makes room.
func (p Default) prepare(f Pool, m Machine, t Task, rc runChoice, need int64) []runChoice {
	_, ws, capacity := thrashOf(f, m, t)
	refetch := refetchOf(f, m.Provider, t)
	missing := int64(0)
	if m.DiskTotal > 0 && need > 0 {
		limit := m.DiskBudget
		if limit <= 0 {
			limit = int64(diskFull * float64(m.DiskTotal))
		}
		missing = need - (limit - m.DiskUsed)
	}
	var out []runChoice
	// finish adds the thrash penalty of capacity cap.
	finish := func(c runChoice, cap int64) {
		if pen := ThrashTime(m.Stack, cap, ws, refetch); pen > 0 {
			c.run += pen
			c.thrash = pen
			c.reason += ", thrashing"
		}
		out = append(out, c)
	}
	joint := rc.slot.Kind == SlotRestore || rc.slot.Kind == SlotMove
	if missing <= 0 {
		finish(rc, capacity)
	} else if !joint && prunable(m, t) >= missing {
		c := rc
		// Pruning other repositories' slots makes their next builds cold.
		pr := DefaultPrune + time.Duration(slotsToPrune(m, t, missing))*time.Duration(float64(DefaultRun)*DefaultColdPenalty)
		c.run += pr
		c.prep += pr
		c.slot = SlotAction{Kind: SlotPrune, Repo: t.RepoID, Bytes: missing}
		c.reason += ", prune slots"
		finish(c, capacity)
	}
	// A disk resized recently cannot grow again (EBS: once per 6 h).
	if !m.CanResize || f.Now.Before(m.ResizableAt) {
		if c, ok := diskTight(out, rc, missing, m.Warm(t.RepoID)); ok {
			finish(c, capacity)
		}
		return out
	}
	s, hasSize := sizeOf(f, m)
	var grows []int64
	if missing > 0 {
		grows = append(grows, max(missing*2, 10<<30)) // grow generously: resizes are slow
	}
	if ws > capacity && capacity > 0 && growsCapacity(m) {
		g := max(ws+ws/4-capacity, 10<<30)
		grows = append(grows, g, 2*g)
	}
	left := m.ExpiresAt.Sub(f.Now)
	if m.ExpiresAt.IsZero() || left <= 0 {
		left = 24 * time.Hour
	}
	seen := map[int64]bool{}
	for _, g := range grows {
		if hasSize && s.Volume.Step > g {
			g = s.Volume.Step
		}
		target := m.DiskTotal + g
		if hasSize && s.Volume.Max > 0 && target > s.Volume.Max {
			target = s.Volume.Max
		}
		g = target - m.DiskTotal
		if g <= 0 || g < missing || seen[target] {
			continue
		}
		seen[target] = true
		c := rc
		c.run += DefaultResize
		c.prep += DefaultResize
		c.cost += float64(g) / 1e9 * p.diskPriceOf(m) * hours(left)
		if joint {
			c.slot.Grow = target
		} else {
			c.slot = SlotAction{Kind: SlotResize, Repo: t.RepoID, Bytes: target}
		}
		c.reason += ", resize disk"
		cap := capacity
		if growsCapacity(m) && cap > 0 {
			cap += g
		}
		if ws > capacity && ws <= cap {
			c.reason += " for working set"
		}
		finish(c, cap)
	}
	if c, ok := diskTight(out, rc, missing, m.Warm(t.RepoID)); ok {
		finish(c, capacity)
	}
	return out
}

// diskTightPenalty is the time charged to a run on a machine whose disk
// the model sees short by more than pruning or a resize can free.
const diskTightPenalty = 5 * time.Minute

// diskTight keeps the run on a warm slot possible when its growth does
// not fit the disk and neither a prune nor a resize covers it (a cold run
// or restore needs the whole slot and goes elsewhere or waits): the disk usage counts
// data outside the slots (images, unaccounted files) that no prune
// frees, and without any option the task waited for capacity until its
// queue timeout while machines were started and idle-stopped around it
// (production: every task of a repository timed out for an hour). The
// worker protects the disk itself (budget prune, no admission at 97 %);
// the penalty lets a machine with room win when there is one.
func diskTight(out []runChoice, rc runChoice, missing int64, warm bool) (runChoice, bool) {
	if len(out) > 0 || missing <= 0 || !warm || rc.slot.Kind != SlotNone {
		return rc, false
	}
	rc.run += diskTightPenalty
	rc.prep += diskTightPenalty
	rc.reason += ", disk tight"
	return rc, true
}

// diskPriceOf is the price per GB-hour of m's hot medium (the configured
// disk price when its profile has none).
func (p Default) diskPriceOf(m Machine) float64 {
	if pr := SlotMedium(m.Stack).PricePerGBHour(); pr > 0 {
		return pr
	}
	return p.diskPrice()
}

func (p Default) diskPrice() float64 {
	if p.Cfg.DiskPricePerGBHour > 0 {
		return p.Cfg.DiskPricePerGBHour
	}
	return DefaultDiskPrice
}

// runChoices returns the ways t can run on m (warm, restore, move, cold),
// each with the disk handling it needs (see prepare). Machines without
// room are skipped.
func (p Default) runChoices(f Pool, m Machine, t Task) []runChoice {
	slow := slowdown(m, t)
	scale := func(d time.Duration) time.Duration { return time.Duration(float64(d) * slow) }
	var out []runChoice
	add := func(rc runChoice, need int64) { out = append(out, p.prepare(f, m, t, rc, need)...) }
	if m.Warm(t.RepoID) {
		add(runChoice{run: scale(t.Profile.WarmRun()), reason: "warm slot"}, slotNeed(m, t, 0))
		return out
	}
	if s, ok := p.bestSnapshot(f, m.Provider, t); ok {
		price, bw := p.egress(f, s.Store, m.Provider)
		restore := t.Profile.Restore
		if restore <= 0 || s.Store != storeOf(f, m.Provider) {
			restore = restoreTime(m, t, s.Bytes, bw)
		}
		rc := runChoice{run: restore + scale(t.Profile.WarmRun()), prep: restore, cost: float64(s.Bytes) / 1e9 * price,
			slot: SlotAction{Kind: SlotRestore, Repo: t.RepoID, Snapshot: s}, reason: "restore snapshot from " + s.Store}
		rc.cost += p.copyCost(f, m, t, s.Bytes) // a second warm copy while the warm machine keeps its own
		add(rc, slotNeed(m, t, s.Bytes))
	}
	if src, ok := p.moveSource(f, m, t); ok {
		b := src.SlotBytes[t.RepoID]
		st := storeOf(f, src.Provider)
		_, upBW := p.egress(f, st, src.Provider)
		price, downBW := p.egress(f, st, m.Provider)
		move := saveTime(src, b, upBW) + restoreTime(m, t, b, downBW)
		rc := runChoice{run: move + scale(t.Profile.WarmRun()), prep: move, cost: float64(b) / 1e9 * price,
			slot: SlotAction{Kind: SlotMove, Repo: t.RepoID, Bytes: b, From: src.ID,
				Snapshot: Snapshot{Repo: t.RepoID, Provider: src.Provider, Store: st, Bytes: b, At: f.Now}},
			reason: "move warm slot from " + src.ID}
		add(rc, slotNeed(m, t, b))
	}
	add(runChoice{run: scale(t.Profile.ColdRun(m.Provider)), reason: "cold"}, slotNeed(m, t, 0))
	return out
}

// saveTime is the time to save a slot of bytes on src to its store with
// bandwidth bw: with a cache file system only its dirty pages are not in
// the store yet (writeback), else the whole slot.
func saveTime(src Machine, bytes, bw int64) time.Duration {
	up := bytes
	if src.VM.Capacity > 0 {
		up = min(src.VM.Dirty, bytes)
	}
	return max(time.Duration(float64(up)/float64(bw)*float64(time.Second)), src.Stack.Top().TransferTime(up))
}

// moveSource returns an idle running machine other than m with a warm slot
// of t's repository that is newer than every known snapshot of it (saved
// and restored on m by a SlotMove). Busy machines are skipped: their slot
// may be in use.
func (p Default) moveSource(f Pool, m Machine, t Task) (Machine, bool) {
	var newest time.Time
	for _, s := range f.Snapshots[t.RepoID] {
		if s.Bytes > 0 && s.At.After(newest) {
			newest = s.At
		}
	}
	for _, src := range f.Machines {
		if src.ID == m.ID || src.State != Running || src.Sessions > 0 || !src.Warm(t.RepoID) || src.SlotBytes[t.RepoID] <= 0 {
			continue
		}
		if !newest.IsZero() && !newest.Before(src.IdleSince) {
			continue // the latest snapshot already holds this slot
		}
		return src, true
	}
	return Machine{}, false
}

// bestSnapshot returns the newest snapshot of t's repository, preferring
// one in the store of prov.
func (p Default) bestSnapshot(f Pool, prov string, t Task) (Snapshot, bool) {
	var best Snapshot
	found := false
	local := storeOf(f, prov)
	for _, s := range f.Snapshots[t.RepoID] {
		if s.Bytes <= 0 {
			continue
		}
		sl, bl := s.Store == local, best.Store == local
		if !found || sl && !bl || sl == bl && s.At.After(best.At) {
			best, found = s, true
		}
	}
	return best, found
}

// machineCost is the marginal machine cost of running t on m from start
// for run: the machine is paid until its sessions end anyway; a run beyond
// that extends its uptime at the full price (an idle machine from now: the
// time it already idled is sunk), and the part of the run within the paid
// time costs the share of the machine it takes from others.
func (p Default) machineCost(f Pool, m Machine, t Task, start time.Time, run time.Duration) float64 {
	price := priceOf(f, m)
	if price == 0 {
		return 0
	}
	busyEnd := f.Now
	for _, r := range m.Releases {
		if e := r.Expected(f.Now); e.After(busyEnd) {
			busyEnd = e
		}
	}
	end := start.Add(run)
	ext := max(end.Sub(busyEnd), 0)
	overlap := max(run-ext, 0)
	sh := math.Min(share(t.Request, m.Capacity), 1)
	return price * (hours(ext) + sh*hours(overlap))
}

// freeAt returns when need fits on m after the expected releases of its
// sessions (now when it fits already; false if never). Sessions running
// longer than expected are assumed to need as long again.
func (p Default) freeAt(f Pool, m Machine, need Resources) (time.Time, bool) {
	adm := p.admissible(m)
	if !adm.Fits(need) {
		return time.Time{}, false
	}
	start := f.Now
	if m.State == Starting {
		start = m.ReadyAt
		if start.IsZero() || start.Before(f.Now) {
			start = f.Now.Add(p.startLatency(f, m.Provider) / 2)
		}
	}
	free := adm.Sub(m.Reserved)
	if free.Fits(need) {
		return start, true
	}
	rs := append([]Release(nil), m.Releases...)
	for i := range rs {
		rs[i].At = rs[i].Expected(f.Now)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].At.Before(rs[j].At) })
	for _, r := range rs {
		free = free.Add(r.Res)
		if free.Fits(need) {
			if r.At.Before(start) {
				return start, true
			}
			return r.At, true
		}
	}
	// Sessions without an expected end free their share eventually.
	last := f.Now.Add(DefaultRun)
	if len(rs) > 0 && rs[len(rs)-1].At.After(last) {
		last = rs[len(rs)-1].At
	}
	if last.Before(start) {
		last = start
	}
	return last, true
}

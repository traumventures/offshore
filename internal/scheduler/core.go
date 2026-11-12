package scheduler

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// The v2 core (docs/design-scheduler-v2.md): every event solves, for the
// pending requests in priority order, "choose the option with the least
// cost + time_value x time to result" over an explicit option set:
//
//   - run on a machine with room now (warm slot, seed, restore, move or
//     cold, with the prune or resize it needs: runChoices),
//   - wait for a busy or starting machine (or one this solve starts or
//     creates for an earlier request), its sessions ending at the
//     queue_confidence quantile of their remaining time,
//   - start a stopped machine,
//   - create a size (or attach a warm detached volume).
//
// Each chosen option books the request on its machine's timeline, so a
// later request sees the capacity taken, including machines on their
// way: there is no separate guard against starting too many machines.
// Only hard limits (disabled providers, account limits, the spend circuit
// breaker) are enforced on top.

// cmach is a machine of the solve: existing (running or starting) or
// planned by it (start, create).
type cmach struct {
	tl  *tlMachine
	act *Action // the start or create that brings it (nil: existing)
}

// solveState is the state of one solve.
type solveState struct {
	f       Pool
	st      *planState
	ms      map[string][]*cmach // per environment
	stopped map[string][]Machine
	acts    []Action
	booked  map[string]bool // machines some request was booked on
	q       float64
	// onNew are the requests booked on each create of the solve with
	// their chosen options (packCreates may merge the creates).
	onNew    map[*cmach][]coreChoice
	newOrder []*cmach       // creates (without volume) in order
	actIdx   map[*cmach]int // index of each create in acts
}

// coreChoice is a request's best option.
type coreChoice struct {
	t        Task
	ok       bool
	o        option
	rc       runChoice
	cm       *cmach
	at       time.Time
	assign   bool // runs now on a running machine
	decision *Decision
}

// queueQ is the quantile of the remaining run times at which a wait for a
// busy machine is scored: waiting is chosen only when it finishes sooner
// with probability queue_confidence.
func (p Default) queueQ() float64 {
	c := p.Cfg.QueueConfidence
	if c <= 0 || c > 1 {
		c = DefaultQueueConfidence
	}
	return c
}

// coreTimeline is the timeline of an existing machine: free capacity now
// (or when it is ready) and its sessions' releases at quantile q;
// reservations without a known end are not counted on to end.
func (p Default) coreTimeline(f Pool, m Machine, q float64) *tlMachine {
	tm := &tlMachine{m: m, ready: f.Now, price: priceOf(f, m)}
	if m.State == Starting {
		tm.ready = m.ReadyAt
		if tm.ready.IsZero() || tm.ready.Before(f.Now) {
			tm.ready = f.Now.Add(p.startLatency(f, m.Provider) / 2)
		}
	}
	tm.free = p.admissible(m).Sub(m.Reserved)
	never := f.Now.Add(24 * time.Hour)
	// Sessions sharing overcommitted cores run slower than when their
	// ends were predicted: stretch the remaining times by the contention.
	stretch := 1.0
	if m.Capacity.CPU > 0 {
		stretch = math.Max(1, m.Reserved.CPU/m.Capacity.CPU)
	}
	covered := Resources{}
	for _, r := range m.Releases {
		at, ok := releaseQ(r, q, f.Now)
		switch {
		case !ok:
			at = never
		case !at.After(f.Now.Add(time.Second)):
			// Overrunning its distribution: as long again as it ran.
			at = r.Expected(f.Now)
		default:
			at = f.Now.Add(time.Duration(float64(at.Sub(f.Now)) * stretch))
		}
		tm.deltas = append(tm.deltas, Release{At: at, Res: r.Res})
		covered = covered.Add(r.Res)
	}
	if rest := m.Reserved.Sub(covered); rest.CPU > 1e-9 || rest.Memory > 0 {
		tm.deltas = append(tm.deltas, Release{At: never, Res: Resources{CPU: math.Max(rest.CPU, 0), Memory: max(rest.Memory, 0)}})
	}
	return tm
}

func (p Default) newSolve(f Pool) *solveState {
	ss := &solveState{f: f, st: p.newCounts(f), ms: map[string][]*cmach{}, stopped: map[string][]Machine{}, booked: map[string]bool{}, q: p.queueQ(), onNew: map[*cmach][]coreChoice{}, actIdx: map[*cmach]int{}}
	for _, m := range f.Machines {
		switch {
		case m.State == Running && p.usable(m, f.Now) && !m.Draining,
			m.State == Starting && p.usable(m, f.Now) && !m.Draining && !p.hung(f, m):
			ss.ms[m.EnvHash] = append(ss.ms[m.EnvHash], &cmach{tl: p.coreTimeline(f, m, ss.q)})
		case m.State == Stopped && p.usable(m, f.Now) && !f.Now.Before(m.StartBlocked) && !p.cfg().Disabled(m.Provider):
			ss.stopped[m.EnvHash] = append(ss.stopped[m.EnvHash], m)
		}
	}
	return ss
}

// idleTail is the idle time charged to a started or created machine
// after its run: the lifecycle policy stops an idle machine after about
// the break-even time of a later start (or idle_stop when configured).
func (p Default) idleTail(f Pool, env string, prov string, price float64) time.Duration {
	if d := p.Cfg.IdleStop; d > 0 {
		return d
	}
	return p.breakEven(f, env, prov, price)
}

// score is the option's cost + time_value x time (only time at r = 0).
func coreScore(lam float64, o option) float64 {
	if math.IsInf(lam, 1) {
		return o.time.Seconds()
	}
	return o.cost + lam*hours(o.time)
}

// better reports whether a beats b (score, then time, cost, fit, tie).
func coreBetter(lam float64, a, b option) bool {
	sa, sb := coreScore(lam, a), coreScore(lam, b)
	switch {
	case math.Abs(sa-sb) > 1e-9*math.Max(1e-6, math.Abs(sb)):
		return sa < sb
	case a.time != b.time:
		return a.time < b.time
	case math.Abs(a.cost-b.cost) > 1e-12:
		return a.cost < b.cost
	case math.Abs(a.fit-b.fit) > 1e-9:
		return a.fit < b.fit
	}
	return a.tie > b.tie
}

// choose evaluates every option of t and returns the best (ok false when
// t has none: nothing fits or every way to get capacity is blocked).
func (p Default) coreChoose(ss *solveState, t Task, explain bool) coreChoice {
	f := ss.f
	c := p.cfg()
	lam, ok := p.timeValue(f, t.EnvHash)
	if !ok {
		lam = 0 // no prices: only... cost 0 everywhere, time decides via tie order
	}
	waited := f.Now.Sub(t.Since)
	type cand struct {
		o      option
		rc     runChoice
		cm     *cmach
		at     time.Time
		assign bool
		late   bool // exceeds the queue timeout
	}
	var cands []cand
	var rejected []Option
	consider := func(cd cand) { cands = append(cands, cd) }
	tooLate := func(wait time.Duration) bool {
		return c.QueueTimeout > 0 && waited+wait > c.QueueTimeout*8/10
	}
	// Regret bound: a wait (at the queue_confidence quantile) is an option
	// only when it is not longer than the fastest new machine takes plus
	// the hedge margin (the time already waited is sunk).
	bound, bounded := p.regretBound(ss, t)
	// Existing machines and those this solve already brings.
	for _, cm := range ss.ms[t.EnvHash] {
		tm := cm.tl
		m := tm.m
		if !p.admissible(m).Fits(t.Request) || !p.physical(f, m.Capacity, t) {
			if explain {
				rejected = append(rejected, rejectOption(OptAssign, m, 0, RejSmaller))
			}
			continue
		}
		at, ok := tm.earliest(t.Request, p.quickRun(f, tm, t))
		if !ok {
			if explain {
				rejected = append(rejected, rejectOption(OptWaitMachine, m, 0, RejNeverFrees))
			}
			continue
		}
		now := !at.After(f.Now) && m.State == Running && cm.act == nil
		if now && !p.accepting(m, t.EnvHash, f.Now) {
			continue // pressure limits: no new session there now
		}
		mm := m
		if !now {
			// It runs beside what is still reserved when it starts.
			mm.Reserved = p.admissible(m).Sub(tm.freeAt(at))
		}
		wait := max(at.Sub(f.Now), 0)
		for _, rc := range p.runChoices(f, mm, t) {
			cost := p.machineCost(f, tm.m, t, at, rc.run) + rc.cost
			kind := OptWaitMachine
			if now {
				kind = OptAssign
			}
			o := option{cost: cost, time: wait + rc.run, tie: Score(t.RepoID, m.ID), info: runOption(kind, m, t, rc, wait)}
			if now {
				o.fit = leftover(p.Free(m), t.Request, p.admissible(m))
			} else {
				o.fit = math.Inf(1)
			}
			if !now && bounded && wait > bound {
				if explain {
					rejected = append(rejected, rejectOption(OptWaitMachine, m, wait, fmt.Sprintf("%s (%s)", RejHedge, bound.Round(time.Second))))
				}
				break
			}
			if cm.act != nil {
				o.info.Kind = OptWaitMachine
				o.info.Reason = "on " + cm.act.Kind.String() + " planned for an earlier request: " + rc.reason
			}
			consider(cand{o: o, rc: rc, cm: cm, at: at, assign: now, late: !now && tooLate(wait)})
		}
	}
	// New capacity: start a stopped machine or create a size.
	newCap := func(act Action, m Machine, lat time.Duration, price float64, free Resources) {
		tm := &tlMachine{m: m, ready: f.Now.Add(lat), free: free, price: price, created: true, latency: lat}
		tail := p.idleTail(f, t.EnvHash, m.Provider, price)
		for _, rc := range p.runChoices(f, m, t) {
			cost := price*hours(lat+rc.run+tail) + rc.cost
			info := candOption(act, tm)
			info.Slot, info.RunSeconds, info.PrepSeconds = rc.slot.Steps(), secs(rc.run), secs(rc.prep)
			if rc.slot.Kind == SlotNone {
				info.Slot = "warm"
				if rc.reason == "cold" {
					info.Slot = "cold"
				}
			}
			info.Reason = act.Reason + ": " + rc.reason
			info.Tasks = 1
			o := option{cost: cost, time: lat + rc.run, tie: Score(t.EnvHash, m.ID), fit: math.Inf(1), info: info}
			a := act
			consider(cand{o: o, rc: rc, cm: &cmach{tl: tm, act: &a}, at: tm.ready})
		}
	}
	for _, m := range ss.stopped[t.EnvHash] {
		if !p.admissible(m).Fits(t.Request) || !p.physical(f, m.Capacity, t) {
			continue
		}
		price := priceOf(f, m)
		if why := p.limitBlocks(ss.st, m.Provider, m.Capacity.CPU, price, false); why != "" {
			if explain {
				rejected = append(rejected, Option{Kind: OptStart, Machine: m.ID, Rejected: RejMachineLimit + ": " + why})
			}
			continue
		}
		mm := m
		mm.State, mm.Reserved, mm.Releases = Starting, Resources{}, nil
		newCap(Action{Kind: Start, Machine: m.ID, Reason: "queued demand"}, mm, p.startLatency(f, m.Provider), price, p.admissible(m))
	}
	for _, s := range p.allowedSizes(f.SizesFor(t.EnvHash), ss.st) {
		if s.Capacity.CPU <= 0 || s.Capacity.Memory <= 0 || !p.sizeAdmissible(s).Fits(t.Request) || !p.physical(f, s.Capacity, t) {
			continue
		}
		m := Machine{ID: "new:" + s.Provider + "/" + s.Name, Provider: s.Provider, EnvHash: t.EnvHash, Size: s.Name, State: Starting, Capacity: s.Capacity,
			Medium: s.Medium, Stack: s.Stack, DiskTotal: max(s.Volume.Min, s.Capacity.Disk)}
		lat := p.boot(f, s.Provider)
		newCap(Action{Kind: Create, EnvHash: t.EnvHash, Size: s, Reason: "queued demand"}, m, lat, sizePrice(s), p.sizeAdmissible(s))
		for _, v := range p.warmVolumes(&planState{f: f, acts: ss.acts}, s, []Task{t}) {
			vm := m
			vm.ID, vm.Zone, vm.Volume, vm.DiskTotal, vm.SlotBytes = m.ID+"/"+v.ID, v.Zone, v.ID, v.Bytes, v.SlotBytes
			vm.WarmRepos = nil
			for repo := range v.SlotBytes {
				vm.WarmRepos = append(vm.WarmRepos, repo)
			}
			sort.Strings(vm.WarmRepos)
			newCap(Action{Kind: Create, EnvHash: t.EnvHash, Size: s, Volume: v.ID, Reason: "queued demand, warm volume " + v.ID},
				vm, lat+SlotMedium(s.Stack).Attach, priceOf(f, vm), p.sizeAdmissible(s))
		}
	}
	if explain && len(f.SizesFor(t.EnvHash)) > 0 {
		rejected = append(rejected, p.sizeRejections(ss.st, t.EnvHash, f.SizesFor(t.EnvHash))...)
	}
	// Waits beyond 80 % of the queue timeout only when nothing else is
	// left (a start or create is always something else).
	inTime := false
	for _, cd := range cands {
		inTime = inTime || !cd.late
	}
	best := -1
	for i, cd := range cands {
		if inTime && cd.late {
			continue
		}
		if best < 0 || coreBetter(lam, cd.o, cands[best].o) {
			best = i
		}
	}
	out := coreChoice{t: t}
	if best >= 0 {
		b := cands[best]
		out.ok, out.o, out.rc, out.cm, out.at, out.assign = true, b.o, b.rc, b.cm, b.at, b.assign
	}
	if explain {
		var opts []option
		for i, cd := range cands {
			if inTime && cd.late {
				r := *cd.o.info
				r.TimeSeconds, r.Cost, r.Rejected = secs(cd.o.time), round6(cd.o.cost), RejQueueTimeout
				rejected = append(rejected, r)
				if i < best {
					best--
				}
				continue
			}
			opts = append(opts, cd.o)
		}
		if best >= 0 {
			// index of the chosen among the kept options
			for i, o := range opts {
				if o.info == out.o.info {
					best = i
				}
			}
		}
		out.decision = p.coreDecision(lam, opts, best, rejected)
	}
	return out
}

// coreDecision builds the Decision record of a choice.
func (p Default) coreDecision(lam float64, opts []option, chosen int, rejected []Option) *Decision {
	d := &Decision{Policy: "v2", CostOptimization: p.costOptimization(), TimeValue: lam, Chosen: -1}
	if math.IsInf(lam, 1) {
		d.TimeValue = -1
	}
	for i, o := range opts {
		var op Option
		if o.info != nil {
			op = *o.info
		}
		op.TimeSeconds, op.Cost, op.Score, op.Chosen = secs(o.time), round6(o.cost), round6(coreScore(lam, o)), i == chosen
		d.Options = append(d.Options, op)
	}
	sort.SliceStable(d.Options, func(i, j int) bool {
		a, b := d.Options[i], d.Options[j]
		if a.Chosen != b.Chosen {
			return a.Chosen
		}
		return a.Score < b.Score
	})
	if chosen >= 0 {
		d.Chosen = 0
	}
	d.Options = append(d.Options, rejected...)
	return d
}

// book commits a choice: the request takes its capacity on the machine's
// timeline; a start or create joins the solve's machines.
func (p Default) bookChoice(ss *solveState, ch coreChoice) {
	cm, t := ch.cm, ch.t
	tm := cm.tl
	end := ch.at.Add(ch.rc.run)
	tm.deltas = append(tm.deltas, Release{At: ch.at, Res: Resources{}.Sub(t.Request)}, Release{At: end, Res: t.Request})
	tm.m.Releases = append(tm.m.Releases, Release{At: end, Res: t.Request})
	if ch.assign {
		tm.m.Reserved = tm.m.Reserved.Add(t.Request)
	}
	ss.booked[tm.m.ID] = true
	if cm.act != nil && cm.act.Kind == Create && cm.act.Volume == "" {
		ss.onNew[cm] = append(ss.onNew[cm], ch)
	}
	if cm.act == nil || containsMach(ss.ms[t.EnvHash], cm) {
		return
	}
	a := *cm.act
	a.Decision = ch.decision
	if a.Kind == Create && a.Volume == "" {
		ss.actIdx[cm] = len(ss.acts)
		ss.newOrder = append(ss.newOrder, cm)
	}
	ss.acts = append(ss.acts, a)
	ss.ms[t.EnvHash] = append(ss.ms[t.EnvHash], cm)
	if a.Kind == Create {
		ss.st.create(a.Size)
		return
	}
	ss.st.grow(tm.m.Provider, tm.m.Capacity.CPU, tm.price)
	ss.booked[a.Machine] = true
	st := ss.stopped[t.EnvHash]
	for i, m := range st {
		if m.ID == a.Machine {
			ss.stopped[t.EnvHash] = append(st[:i:i], st[i+1:]...)
			break
		}
	}
}

func containsMach(ms []*cmach, cm *cmach) bool {
	for _, x := range ms {
		if x == cm {
			return true
		}
	}
	return false
}

// coreQueue is the queue the solve plans for: in priority order, at most
// 32 requests (the rest is planned once these are served).
func coreQueue(now time.Time, ts []Task) []Task {
	ts = prioritize(now, ts)
	if len(ts) > 32 {
		ts = ts[:32]
	}
	return ts
}

// placeCore is Place of the v2 core: t is planned after the queued
// requests of higher priority; it runs now if its best option is a
// machine with room now, otherwise it waits (Plan starts or creates what
// its best option needs).
func (p Default) placeCore(f Pool, t Task) Placement {
	if !p.fitsAnySize(f, t) {
		reason := "task larger than every machine size"
		d := p.coreDecision(0, nil, -1, []Option{{Kind: OptReject, Rejected: reason}})
		return Placement{Kind: Reject, Reason: reason, Decision: d}
	}
	ss := p.newSolve(f)
	for _, q := range coreQueue(f.Now, append(append([]Task(nil), f.Queue...), t)) {
		if q.ID == t.ID {
			ch := p.coreChoose(ss, t, true)
			if !ch.ok {
				return Placement{Kind: Wait, Reason: "no capacity can be had (limits, outages)", Decision: ch.decision}
			}
			if !ch.assign {
				reason := ch.o.info.Reason
				if ch.cm.act == nil {
					reason = fmt.Sprintf("waiting %s for %s on %s", ch.at.Sub(f.Now).Round(time.Second), ch.rc.reason, ch.cm.tl.m.ID)
				}
				return Placement{Kind: Wait, Reason: reason, Decision: ch.decision}
			}
			return Placement{Kind: Assign, Machine: ch.cm.tl.m.ID, Grant: p.Grant(t), Reason: ch.rc.reason, Slot: ch.rc.slot,
				Expected: t.Profile.High(ch.rc.run), Run: ch.rc.run, Decision: ch.decision}
		}
		if ch := p.coreChoose(ss, q, false); ch.ok {
			p.bookChoice(ss, ch)
		}
	}
	return Placement{Kind: Wait, Reason: "not planned"}
}

// planCore is Plan of the v2 core: the queue's starts and creates, then
// the lifecycle of idle and stopped machines and the warm pool.
func (p Default) planCore(f Pool) []Action {
	ss := p.newSolve(f)
	for _, t := range coreQueue(f.Now, f.Queue) {
		if !p.fitsAnySize(f, t) {
			continue
		}
		explain := true
		if ch := p.coreChoose(ss, t, explain); ch.ok {
			p.bookChoice(ss, ch)
		}
	}
	p.packCreates(ss)
	st := p.newPlanState(f)
	st.acts = ss.acts
	running := p.lifecycle(st, ss.booked)
	p.warmPool(st, running)
	p.enforceLimits(st)
	return st.acts
}

// regretBound is the longest total wait a request accepts for existing
// capacity: the fastest start or create that fits it plus the hedge
// margin 30s/(1-r) (none at r = 1, or when no machine can be had).
func (p Default) regretBound(ss *solveState, t Task) (time.Duration, bool) {
	margin, ok := p.hedgeMargin()
	if !ok {
		return 0, false
	}
	f := ss.f
	best := time.Duration(-1)
	for _, m := range ss.stopped[t.EnvHash] {
		if p.admissible(m).Fits(t.Request) && p.physical(f, m.Capacity, t) &&
			p.limitBlocks(ss.st, m.Provider, m.Capacity.CPU, priceOf(f, m), false) == "" {
			if l := p.startLatency(f, m.Provider); best < 0 || l < best {
				best = l
			}
		}
	}
	for _, s := range p.allowedSizes(f.SizesFor(t.EnvHash), ss.st) {
		if p.sizeAdmissible(s).Fits(t.Request) && p.physical(f, s.Capacity, t) {
			if l := p.boot(f, s.Provider); best < 0 || l < best {
				best = l
			}
		}
	}
	if best < 0 {
		return 0, false
	}
	return best + margin, true
}

// freeAt is the free capacity of the timeline machine at t (after the
// releases and bookings up to t).
func (tm *tlMachine) freeAt(t time.Time) Resources {
	r := tm.free
	for _, d := range tm.deltas {
		if !d.At.After(t) {
			r = r.Add(d.Res)
		}
	}
	return r
}

// packCreates revisits the creates of one solve per environment: the
// requests booked on two or more new machines may be served cheaper by
// fewer machines of a larger size (each request chose the cheapest
// machine for itself). Requests are packed largest first onto machines
// of each allowed size, all starting when the machines are ready; the
// packing replaces the creates when its total cost + time_value x time
// is lower.
func (p Default) packCreates(ss *solveState) {
	f := ss.f
	byEnv := map[string][]*cmach{}
	var envs []string
	for _, cm := range ss.newOrder {
		env := cm.act.EnvHash
		if len(byEnv[env]) == 0 {
			envs = append(envs, env)
		}
		byEnv[env] = append(byEnv[env], cm)
	}
	for _, env := range envs {
		cms := byEnv[env]
		if len(cms) < 2 {
			continue
		}
		lam, _ := p.timeValue(f, env)
		var chs []coreChoice
		cur := 0.0
		for _, cm := range cms {
			for _, ch := range ss.onNew[cm] {
				chs = append(chs, ch)
				cur += coreScore(lam, ch.o)
			}
		}
		sort.SliceStable(chs, func(i, j int) bool {
			return share(chs[i].t.Request, Resources{CPU: 1, Memory: 1 << 30}) > share(chs[j].t.Request, Resources{CPU: 1, Memory: 1 << 30})
		})
		bestScore, bestN := cur, 0
		var bestSize Size
		for _, s := range p.allowedSizes(f.SizesFor(env), ss.st) {
			adm := p.sizeAdmissible(s)
			lat := p.boot(f, s.Provider)
			tail := p.idleTail(f, env, s.Provider, sizePrice(s))
			var bins []Resources
			var ends []time.Duration
			score, ok := 0.0, true
			for _, ch := range chs {
				if !adm.Fits(ch.t.Request) || !p.physical(f, s.Capacity, ch.t) {
					ok = false
					break
				}
				run := ch.rc.run
				placed := false
				for i := range bins {
					if bins[i].Fits(ch.t.Request) {
						bins[i] = bins[i].Sub(ch.t.Request)
						ends[i] = max(ends[i], run)
						placed = true
						break
					}
				}
				if !placed {
					bins = append(bins, adm.Sub(ch.t.Request))
					ends = append(ends, run)
				}
				score += lam * hours(lat+run)
			}
			if !ok || len(bins) >= len(cms) {
				continue
			}
			for _, e := range ends {
				score += sizePrice(s) * hours(lat+e+tail)
			}
			if score < bestScore-1e-9 {
				bestScore, bestN, bestSize = score, len(bins), s
			}
		}
		if bestN == 0 {
			continue
		}
		// Replace the environment's creates by bestN of bestSize.
		drop := map[int]bool{}
		for _, cm := range cms {
			drop[ss.actIdx[cm]] = true
		}
		var kept []Action
		var dec *Decision
		for i, a := range ss.acts {
			if drop[i] {
				if dec == nil {
					dec = a.Decision
				}
				continue
			}
			kept = append(kept, a)
		}
		ss.acts = kept
		for i := 0; i < bestN; i++ {
			ss.acts = append(ss.acts, Action{Kind: Create, EnvHash: env, Size: bestSize, Decision: dec,
				Reason: fmt.Sprintf("queued demand: %d requests on %d %s", len(chs), bestN, bestSize.Name)})
		}
	}
}

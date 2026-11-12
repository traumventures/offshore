package scheduler

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Queue confidence (default policy): a request waits for capacity (a busy
// warm machine, a starting machine, the capacity already on its way) only
// when the scheduler is confident that waiting finishes it sooner than
// running it now (a cold or restored slot on a machine with room, a start
// or a create):
//
//	P(finish_wait < finish_now + slack) >= queue_confidence (0.8)
//
// finish_wait is the wait (the remaining run times of the sessions it
// waits for: log-normal with the key's median and 80th percentile,
// conditional on having run until now, comonotone across sessions) plus
// the request's run there; finish_now the latency of the alternative
// (start/boot, deterministic) plus its run. The request's own run time is
// uncertain too, but in the same way for both (one build, comonotone).
// A session whose remaining time is unknown (no lease estimate) is not
// counted on to end. slack = (cost_now - cost_wait) / time_value lets
// cost_optimization bias the decision: at 0 only time counts, at 1 a
// cheaper wait is always taken.

// DefaultQueueConfidence is queue_confidence when unset.
const DefaultQueueConfidence = 0.8

// queueGrid is the number of quantiles of each distribution.
const queueGrid = 16

// neverWait stands for a wait that is not known to end.
const neverWait = time.Duration(math.MaxInt64 / 4)

// queueConf reports whether queue confidence decides (default policy,
// not ablated) and the threshold.
func (p Default) queueConf() (float64, bool) {
	if !p.global() || abl("queueconf") {
		return 0, false
	}
	c := p.Cfg.QueueConfidence
	if c <= 0 || c > 1 {
		c = DefaultQueueConfidence
	}
	return c, true
}

// QueueVerdict is a queue-confidence decision for the event log: the
// median finish times of the best wait and of the best way to run now,
// the probability that waiting finishes first and the threshold.
type QueueVerdict struct {
	Verdict           string  `json:"verdict"` // "wait", "run now", "start now" or "create now"
	P                 float64 `json:"p"`
	Confidence        float64 `json:"confidence"`
	WaitFinishSeconds float64 `json:"wait_finish_seconds"` // median; -1: not known to end
	NowFinishSeconds  float64 `json:"now_finish_seconds"`  // median; -1: no way to run now
	SlackSeconds      float64 `json:"slack_seconds,omitempty"`
}

// Short is the verdict for `offshore top`: "wait: p=0.86".
func (v *QueueVerdict) Short() string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%s: p=%.2f", v.Verdict, v.P)
}

// Long adds the median finish times: "wait: p=0.86 (finish 3m10s
// waiting vs 4m0s now)".
func (v *QueueVerdict) Long() string {
	if v == nil {
		return ""
	}
	fin := func(s float64) string {
		if s < 0 {
			return "never"
		}
		return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
	}
	return fmt.Sprintf("%s (finish %s waiting vs %s now)", v.Short(), fin(v.WaitFinishSeconds), fin(v.NowFinishSeconds))
}

func gridQ(k int) float64 { return (float64(k) + 0.5) / queueGrid }

// runSigma is the log-normal sigma of a key's run time (80th percentile =
// median x Spread; 1.5 when unknown).
func runSigma(pr Profile) float64 {
	sp := pr.Spread
	if sp < 1 {
		sp = 1.5
	}
	return math.Min(math.Max(math.Log(sp)/0.8416, 0.05), 1.5)
}

// runFactor is the run time at quantile u relative to the median.
func runFactor(sigma, u float64) float64 {
	return math.Exp(sigma * math.Sqrt2 * math.Erfinv(2*u-1))
}

// releaseQ is r's end at quantile q of its remaining time; ok is false
// when its remaining time is unknown (no lease estimate).
func releaseQ(r Release, q float64, now time.Time) (time.Time, bool) {
	if r.Unknown || r.At.IsZero() {
		return time.Time{}, false
	}
	if r.Run <= 0 || r.Start.IsZero() || !r.At.After(r.Start.Add(r.Run)) {
		// No spread known: the point ETA, until the session overruns it
		// (then nothing is known about its end).
		if r.At.Before(now) {
			return time.Time{}, false
		}
		return r.At, true
	}
	return quantileEnd(r, q, now), true
}

// waitQuantiles returns the wait until need fits on m at each quantile
// of the remaining run times of its sessions (ascending; neverWait when
// the releases it needs are not known to end).
func (p Default) waitQuantiles(f Pool, m Machine, need Resources) []time.Duration {
	out := make([]time.Duration, queueGrid)
	adm := p.admissible(m)
	if !adm.Fits(need) {
		for k := range out {
			out[k] = neverWait
		}
		return out
	}
	start := f.Now
	if m.State == Starting {
		start = m.ReadyAt
		if start.IsZero() || start.Before(f.Now) {
			start = f.Now.Add(p.startLatency(f, m.Provider) / 2)
		}
	}
	type rel struct {
		at  time.Time
		res Resources
	}
	for k := range out {
		q := gridQ(k)
		free := adm.Sub(m.Reserved)
		var rs []rel
		for _, r := range m.Releases {
			if at, ok := releaseQ(r, q, f.Now); ok {
				rs = append(rs, rel{at, r.Res})
			}
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].at.Before(rs[j].at) })
		at, ok := start, free.Fits(need)
		for _, r := range rs {
			if ok {
				break
			}
			free = free.Add(r.res)
			if free.Fits(need) {
				ok = true
				if r.at.After(at) {
					at = r.at
				}
			}
		}
		if !ok {
			out[k] = neverWait
			continue
		}
		out[k] = max(at.Sub(f.Now), 0)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// queueSlack is the time a wait may lose against running now and still
// be preferred because it is cheaper (negative: it must win by that much
// because it is dearer): (costNow - costWait) / time value.
func (p Default) queueSlack(f Pool, env string, costNow, costWait float64) time.Duration {
	lam, ok := p.timeValue(f, env)
	if !ok || math.IsInf(lam, 1) {
		return 0
	}
	d := costNow - costWait
	if lam <= 0 {
		switch {
		case d > 1e-12:
			return 1000 * time.Hour
		case d < -1e-12:
			return -1000 * time.Hour
		}
		return 0
	}
	h := math.Max(math.Min(d/lam, 1000), -1000)
	return time.Duration(h * float64(time.Hour))
}

// probWaitFirst is P(wait + waitRun·F < nowLead + nowRun·F + slack) over
// the wait quantiles and the request's run factor F (log-normal, sigma),
// with the median finish times of both.
func probWaitFirst(waits []time.Duration, waitRun, nowLead, nowRun time.Duration, sigma float64, slack time.Duration) (p float64, waitMed, nowMed time.Duration) {
	hit := 0
	for _, w := range waits {
		if w >= neverWait {
			continue
		}
		for k := 0; k < queueGrid; k++ {
			fct := runFactor(sigma, gridQ(k))
			if float64(w)+float64(waitRun)*fct < float64(nowLead)+float64(nowRun)*fct+float64(slack) {
				hit++
			}
		}
	}
	if len(waits) > 0 {
		p = float64(hit) / float64(len(waits)*queueGrid)
	}
	waitMed = neverWait
	if len(waits) > 0 {
		if w := waits[(len(waits)-1)/2]; w < neverWait {
			waitMed = w + waitRun
		}
	}
	return p, waitMed, nowLead + nowRun
}

func verdictSeconds(d time.Duration) float64 {
	if d >= neverWait || d < 0 {
		return -1
	}
	return secs(d)
}

// projected is a request's projected start and run in a timeline.
type projection struct {
	at  time.Time
	run time.Duration
}

// tlOfQ is tm with its sessions' releases at quantile q of their
// remaining times; releases not known to end, and reservations without a
// lease estimate, are not counted on (a day away). Sessions a plan put on
// a hypothetical machine keep their planned changes.
func (p Default) tlOfQ(f Pool, tm *tlMachine, q float64) *tlMachine {
	c := *tm
	if tm.created {
		c.deltas = append([]Release(nil), tm.deltas...)
		return &c
	}
	m := tm.m
	c.deltas, c.guess = nil, nil
	never := f.Now.Add(24 * time.Hour)
	covered := Resources{}
	for _, r := range m.Releases {
		at, ok := releaseQ(r, q, f.Now)
		if !ok {
			at = never
		}
		c.deltas = append(c.deltas, Release{At: at, Res: r.Res})
		covered = covered.Add(r.Res)
	}
	if rest := m.Reserved.Sub(covered); rest.CPU > 1e-9 || rest.Memory > 0 {
		c.deltas = append(c.deltas, Release{At: never, Res: Resources{CPU: math.Max(rest.CPU, 0), Memory: max(rest.Memory, 0)}})
	}
	return &c
}

// projectDist list-schedules tasks on ms at each quantile of the
// sessions' remaining times (see project): per quantile, each task's
// projected start and run (missing: never).
func (p Default) projectDist(f Pool, ms []*tlMachine, tasks []Task) []map[string]projection {
	out := make([]map[string]projection, queueGrid)
	for k := range out {
		qms := make([]*tlMachine, len(ms))
		for i, tm := range ms {
			qms[i] = p.tlOfQ(f, tm, gridQ(k))
		}
		out[k] = p.projectRuns(f, qms, tasks)
	}
	return out
}

// queueVerdictNew is the queue-confidence verdict for t queued behind the
// projection proj against a new machine cand (its latency plus t's run
// there): ok is true when waiting is confidently better.
func (p Default) queueVerdictNew(f Pool, t Task, proj []map[string]projection, cand *tlMachine, conf float64) (QueueVerdict, bool) {
	waits := make([]time.Duration, 0, len(proj))
	var runs []time.Duration
	for _, m := range proj {
		if pr, ok := m[t.ID]; ok {
			waits, runs = append(waits, max(pr.at.Sub(f.Now), 0)), append(runs, pr.run)
		} else {
			waits = append(waits, neverWait)
		}
	}
	waitRun := time.Duration(0)
	if len(runs) > 0 {
		sort.Slice(runs, func(i, j int) bool { return runs[i] < runs[j] })
		waitRun = runs[(len(runs)-1)/2]
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	nowRun := p.quickRun(f, cand, t)
	// What the new machine costs beyond the run itself: its latency and
	// its idle tail.
	extra := cand.price * hours(cand.latency+p.cfg().IdleStop)
	slack := p.queueSlack(f, t.EnvHash, extra, 0)
	pr, wm, nm := probWaitFirst(waits, waitRun, cand.latency, nowRun, runSigma(t.Profile), slack)
	kind := "start now"
	if strings.HasPrefix(cand.m.ID, "new:") {
		kind = "create now"
	}
	v := QueueVerdict{Verdict: kind, P: math.Round(pr*100) / 100, Confidence: conf, WaitFinishSeconds: verdictSeconds(wm),
		NowFinishSeconds: verdictSeconds(nm), SlackSeconds: secs(slack)}
	if pr >= conf {
		v.Verdict = "wait"
		return v, true
	}
	return v, false
}

// chooseQueued picks t's option under queue confidence: the best-scoring
// wait among those that finish sooner than the best way to run now with
// probability >= conf, else the best way to run now. waitQ holds each
// wait option's wait quantiles (nil for the options that run now) and
// waitRun its run after the wait.
func (p Default) chooseQueued(f Pool, t Task, opts []option, pls []Placement, waitQ [][]time.Duration, waitRun []time.Duration, conf float64) (int, *QueueVerdict) {
	var nowIdx, waitIdx []int
	for i, pl := range pls {
		if pl.Kind == Wait {
			waitIdx = append(waitIdx, i)
		} else {
			nowIdx = append(nowIdx, i)
		}
	}
	sub := func(idx []int) int {
		os := make([]option, len(idx))
		for k, i := range idx {
			os[k] = opts[i]
		}
		if k := p.choose(f, t.EnvHash, os); k >= 0 {
			return idx[k]
		}
		return -1
	}
	bn := sub(nowIdx)
	if len(waitIdx) == 0 {
		return bn, nil
	}
	if bn < 0 {
		return sub(waitIdx), nil
	}
	sigma := runSigma(t.Profile)
	var ok []int
	var vs = map[int]QueueVerdict{}
	bestP := -1
	for _, i := range waitIdx {
		wq := waitQ[i]
		if wq == nil {
			continue
		}
		run := waitRun[i]
		slack := p.queueSlack(f, t.EnvHash, opts[bn].cost, opts[i].cost)
		pr, wm, nm := probWaitFirst(wq, run, 0, opts[bn].time, sigma, slack)
		v := QueueVerdict{Verdict: "run now", P: math.Round(pr*100) / 100, Confidence: conf, WaitFinishSeconds: verdictSeconds(wm),
			NowFinishSeconds: verdictSeconds(nm), SlackSeconds: secs(slack)}
		if pr >= conf {
			v.Verdict = "wait"
			ok = append(ok, i)
		}
		vs[i] = v
		if bestP < 0 || v.P > vs[bestP].P {
			bestP = i
		}
	}
	if len(ok) > 0 {
		w := sub(ok)
		v := vs[w]
		return w, &v
	}
	if bestP < 0 {
		return bn, nil
	}
	v := vs[bestP]
	return bn, &v
}

// queueVerdictQueued is the verdict for t when no machine has room: the
// queue of its environment (t included) projected on the machines there
// and on their way, against the new machine (start or create) most
// likely to finish t sooner. ok is false when nothing is known.
func (p Default) queueVerdictQueued(f Pool, t Task, conf float64) (QueueVerdict, bool) {
	env := t.EnvHash
	var ms []*tlMachine
	for _, m := range f.Machines {
		if m.EnvHash != env || m.Draining || !p.usable(m, f.Now) || (m.State != Running && m.State != Starting) {
			continue
		}
		ms = append(ms, p.tlOf(f, m))
	}
	tasks := []Task{t}
	for _, q := range f.Queue {
		if q.EnvHash == env && q.ID != t.ID {
			tasks = append(tasks, q)
		}
	}
	tasks = prioritize(f.Now, tasks)
	if len(tasks) > 16 {
		tasks = tasks[:16]
	}
	cands := p.newMachines(f, p.newPlanState(f), env)
	if len(cands) == 0 {
		return QueueVerdict{Verdict: "wait", P: 1, Confidence: conf, WaitFinishSeconds: -1, NowFinishSeconds: -1}, true
	}
	proj := p.projectDist(f, ms, tasks)
	var best QueueVerdict
	found := false
	for _, c := range cands {
		if !p.admissible(c.m).Fits(t.Request) || !p.physical(f, c.m.Capacity, t) {
			continue
		}
		v, _ := p.queueVerdictNew(f, t, proj, c, conf)
		if !found || v.P < best.P || v.P == best.P && v.NowFinishSeconds < best.NowFinishSeconds {
			best, found = v, true
		}
	}
	return best, found
}

// newMachines are the machines scale-out could add for env now: its
// stopped machines (start) and the allowed sizes (create), as timeline
// machines ready after their latency. None without room (limits).
func (p Default) newMachines(f Pool, st *planState, env string) []*tlMachine {
	if !p.room(st, env) && !p.freeSlotPossible(st, env) {
		return nil
	}
	c := p.cfg()
	var out []*tlMachine
	for _, m := range f.Machines {
		if m.EnvHash != env || m.State != Stopped || !p.usable(m, f.Now) || f.Now.Before(m.StartBlocked) || c.Disabled(m.Provider) {
			continue
		}
		tm := p.tlOf(f, m)
		tm.m.State, tm.created, tm.price = Starting, true, priceOf(f, m)
		tm.latency = p.startLatency(f, m.Provider)
		tm.ready, tm.free, tm.deltas = f.Now.Add(tm.latency), p.admissible(m), nil
		out = append(out, tm)
	}
	for _, s := range p.allowedSizes(f.SizesFor(env), st) {
		if s.Capacity.CPU <= 0 || s.Capacity.Memory <= 0 {
			continue
		}
		m := Machine{ID: "new:" + s.Provider + "/" + s.Name, Provider: s.Provider, EnvHash: env, Size: s.Name, State: Starting, Capacity: s.Capacity,
			Medium: s.Medium, Stack: s.Stack, DiskTotal: max(s.Volume.Min, s.Capacity.Disk)}
		lat := p.boot(f, s.Provider)
		out = append(out, &tlMachine{m: m, ready: f.Now.Add(lat), free: p.sizeAdmissible(s), price: sizePrice(s), created: true, latency: lat})
	}
	return out
}

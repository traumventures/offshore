// Package sim is a discrete-event simulator for scheduling policies. It
// replays a trace of build sessions against a scheduler.Policy, machine
// sizes and provider latencies the way the orchestrator drives the policy
// (Place for every waiting task, Plan while tasks wait and on every
// reconcile tick) and models what the policy cannot see: boot and start
// latency, CPU overcommit slowing tasks down, memory beyond physical
// capacity swapping or being OOM killed, cold build caches.
package sim

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// Options configures a simulation run.
type Options struct {
	Name      string           // label in reports
	Policy    scheduler.Policy // decides placement and fleet changes
	Sizes     []scheduler.Size // machine types the policy may create
	Estimator scheduler.Estimator
	// Boot and Start are the latencies until a created or a started machine
	// accepts sessions, per provider ("" = default for all providers).
	Boot, Start map[string]time.Duration
	MaxLifetime time.Duration // machine lifetime (default 24h)
	Reconcile   time.Duration // periodic Plan (default 15s, like the daemon)
	// ColdPenalty extends a build without a warm cache on its machine:
	// duration * (1 + ColdPenalty). Default 0.5; negative = no penalty.
	ColdPenalty float64
	// Swap lets memory beyond physical capacity swap: every task on the
	// machine slows down by (physical/used)^SwapSlowdown (default 2).
	// Without swap the most recently started tasks are OOM killed and
	// retried (up to Retries times, default 3).
	Swap         bool
	SwapSlowdown float64
	Retries      int
	// RecentTTL drops environments from the warm pool after this long
	// without use (default 24h, like the orchestrator).
	RecentTTL time.Duration
	// QueueTimeout fails tasks waiting longer (0 = wait forever), like
	// fleet.queue_timeout.
	QueueTimeout time.Duration
	// MinBilling is the minimum billed time per create or start (default
	// 60s like EC2; negative = none).
	MinBilling time.Duration
	// Debug, when set, is called for every completed job with its wait.
	Debug func(j Job, wait time.Duration)
}

func (o *Options) defaults() {
	if o.MaxLifetime <= 0 {
		o.MaxLifetime = 24 * time.Hour
	}
	if o.Reconcile <= 0 {
		o.Reconcile = 15 * time.Second
	}
	if o.ColdPenalty == 0 {
		o.ColdPenalty = 0.5
	} else if o.ColdPenalty < 0 {
		o.ColdPenalty = 0
	}
	if o.SwapSlowdown <= 0 {
		o.SwapSlowdown = 2
	}
	if o.Retries <= 0 {
		o.Retries = 3
	}
	if o.RecentTTL <= 0 {
		o.RecentTTL = 24 * time.Hour
	}
	if o.MinBilling == 0 {
		o.MinBilling = time.Minute
	} else if o.MinBilling < 0 {
		o.MinBilling = 0
	}
	if o.Estimator == nil {
		o.Estimator = scheduler.Static{}
	}
}

func latency(m map[string]time.Duration, prov string, def time.Duration) time.Duration {
	if d, ok := m[prov]; ok {
		return d
	}
	if d, ok := m[""]; ok {
		return d
	}
	return def
}

// Report summarises a simulation run.
type Report struct {
	Name         string         `json:"name"`
	Jobs         int            `json:"jobs"`
	Completed    int            `json:"completed"`
	Failed       int            `json:"failed"`   // not completed: rejected, timed out or OOM killed too often
	Rejected     int            `json:"rejected"` // larger than every size
	TimedOut     int            `json:"timed_out"`
	Cost         float64        `json:"cost"` // machine-hours * price
	MachineHours float64        `json:"machine_hours"`
	Machines     int            `json:"machines"` // machines created
	MaxRunning   int            `json:"max_running"`
	WaitP50      time.Duration  `json:"wait_p50"`
	WaitP95      time.Duration  `json:"wait_p95"`
	WaitMax      time.Duration  `json:"wait_max"`
	TurnP50      time.Duration  `json:"turnaround_p50"` // wait + run time
	TurnP95      time.Duration  `json:"turnaround_p95"`
	CPUUtil      float64        `json:"cpu_util"` // used / provisioned core-seconds
	MemUtil      float64        `json:"mem_util"`
	Slowdown     float64        `json:"slowdown"` // mean run time / warm full-speed duration
	OOMs         int            `json:"ooms"`
	Swapped      int            `json:"swapped"` // jobs that ran while their machine swapped
	WarmHits     int            `json:"warm_hits"`
	WarmRate     float64        `json:"warm_rate"`
	Actions      map[string]int `json:"actions"`
}

type job struct {
	Job
	n        int
	arrival  time.Time
	since    time.Time // waiting since (current attempt)
	wait     time.Duration
	attempts int
	done     bool
	swapped  bool
	runFor   time.Duration
}

type run struct {
	j       *job
	reserve scheduler.Resources
	work    float64 // remaining seconds at full speed
	speed   float64
	seq     int
	start   time.Time
}

type machine struct {
	id       string
	prov     string
	env      string
	size     scheduler.Size
	state    scheduler.State
	readyAt  time.Time
	created  time.Time
	expires  time.Time
	draining bool
	runs     []*run
	warm     map[string]bool
	idle     time.Time
	up       time.Time // compute billed since (create or start)
	swapping bool
}

type sim struct {
	o        Options
	now      time.Time
	machines []*machine
	queue    []*job
	recent   map[string]time.Time
	seq      int
	ids      int
	r        Report

	cpuUsed, cpuProv, memUsed, memProv float64
}

// Run simulates trace under o.
func Run(trace Trace, o Options) Report {
	o.defaults()
	s := &sim{o: o, now: trace.Start, recent: map[string]time.Time{}}
	s.r = Report{Name: o.Name, Jobs: len(trace.Jobs), Actions: map[string]int{}}
	jobs := make([]*job, len(trace.Jobs))
	last := 0.0
	for i, tj := range trace.Jobs {
		at := trace.Start.Add(time.Duration(tj.At * float64(time.Second)))
		jobs[i] = &job{Job: tj, n: i, arrival: at}
		last = math.Max(last, tj.At)
	}
	end := trace.Start.Add(time.Duration(math.Max(trace.End, last+6*3600) * float64(time.Second)))
	next := 0
	tick := trace.Start
	var waits, turns []time.Duration
	var slow float64
	for {
		s.speeds()
		// Next event.
		t := tick
		if next < len(jobs) && jobs[next].arrival.Before(t) {
			t = jobs[next].arrival
		}
		for _, m := range s.machines {
			if m.state == scheduler.Starting && m.readyAt.Before(t) {
				t = m.readyAt
			}
			for _, r := range m.runs {
				if r.speed > 0 {
					if c := s.now.Add(time.Duration(math.Ceil(r.work / r.speed * 1e9))); c.Before(t) {
						t = c
					}
				}
			}
		}
		pending := next < len(jobs) || len(s.queue) > 0 || s.busy()
		if !pending && !t.Before(end) || t.After(end.Add(7*24*time.Hour)) {
			break
		}
		if t.Before(s.now) {
			t = s.now
		}
		s.advance(t.Sub(s.now))
		s.now = t
		// Completions.
		for _, m := range s.machines {
			kept := m.runs[:0]
			for _, r := range m.runs {
				if r.work > 1e-6 {
					kept = append(kept, r)
					continue
				}
				r.j.done = true
				r.j.runFor = s.now.Sub(r.start)
				s.r.Completed++
				waits = append(waits, r.j.wait)
				if s.o.Debug != nil {
					s.o.Debug(r.j.Job, r.j.wait)
				}
				turns = append(turns, s.now.Sub(r.j.arrival))
				slow += r.j.runFor.Seconds() / math.Max(r.j.Duration, 1e-3)
				if r.j.swapped {
					s.r.Swapped++
				}
				m.warm[r.j.Repo] = true
				s.o.Estimator.Observe(r.j.Repo, scheduler.Usage{PeakMemory: int64(r.j.PeakMemory), CPUSeconds: r.j.CPU * r.j.Duration,
					Wall: r.j.runFor})
			}
			m.runs = kept
			if len(m.runs) == 0 && m.idle.IsZero() && m.state == scheduler.Running {
				m.idle = s.now
			}
		}
		// Machines becoming ready.
		for _, m := range s.machines {
			if m.state == scheduler.Starting && !m.readyAt.After(s.now) {
				m.state = scheduler.Running
				m.idle = s.now
			}
		}
		for next < len(jobs) && !jobs[next].arrival.After(s.now) {
			j := jobs[next]
			next++
			j.since = j.arrival
			s.recent[j.Env] = s.now
			s.enqueue(j)
		}
		isTick := !tick.After(s.now)
		if isTick {
			tick = s.now.Add(s.o.Reconcile)
			s.reconcile()
		}
		s.settle(isTick)
	}
	s.advance(0)
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	if n := len(waits); n > 0 {
		s.r.WaitP50 = waits[(n-1)/2]
		s.r.WaitP95 = waits[int(math.Ceil(0.95*float64(n)))-1]
		s.r.WaitMax = waits[n-1]
		s.r.Slowdown = slow / float64(n)
		sort.Slice(turns, func(i, j int) bool { return turns[i] < turns[j] })
		s.r.TurnP50 = turns[(n-1)/2]
		s.r.TurnP95 = turns[int(math.Ceil(0.95*float64(n)))-1]
	}
	for _, j := range jobs {
		if !j.done {
			s.r.Failed++
		}
	}
	if s.cpuProv > 0 {
		s.r.CPUUtil = s.cpuUsed / s.cpuProv
		s.r.MemUtil = s.memUsed / s.memProv
	}
	if s.r.Completed > 0 {
		s.r.WarmRate = float64(s.r.WarmHits) / float64(s.r.Completed+s.r.OOMs)
	}
	return s.r
}

func (s *sim) busy() bool {
	for _, m := range s.machines {
		if len(m.runs) > 0 {
			return true
		}
	}
	return false
}

func (s *sim) enqueue(j *job) {
	s.queue = append(s.queue, j)
}

// speeds computes how fast every running task progresses.
func (s *sim) speeds() {
	for _, m := range s.machines {
		var cpu float64
		var mem int64
		for _, r := range m.runs {
			cpu += r.j.CPU
			mem += int64(r.j.PeakMemory)
		}
		f := 1.0
		if cpu > m.size.Capacity.CPU && cpu > 0 {
			f = m.size.Capacity.CPU / cpu
		}
		m.swapping = false
		if mem > m.size.Capacity.Memory && m.size.Capacity.Memory > 0 {
			m.swapping = true
			f *= math.Pow(float64(m.size.Capacity.Memory)/float64(mem), s.o.SwapSlowdown)
		}
		for _, r := range m.runs {
			r.speed = f
			if m.swapping {
				r.j.swapped = true
			}
		}
	}
}

// advance moves time forward by d: task progress, billing, utilisation.
func (s *sim) advance(d time.Duration) {
	sec := d.Seconds()
	running := 0
	for _, m := range s.machines {
		if m.state != scheduler.Running && m.state != scheduler.Starting {
			continue
		}
		running++
		c := m.size.Capacity
		s.r.MachineHours += sec / 3600
		s.r.Cost += sec / 3600 * m.size.PricePerHour
		s.cpuProv += c.CPU * sec
		s.memProv += float64(c.Memory) * sec
		var cpu float64
		var mem int64
		for _, r := range m.runs {
			r.work -= r.speed * sec
			cpu += r.j.CPU
			mem += int64(r.j.PeakMemory)
		}
		s.cpuUsed += math.Min(cpu, c.CPU) * sec
		s.memUsed += math.Min(float64(mem), float64(c.Memory)) * sec
	}
	s.r.MaxRunning = max(s.r.MaxRunning, running)
}

func (s *sim) task(j *job) scheduler.Task {
	fallback := scheduler.Resources{CPU: j.CPU, Memory: int64(j.Memory)}
	return scheduler.Task{ID: fmt.Sprintf("j%d", j.n), EnvHash: j.Env, RepoID: j.Repo, Key: j.Repo,
		Request: s.o.Estimator.Estimate(j.Repo, fallback), Since: j.since}
}

// fleet is the policy's view, built like the orchestrator's fleetLocked.
func (s *sim) fleet() scheduler.Fleet {
	f := scheduler.Fleet{Now: s.now, Sizes: s.o.Sizes, Recent: map[string]time.Time{}}
	for env, t := range s.recent {
		f.Recent[env] = t
	}
	for _, m := range s.machines {
		sm := scheduler.Machine{ID: m.id, Provider: m.prov, EnvHash: m.env, Size: m.size.Name, State: m.state,
			Draining: m.draining, Capacity: m.size.Capacity, CreatedAt: m.created, ExpiresAt: m.expires}
		if m.state == scheduler.Running {
			var cpu float64
			var mem int64
			for _, r := range m.runs {
				sm.Reserved = sm.Reserved.Add(r.reserve)
				cpu += r.j.CPU
				mem += int64(r.j.PeakMemory)
			}
			sm.Used = scheduler.Resources{CPU: math.Min(cpu, m.size.Capacity.CPU), Memory: min(mem, m.size.Capacity.Memory)}
			if cpu > m.size.Capacity.CPU {
				sm.Pressure.CPU = 1 - m.size.Capacity.CPU/cpu
			}
			if mem > m.size.Capacity.Memory {
				sm.Pressure.Memory = 1 - float64(m.size.Capacity.Memory)/float64(mem)
			}
			sm.Sessions = len(m.runs)
			for repo := range m.warm {
				sm.WarmRepos = append(sm.WarmRepos, repo)
			}
			sort.Strings(sm.WarmRepos)
			if len(m.runs) == 0 {
				sm.IdleSince = m.idle
			}
		}
		f.Machines = append(f.Machines, sm)
	}
	for _, j := range s.queue {
		f.Queue = append(f.Queue, s.task(j))
	}
	return f
}

func (s *sim) find(id string) *machine {
	for _, m := range s.machines {
		if m.id == id {
			return m
		}
	}
	return nil
}

// settle places waiting tasks; while tasks wait (and on ticks) it executes
// the policy's plan, as the orchestrator's Acquire loop and Reconcile do.
func (s *sim) settle(tick bool) {
	s.placeQueued()
	if len(s.queue) == 0 && !tick {
		return
	}
	s.execute(s.o.Policy.Plan(s.fleet()))
	s.placeQueued()
}

func (s *sim) placeQueued() {
	for _, j := range append([]*job(nil), s.queue...) {
		if s.o.QueueTimeout > 0 && s.now.Sub(j.since) > s.o.QueueTimeout {
			s.dequeue(j)
			s.r.TimedOut++
			continue
		}
		t := s.task(j)
		f := s.fleet()
		f.Queue = slices.DeleteFunc(f.Queue, func(o scheduler.Task) bool { return o.ID == t.ID })
		pl := s.o.Policy.Place(f, t)
		switch pl.Kind {
		case scheduler.Assign:
			if m := s.find(pl.Machine); m != nil && m.state == scheduler.Running {
				s.dequeue(j)
				s.assign(m, j, pl.Grant.Reserve)
			}
		case scheduler.Reject:
			s.dequeue(j)
			s.r.Rejected++
		}
	}
}

func (s *sim) dequeue(j *job) {
	s.queue = slices.DeleteFunc(s.queue, func(o *job) bool { return o == j })
}

func (s *sim) assign(m *machine, j *job, reserve scheduler.Resources) {
	j.wait += s.now.Sub(j.since)
	j.attempts++
	work := j.Duration
	if m.warm[j.Repo] {
		s.r.WarmHits++
	} else {
		work *= 1 + s.o.ColdPenalty
	}
	s.seq++
	m.runs = append(m.runs, &run{j: j, reserve: reserve, work: work, seq: s.seq, start: s.now})
	m.idle = time.Time{}
	if !s.o.Swap {
		s.oom(m)
	}
}

// oom kills the most recently started tasks until memory fits.
func (s *sim) oom(m *machine) {
	for {
		var mem int64
		for _, r := range m.runs {
			mem += int64(r.j.PeakMemory)
		}
		if mem <= m.size.Capacity.Memory || len(m.runs) == 0 {
			return
		}
		k := len(m.runs) - 1
		r := m.runs[k]
		m.runs = m.runs[:k]
		s.r.OOMs++
		s.o.Estimator.Observe(r.j.Repo, scheduler.Usage{PeakMemory: int64(r.j.PeakMemory), OOM: true, Wall: s.now.Sub(r.start)})
		if r.j.attempts <= s.o.Retries {
			r.j.since = s.now
			s.enqueue(r.j)
		}
		if len(m.runs) == 0 {
			m.idle = s.now
		}
	}
}

// reconcile enforces the orchestrator's invariants: expired machines drain
// while busy and are destroyed otherwise; old environments leave the warm pool.
func (s *sim) reconcile() {
	for _, m := range s.machines {
		if s.now.Before(m.expires) {
			continue
		}
		switch {
		case m.state == scheduler.Running && len(m.runs) > 0:
			m.draining = true
		case m.state != scheduler.Starting:
			s.execute([]scheduler.Action{{Kind: scheduler.Destroy, Machine: m.id, Reason: "expired"}})
		}
	}
	for env, t := range s.recent {
		if s.now.Sub(t) > s.o.RecentTTL {
			delete(s.recent, env)
		}
	}
}

func (s *sim) execute(acts []scheduler.Action) {
	for _, a := range acts {
		s.r.Actions[a.Kind.String()+":"+a.Reason]++
		switch a.Kind {
		case scheduler.Create:
			s.ids++
			m := &machine{id: fmt.Sprintf("m%04d", s.ids), prov: a.Size.Provider, env: a.EnvHash, size: a.Size,
				state: scheduler.Starting, readyAt: s.now.Add(latency(s.o.Boot, a.Size.Provider, 60*time.Second)),
				created: s.now, expires: s.now.Add(s.o.MaxLifetime), warm: map[string]bool{}, up: s.now}
			s.machines = append(s.machines, m)
			s.r.Machines++
			continue
		}
		m := s.find(a.Machine)
		if m == nil {
			continue
		}
		switch a.Kind {
		case scheduler.Start:
			if m.state == scheduler.Stopped {
				m.state = scheduler.Starting
				m.readyAt = s.now.Add(latency(s.o.Start, m.prov, 20*time.Second))
				m.draining = false
				m.up = s.now
			}
		case scheduler.Stop, scheduler.Destroy:
			for _, r := range m.runs { // killed sessions are retried
				r.j.since = s.now
				s.enqueue(r.j)
			}
			m.runs = nil
			m.draining = false
			if m.state == scheduler.Running || m.state == scheduler.Starting {
				if short := s.o.MinBilling - s.now.Sub(m.up); short > 0 {
					s.r.MachineHours += short.Hours()
					s.r.Cost += short.Hours() * m.size.PricePerHour
				}
			}
			if a.Kind == scheduler.Stop {
				m.state = scheduler.Stopped
			} else {
				s.remove(m)
			}
		case scheduler.Drain:
			m.draining = true
		case scheduler.Undrain:
			m.draining = false
		}
	}
}

func (s *sim) remove(m *machine) {
	for i, x := range s.machines {
		if x == m {
			s.machines = append(s.machines[:i], s.machines[i+1:]...)
			return
		}
	}
}

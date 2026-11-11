// Package sim is a discrete-event simulator for scheduling policies. It
// replays a trace of build sessions against a scheduler.Policy, machine
// sizes and provider latencies the way the orchestrator drives the policy
// (Place for every waiting task, Plan while tasks wait and on every
// reconcile tick) and models what the policy cannot see: boot and start
// latency, CPU overcommit slowing tasks down, memory beyond physical
// capacity swapping or being OOM killed, cold build caches, and the cache
// file system of each size's tier stack (anyfs): lazy restores fault in
// a build's working set at the stack's fault time (the rest of the slot
// pages in in the background), and a working set larger than the local
// capacity thrashes (refaults from the tier below lengthen the build).
package sim

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/scheduler"
)

// Options configures a simulation run.
type Options struct {
	Name      string           // label in reports
	Policy    scheduler.Policy // decides placement and pool changes
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
	// MemoryRamp is the share of its peak memory a build uses at its
	// start; it grows linearly to the peak at its end (default 0.3;
	// negative: the peak for the whole run). OOM kills are checked at
	// every event against the current use.
	MemoryRamp float64
	// Writeback saves a snapshot of a build's slot to the provider's store
	// when the build ends (the cache file system writes back
	// continuously), so other machines can restore a warm slot while its
	// machine is busy (default true; set NoWriteback to disable).
	NoWriteback bool
	Retries     int
	// RecentTTL drops environments from the warm pool after this long
	// without use (default 24h, like the orchestrator).
	RecentTTL time.Duration
	// QueueTimeout fails tasks waiting longer (0 = wait forever), like
	// pool.queue_timeout.
	QueueTimeout time.Duration
	// MinBilling is the minimum billed time per create or start (default
	// 60s like EC2; negative = none).
	MinBilling time.Duration
	// Resize lets the policy grow machine disks (provider.DiskResizer).
	Resize bool
	// Refetch is the true refetch factor of the simulated cache file
	// systems: a thrashing build refaults Refetch times its working set
	// beyond the local capacity (0 = scheduler.DefaultRefetch). The policy
	// does not know it: it sees the factor calibrated from the simulated
	// thrash/page_in/reclaim events (events.Refetch).
	Refetch float64
	// Bandwidth is the restore throughput from a provider's own store,
	// CrossBandwidth from another provider's (bytes/s; defaults like the
	// cost model); CrossEgress the price per GB across providers.
	Bandwidth, CrossBandwidth int64
	CrossEgress               float64
	// Down makes providers fail creates with a capacity error during
	// [From, To) seconds since the trace start.
	Down []Outage
	// Debug, when set, is called for every completed job with its wait.
	Debug func(j Job, wait time.Duration)
}

// Outage is a period in which a provider has no capacity.
type Outage struct {
	Provider string
	From, To float64 // seconds since the trace start
}

func (o *Options) defaults() {
	if o.Bandwidth <= 0 {
		o.Bandwidth = scheduler.DefaultBandwidth
	}
	if o.CrossBandwidth <= 0 {
		o.CrossBandwidth = scheduler.DefaultCrossBW
	}
	if o.CrossEgress == 0 {
		o.CrossEgress = scheduler.DefaultCrossEgress
	}
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
	if o.MemoryRamp == 0 {
		o.MemoryRamp = 0.3
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
	Name         string        `json:"name"`
	Jobs         int           `json:"jobs"`
	Completed    int           `json:"completed"`
	Failed       int           `json:"failed"`   // not completed: rejected, timed out or OOM killed too often
	Rejected     int           `json:"rejected"` // larger than every size
	TimedOut     int           `json:"timed_out"`
	Cost         float64       `json:"cost"` // machine-hours * price
	MachineHours float64       `json:"machine_hours"`
	Machines     int           `json:"machines"` // machines created
	MaxRunning   int           `json:"max_running"`
	WaitP50      time.Duration `json:"wait_p50"`
	WaitP95      time.Duration `json:"wait_p95"`
	WaitMax      time.Duration `json:"wait_max"`
	TurnP50      time.Duration `json:"turnaround_p50"` // wait + run time
	TurnP95      time.Duration `json:"turnaround_p95"`
	CPUUtil      float64       `json:"cpu_util"` // used / provisioned core-seconds
	MemUtil      float64       `json:"mem_util"`
	Slowdown     float64       `json:"slowdown"` // mean run time / warm full-speed duration
	OOMs         int           `json:"ooms"`
	Swapped      int           `json:"swapped"` // jobs that ran while their machine swapped
	WarmHits     int           `json:"warm_hits"`
	WarmRate     float64       `json:"warm_rate"`
	Restores     int           `json:"restores"`
	Moves        int           `json:"moves"` // slots saved on one machine and restored on another
	Prunes       int           `json:"prunes"`
	Resizes      int           `json:"resizes"`
	Evictions    int           `json:"evictions"` // slots dropped because a disk was full
	EgressCost   float64       `json:"egress_cost"`
	// Volumes: cache volumes created, attached warm from the detached pool
	// (keep_detached), expired detached; VolumeCost is their storage (also
	// in Cost).
	VolumesCreated int     `json:"volumes_created"`
	VolumeAttaches int     `json:"volume_attaches"`
	VolumesExpired int     `json:"volumes_expired"`
	VolumeCost     float64 `json:"volume_cost"`
	// Flaps counts machines created or started within two minutes after a
	// machine of the same environment was drained or stopped by
	// consolidation (the production flapping case).
	Flaps   int            `json:"flaps"`
	Actions map[string]int `json:"actions"`
	// Created counts machines created per size.
	Created map[string]int `json:"created,omitempty"`
	// HoursBy are the billed machine-hours per size.
	HoursBy map[string]float64 `json:"hours_by,omitempty"`
	// Thrashed counts builds whose working set exceeded their machine's
	// local capacity; ThrashTime is the build time it added. PageInBytes
	// are the bytes faulted in before builds proceeded (lazy restores and
	// refaults).
	Thrashed int `json:"thrashed"`
	// Refetch is the calibrated refetch factor at the end (0: no
	// thrashing observed).
	Refetch     float64       `json:"refetch,omitempty"`
	ThrashTime  time.Duration `json:"thrash_time"`
	PageInBytes int64         `json:"page_in_bytes"`
	// ThrashBias is the policy's predicted run time (Placement.Run) over
	// the actual run time, summed over the builds that thrashed or were
	// placed expecting to (0: none): above 1 the model overestimates
	// thrashing builds (e.g. counts the thrash time twice), below 1 it
	// underestimates them.
	ThrashBias float64 `json:"thrash_bias,omitempty"`
	// IdleHours are the machine-hours of running machines without a
	// session (paid, unused).
	IdleHours float64 `json:"idle_hours"`
	// Cold counts builds that ran without a warm or restored slot;
	// ColdRate is their share of all build starts.
	Cold     int     `json:"cold"`
	ColdRate float64 `json:"cold_rate"`
	// Late counts jobs that waited longer than getting a machine would
	// have taken when they arrived (the fastest start of a stopped machine
	// of their environment, else the fastest create) plus LateMargin: the
	// SLA of "never wait longer than the best start/create".
	Late int `json:"late"`
	// Regret is the hindsight latency regret of each job: its turnaround
	// minus the best alternative at its arrival (start a stopped machine
	// of its environment, warm when it held the slot, or create one and
	// build cold), at least 0. RegretP95 and RegretSum summarise it.
	RegretP95 time.Duration `json:"regret_p95"`
	RegretSum time.Duration `json:"regret_sum"`
}

// LateMargin is the grace beyond the best provisioning latency before a
// wait counts as Late.
const LateMargin = 30 * time.Second

type job struct {
	Job
	n        int
	id       string // task ID ("j<n>")
	arrival  time.Time
	since    time.Time // waiting since (current attempt)
	wait     time.Duration
	attempts int
	done     bool
	swapped  bool
	runFor   time.Duration
	// bound is the fastest provisioning latency at arrival, alt the best
	// alternative turnaround (Late, Regret); set on the first arrival.
	bound, alt time.Duration
	hasAlt     bool
}

type run struct {
	j        *job
	reserve  scheduler.Resources
	expected time.Duration // the policy's expected run time (release forecast)
	warm     bool
	restored bool
	before   int64   // slot bytes at the start
	work     float64 // remaining seconds at full speed
	total    float64 // work at the start
	speed    float64
	seq      int
	start    time.Time
	// ws, capacity and refault describe a thrashing build (refault > 0):
	// its cache events are reported when it ends.
	ws, capacity, refault int64
	// pred is the policy's median run time (Placement.Run); predThrash
	// is set when it expected the build to thrash.
	pred       time.Duration
	predThrash bool
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
	disk     int64            // disk size (0 = unlimited)
	slots    map[string]int64 // repository -> slot bytes
	volume   string           // cache volume (medium volume)
	resized  time.Time        // last disk resize
	ws       int64            // working set of the last build
	thrash   bool             // the last build thrashed
}

// capacity is the bytes m's top tier holds: its disk for a single hot
// medium over the store, the size's local capacity for deeper stacks
// (0: unknown, no thrashing).
func (m *machine) capacity() int64 {
	if len(m.size.Stack.Levels) <= 2 && m.disk > 0 {
		return m.disk
	}
	return m.size.Stack.Capacity
}

// volume is a detached cache volume kept for reuse.
type volume struct {
	id      string
	prov    string
	size    scheduler.Size
	bytes   int64
	slots   map[string]int64
	expires time.Time
}

type sim struct {
	o        Options
	fc       *scheduler.Forecast
	est      map[string]estimate // per repository until the next observation
	snaps    map[string][]scheduler.Snapshot
	calm     map[string]time.Time // env -> last consolidation drain or stop
	cal      *events.Refetch      // refetch calibration from cache events
	vols     []*volume            // detached cache volumes
	volIDs   int
	now      time.Time
	machines []*machine
	queue    []*job
	recent   map[string]time.Time
	seq      int
	ids      int
	r        Report

	cpuUsed, cpuProv, memUsed, memProv float64
	start                              time.Time
}

// Run simulates trace under o.
func Run(trace Trace, o Options) Report {
	o.defaults()
	s := &sim{o: o, now: trace.Start, recent: map[string]time.Time{}, fc: scheduler.NewForecast(),
		snaps: map[string][]scheduler.Snapshot{}, calm: map[string]time.Time{}, cal: events.NewRefetch()}
	s.start = trace.Start
	s.r = Report{Name: o.Name, Jobs: len(trace.Jobs), Actions: map[string]int{}, Created: map[string]int{}, HoursBy: map[string]float64{}}
	jobs := make([]*job, len(trace.Jobs))
	last := 0.0
	for i, tj := range trace.Jobs {
		at := trace.Start.Add(time.Duration(tj.At * float64(time.Second)))
		jobs[i] = &job{Job: tj, n: i, id: "j" + strconv.Itoa(i), arrival: at}
		last = math.Max(last, tj.At)
	}
	end := trace.Start.Add(time.Duration(math.Max(trace.End, last+6*3600) * float64(time.Second)))
	next := 0
	tick := trace.Start
	var waits, turns []time.Duration
	var regrets []time.Duration
	var predSum, actSum time.Duration // ThrashBias
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
				s.cacheEvents(m, r)
				r.j.runFor = s.now.Sub(r.start)
				s.r.Completed++
				waits = append(waits, r.j.wait)
				if s.o.Debug != nil {
					s.o.Debug(r.j.Job, r.j.wait)
				}
				turns = append(turns, s.now.Sub(r.j.arrival))
				if r.j.hasAlt {
					turn := s.now.Sub(r.j.arrival)
					reg := max(turn-r.j.alt, 0)
					regrets = append(regrets, reg)
					s.r.RegretSum += reg
					if r.j.wait > r.j.bound+LateMargin {
						s.r.Late++
					}
				}
				slow += r.j.runFor.Seconds() / math.Max(r.j.Duration, 1e-3)
				if r.j.swapped {
					s.r.Swapped++
				}
				m.warm[r.j.Repo] = true
				after := r.before
				if r.j.SlotBytes > 0 {
					if r.warm || r.restored {
						after += int64(r.j.SlotGrowth)
					} else {
						after = int64(r.j.SlotBytes)
					}
					m.slots[r.j.Repo] = after
					s.fitDisk(m, r.j.Repo)
					if !s.o.NoWriteback {
						s.snapshot(m, r.j.Repo, after)
					}
				}
				u := scheduler.Usage{PeakMemory: int64(r.j.PeakMemory), CPUSeconds: r.j.CPU * r.j.Duration,
					Wall: r.j.runFor, Warm: r.warm, Provider: m.prov, SlotBytesBefore: r.before, SlotBytesAfter: after,
					WorkingSetBytes: int64(r.j.WorkingSet), LocalCapacity: r.capacity, RefaultBytes: r.refault}
				// The cache file system reports the refaulted bytes; the
				// orchestrator turns them into thrash time (RefaultTime).
				u.ThrashSeconds = scheduler.RefaultTime(m.size.Stack, r.refault).Seconds()
				if r.pred > 0 && (r.refault > 0 || r.predThrash) {
					predSum += r.pred
					actSum += r.j.runFor
				}
				if r.restored {
					u.Warm, u.Restored = false, true
				}
				s.observe(r.j.Repo, u)
			}
			m.runs = kept
			if len(m.runs) == 0 && m.idle.IsZero() && m.state == scheduler.Running {
				m.idle = s.now
			}
		}
		s.ooms() // after the completions: a finished build frees its memory
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
			s.alternative(j)
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
	if n := len(regrets); n > 0 {
		sort.Slice(regrets, func(i, j int) bool { return regrets[i] < regrets[j] })
		s.r.RegretP95 = regrets[int(math.Ceil(0.95*float64(n)))-1]
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
		s.r.ColdRate = float64(s.r.Cold) / float64(s.r.Completed+s.r.OOMs)
	}
	s.r.Refetch = s.cal.Factors()[""]
	if actSum > 0 {
		s.r.ThrashBias = float64(predSum) / float64(actSum)
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

// alternative records for a newly arrived job the fastest provisioning
// latency and the best turnaround it could have had by getting a machine
// of its own: start a stopped machine of its environment (warm when the
// machine holds its slot) or create one of a size that fits it on a
// provider with capacity (cold).
func (s *sim) alternative(j *job) {
	if j.hasAlt {
		return
	}
	warmRun := time.Duration(j.Duration * float64(time.Second))
	coldRun := time.Duration(j.Duration * (1 + s.o.ColdPenalty) * float64(time.Second))
	best, bound := time.Duration(-1), time.Duration(-1)
	consider := func(lat, run time.Duration) {
		if bound < 0 || lat < bound {
			bound = lat
		}
		if best < 0 || lat+run < best {
			best = lat + run
		}
	}
	for _, m := range s.machines {
		if m.env != j.Env || m.state != scheduler.Stopped || !m.size.Capacity.Fits(scheduler.Resources{CPU: math.Min(j.CPU, m.size.Capacity.CPU), Memory: int64(j.PeakMemory)}) {
			continue
		}
		run := coldRun
		if m.warm[j.Repo] {
			run = warmRun
		}
		consider(latency(s.o.Start, m.prov, 20*time.Second), run)
	}
	for _, sz := range s.o.Sizes {
		if s.down(sz.Provider) || sz.Capacity.Memory < int64(j.PeakMemory) || sz.Capacity.CPU < j.CPU && !s.largestCPU(sz) {
			continue
		}
		consider(latency(s.o.Boot, sz.Provider, 60*time.Second), coldRun)
	}
	if best < 0 {
		return
	}
	j.alt, j.bound, j.hasAlt = best, bound, true
}

// largestCPU reports whether sz has the most cores of all sizes (a job
// using more cores than any size fits the largest).
func (s *sim) largestCPU(sz scheduler.Size) bool {
	for _, o := range s.o.Sizes {
		if o.Capacity.CPU > sz.Capacity.CPU {
			return false
		}
	}
	return true
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
			mem += s.mem(r)
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
		s.r.HoursBy[m.size.Name] += sec / 3600
		if m.state == scheduler.Running && len(m.runs) == 0 {
			s.r.IdleHours += sec / 3600
		}
		s.cpuProv += c.CPU * sec
		s.memProv += float64(c.Memory) * sec
		var cpu float64
		var mem int64
		for _, r := range m.runs {
			r.work -= r.speed * sec
			cpu += r.j.CPU
			mem += s.mem(r)
		}
		s.cpuUsed += math.Min(cpu, c.CPU) * sec
		s.memUsed += math.Min(float64(mem), float64(c.Memory)) * sec
	}
	s.r.MaxRunning = max(s.r.MaxRunning, running)
	// Volumes cost storage while they exist: attached (also to stopped
	// machines) and detached.
	for _, m := range s.machines {
		if m.volume != "" {
			s.storage(m.size, m.disk, sec)
		}
	}
	for _, v := range s.vols {
		s.storage(v.size, v.bytes, sec)
	}
}

func (s *sim) storage(sz scheduler.Size, bytes int64, sec float64) {
	c := float64(bytes) / 1e9 * scheduler.SlotMedium(sz.Stack).PricePerGBHour() * sec / 3600
	s.r.VolumeCost += c
	s.r.Cost += c
}

type estimate struct {
	fallback scheduler.Resources
	need     scheduler.Resources
	profile  scheduler.Profile
}

// observe records a finished run with the estimator.
func (s *sim) observe(repo string, u scheduler.Usage) {
	s.o.Estimator.Observe(repo, u)
	delete(s.est, repo)
}

func (s *sim) task(j *job) scheduler.Task {
	fallback := scheduler.Resources{CPU: j.CPU, Memory: int64(j.Memory)}
	e, ok := s.est[j.Repo]
	if !ok || e.fallback != fallback {
		e = estimate{fallback, s.o.Estimator.Estimate(j.Repo, fallback), s.o.Estimator.Profile(j.Repo)}
		if s.est == nil {
			s.est = map[string]estimate{}
		}
		s.est[j.Repo] = e
	}
	return scheduler.Task{ID: j.id, EnvHash: j.Env, RepoID: j.Repo, Key: j.Repo,
		Request: e.need, Since: j.since, Profile: e.profile}
}

// pool is the policy's view, built like the orchestrator's poolLocked.
func (s *sim) pool() scheduler.Pool {
	f := scheduler.Pool{Now: s.now, Sizes: s.o.Sizes, Recent: map[string]time.Time{}, Providers: map[string]scheduler.ProviderState{},
		Refetch: s.cal.Factors()}
	for env, t := range s.recent {
		f.Recent[env] = t
	}
	for _, sz := range s.o.Sizes {
		f.Providers[sz.Provider] = scheduler.ProviderState{Boot: latency(s.o.Boot, sz.Provider, 60*time.Second),
			Start: latency(s.o.Start, sz.Provider, 20*time.Second)}
	}
	if len(s.machines) > 0 {
		f.Machines = make([]scheduler.Machine, 0, len(s.machines))
	}
	for _, m := range s.machines {
		sm := scheduler.Machine{ID: m.id, Provider: m.prov, EnvHash: m.env, Size: m.size.Name, State: m.state,
			Draining: m.draining, Capacity: m.size.Capacity, CreatedAt: m.created, ExpiresAt: m.expires}
		if m.state == scheduler.Starting {
			sm.ReadyAt = m.readyAt
		}
		if m.state == scheduler.Running {
			var cpu float64
			var mem int64
			for _, r := range m.runs {
				sm.Reserved = sm.Reserved.Add(r.reserve)
				sm.Releases = append(sm.Releases, scheduler.Release{At: r.start.Add(r.expected), Res: r.reserve, Start: r.start})
				cpu += r.j.CPU
				mem += s.mem(r)
			}
			sm.Used = scheduler.Resources{CPU: math.Min(cpu, m.size.Capacity.CPU), Memory: min(mem, m.size.Capacity.Memory)}
			if cpu > m.size.Capacity.CPU {
				sm.Pressure.CPU = 1 - m.size.Capacity.CPU/cpu
			}
			if mem > m.size.Capacity.Memory {
				sm.Pressure.Memory = 1 - float64(m.size.Capacity.Memory)/float64(mem)
			}
			sm.Sessions = len(m.runs)
			sm.DiskTotal, sm.CanResize = m.disk, s.o.Resize
			sm.Medium, sm.Stack, sm.Volume = m.size.Medium, m.size.Stack, m.volume
			// The cache file system's view, as the worker reports it.
			sm.VM = scheduler.VM{Capacity: m.capacity(), WorkingSet: m.ws}
			sm.VM.Thrashing = m.thrash && sm.VM.Capacity > 0 && m.ws > sm.VM.Capacity
			if m.volume != "" {
				sm.CanResize = m.size.Volume.Max == 0 || m.disk < m.size.Volume.Max
				if cd := scheduler.SlotMedium(m.size.Stack).ResizeCooldown; cd > 0 && !m.resized.IsZero() {
					sm.ResizableAt = m.resized.Add(cd)
				}
			}
			if len(m.slots) > 0 {
				sm.SlotBytes = map[string]int64{}
				for repo, b := range m.slots {
					sm.SlotBytes[repo] = b
					sm.DiskUsed += b
				}
			}
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
	if len(s.queue) > 0 {
		f.Queue = make([]scheduler.Task, 0, len(s.queue))
	}
	for _, j := range s.queue {
		f.Queue = append(f.Queue, s.task(j))
	}
	f.Snapshots = s.snaps
	for _, v := range s.vols {
		f.Volumes = append(f.Volumes, scheduler.Volume{ID: v.id, Provider: v.prov, Bytes: v.bytes, SlotBytes: v.slots, ExpiresAt: v.expires})
	}
	for _, o := range s.o.Down {
		at := s.start.Add(time.Duration(o.To * float64(time.Second)))
		if s.now.Before(at) && !s.now.Before(s.start.Add(time.Duration(o.From*float64(time.Second)))) {
			ps := f.Providers[o.Provider]
			ps.Down = at
			f.Providers[o.Provider] = ps
		}
	}
	return f
}

// fitDisk drops other repositories' slots while m's disk is over full
// (the worker's own eviction when nobody pruned).
func (s *sim) fitDisk(m *machine, keep string) {
	if m.disk <= 0 {
		return
	}
	for {
		var used int64
		victim, vb := "", int64(0)
		for repo, b := range m.slots {
			used += b
			if repo != keep && (b > vb || b == vb && victim != "" && repo < victim) {
				victim, vb = repo, b
			}
		}
		if used <= m.disk || victim == "" {
			return
		}
		delete(m.slots, victim)
		delete(m.warm, victim)
		s.r.Evictions++
	}
}

// down reports whether prov has no capacity now.
func (s *sim) down(prov string) bool {
	for _, o := range s.o.Down {
		if o.Provider == prov && !s.now.Before(s.start.Add(time.Duration(o.From*float64(time.Second)))) &&
			s.now.Before(s.start.Add(time.Duration(o.To*float64(time.Second)))) {
			return true
		}
	}
	return false
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
	f := s.pool()
	s.fc.Observe(f)
	s.fc.Apply(&f)
	acts := s.o.Policy.Plan(f)
	s.fc.Note(f, acts)
	s.execute(acts)
	s.placeQueued()
}

func (s *sim) placeQueued() {
	// The policy's view only changes when a task is assigned, rejected or
	// times out (time stands still here): build it once and again only
	// after such a change, instead of once per waiting task.
	var (
		base  scheduler.Pool
		tasks []scheduler.Task // base.Queue
		at    map[*job]int     // index in tasks
		buf   []scheduler.Task
		ex    = -1 // index of tasks missing in buf (-1: buf not built)
		mbuf  []scheduler.Machine
		dirty = true
	)
	for _, j := range append([]*job(nil), s.queue...) {
		if s.o.QueueTimeout <= 0 && len(s.queue) > 0 && !s.anyFree(j) {
			continue // no machine of its environment has room: Place would wait
		}
		if s.o.QueueTimeout > 0 && s.now.Sub(j.since) > s.o.QueueTimeout {
			s.dequeue(j)
			s.r.TimedOut++
			dirty = true
			continue
		}
		if dirty {
			base = s.pool()
			tasks = base.Queue
			clear(at)
			if at == nil {
				at = make(map[*job]int, len(s.queue))
			}
			for i, q := range s.queue {
				at[q] = i
			}
			dirty, ex = false, -1
		}
		// The queue without j, in order. Successive tasks are in queue
		// order, so buf is updated incrementally from the previous
		// exclusion (the policy must not modify f.Queue). Machines are a
		// fresh copy (small).
		f := base
		i, ok := at[j]
		switch {
		case !ok:
			buf, ex = append(buf[:0], tasks...), -1
		case ex < 0:
			buf = append(append(buf[:0], tasks[:i]...), tasks[i+1:]...)
			ex = i
		case i > ex:
			copy(buf[ex:i], tasks[ex:i])
			ex = i
		case i < ex:
			copy(buf[i:ex], tasks[i+1:ex+1])
			ex = i
		}
		f.Queue = nil
		if len(buf) > 0 {
			f.Queue = buf[:len(buf):len(buf)]
		}
		if base.Machines != nil {
			mbuf = append(mbuf[:0], base.Machines...)
			f.Machines = mbuf[:len(mbuf):len(mbuf)]
		}
		t := s.task(j)
		pl := s.o.Policy.Place(f, t)
		switch pl.Kind {
		case scheduler.Assign:
			if m := s.find(pl.Machine); m != nil && m.state == scheduler.Running {
				s.dequeue(j)
				s.assign(m, j, pl, expected(pl, t, m))
				dirty = true
			}
		case scheduler.Reject:
			s.dequeue(j)
			s.r.Rejected++
			dirty = true
		}
	}
}

// anyFree reports whether a running machine of j's environment could
// take any task (a cheap filter before building the pool).
func (s *sim) anyFree(j *job) bool {
	for _, m := range s.machines {
		if m.env != j.Env || m.state != scheduler.Running || m.draining {
			continue
		}
		var cpu float64
		var mem int64
		for _, r := range m.runs {
			cpu += r.reserve.CPU
			mem += r.reserve.Memory
		}
		if cpu < 2*m.size.Capacity.CPU && mem < m.size.Capacity.Memory {
			return true
		}
	}
	return false
}

func (s *sim) dequeue(j *job) {
	s.queue = slices.DeleteFunc(s.queue, func(o *job) bool { return o == j })
}

// cacheEvents feeds the refetch calibration with what m's cache file
// system reports for the finished build r: when it thrashed, a thrash
// event, its reclaims and refaults, then its task_end (one episode per
// build, as in the sim's thrash model).
func (s *sim) cacheEvents(m *machine, r *run) {
	if r.refault > 0 {
		vm := scheduler.VM{Capacity: r.capacity, WorkingSet: r.ws, Thrashing: true}
		s.cal.Observe(events.Event{Type: events.Thrash, Machine: m.id, Provider: m.prov, VM: &vm, Bytes: r.ws})
		s.cal.Observe(events.Event{Type: events.Reclaim, Machine: m.id, Provider: m.prov, Bytes: r.refault})
		s.cal.Observe(events.Event{Type: events.PageIn, Machine: m.id, Provider: m.prov, Bytes: r.refault})
	}
	s.cal.Observe(events.Event{Type: events.TaskEnd, Machine: m.id, Provider: m.prov})
}

// prune frees bytes on m for repo (other slots, largest first, then a
// third of repo's own) and returns the seconds it takes.
func (s *sim) prune(m *machine, repo string, bytes int64) float64 {
	s.r.Prunes++
	freed := int64(0)
	for freed < bytes {
		victim, vb := "", int64(0)
		for r, b := range m.slots {
			if r != repo && (b > vb || b == vb && victim != "" && r < victim) {
				victim, vb = r, b
			}
		}
		if victim == "" {
			if b := m.slots[repo]; b > 0 {
				m.slots[repo] = b * 2 / 3
			}
			break
		}
		delete(m.slots, victim)
		delete(m.warm, victim)
		freed += vb
	}
	return scheduler.DefaultPrune.Seconds()
}

// resize grows m's disk to target (capped by its volume's maximum) and
// returns the seconds it takes.
func (s *sim) resize(m *machine, target int64) float64 {
	s.r.Resizes++
	if m.volume != "" && m.size.Volume.Max > 0 {
		target = min(target, m.size.Volume.Max)
	}
	m.disk, m.resized = max(m.disk, target), s.now
	return scheduler.DefaultResize.Seconds()
}

// expected is the run time the policy expects for a placement.
func expected(pl scheduler.Placement, t scheduler.Task, m *machine) time.Duration {
	if pl.Expected > 0 {
		return pl.Expected
	}
	if m.warm[t.RepoID] {
		return t.Profile.High(t.Profile.WarmRun())
	}
	return t.Profile.High(t.Profile.ColdRun(m.prov))
}

func (s *sim) assign(m *machine, j *job, pl scheduler.Placement, exp time.Duration) {
	reserve, slot := pl.Grant.Reserve, pl.Slot
	j.wait += s.now.Sub(j.since)
	j.attempts++
	work := j.Duration
	prep := 0.0 // seconds of slot preparation before the build runs
	restored := false
	switch slot.Kind {
	case scheduler.SlotPrune:
		prep = s.prune(m, j.Repo, slot.Bytes)
	case scheduler.SlotResize:
		prep = s.resize(m, slot.Bytes)
	}
	// A restore or move may grow the disk in the same step.
	if slot.Grow > 0 {
		prep += s.resize(m, slot.Grow)
	}
	switch slot.Kind {
	case scheduler.SlotRestore:
		s.r.Restores++
		bw := s.o.Bandwidth
		if slot.Snapshot.Store != m.prov {
			bw = s.o.CrossBandwidth
			egress := float64(slot.Snapshot.Bytes) / 1e9 * s.o.CrossEgress
			s.r.EgressCost += egress
			s.r.Cost += egress
		}
		prep += scheduler.LazyRestore(m.size.Stack, slot.Snapshot.Bytes, int64(j.WorkingSet), bw).Seconds()
		s.r.PageInBytes += min(slot.Snapshot.Bytes, max(int64(j.WorkingSet), 0)) // 0: not modelled
		m.slots[j.Repo] = slot.Snapshot.Bytes
		restored = true
	case scheduler.SlotMove:
		// Save on the source (its provider's store), then restore here.
		s.r.Moves++
		src := s.find(slot.From)
		bw := s.o.Bandwidth
		if src != nil && src.prov != m.prov {
			bw = s.o.CrossBandwidth
			egress := float64(slot.Bytes) / 1e9 * s.o.CrossEgress
			s.r.EgressCost += egress
			s.r.Cost += egress
		}
		prep += float64(slot.Bytes)/float64(s.o.Bandwidth) + scheduler.LazyRestore(m.size.Stack, slot.Bytes, int64(j.WorkingSet), bw).Seconds()
		s.r.PageInBytes += min(slot.Bytes, max(int64(j.WorkingSet), 0))
		m.slots[j.Repo] = slot.Bytes
		restored = true
	}
	warm := m.warm[j.Repo]
	if warm {
		s.r.WarmHits++
	} else if !restored {
		work *= 1 + s.o.ColdPenalty
		s.r.Cold++
	}
	work += prep
	// The working set beyond the local capacity faults in again during
	// the build (the cache file system reports thrashing).
	m.ws, m.thrash = int64(j.WorkingSet), false
	refault := int64(0)
	refetch := j.Refetch
	if refetch <= 0 {
		refetch = s.o.Refetch
	}
	if pen := scheduler.ThrashTime(m.size.Stack, m.capacity(), m.ws, refetch); pen > 0 {
		work += pen.Seconds()
		m.thrash = true
		s.r.Thrashed++
		s.r.ThrashTime += pen
		refault = scheduler.RefetchBytes(m.capacity(), m.ws, refetch)
		s.r.PageInBytes += refault
	}
	s.seq++
	m.runs = append(m.runs, &run{j: j, reserve: reserve, work: work, total: work, seq: s.seq, start: s.now, expected: exp, warm: warm,
		restored: restored, before: m.slots[j.Repo], ws: m.ws, capacity: m.capacity(), refault: refault,
		pred: pl.Run, predThrash: strings.Contains(pl.Reason, "thrashing")})
	m.idle = time.Time{}
	if !s.o.Swap {
		s.oom(m)
	}
}

// mem is the memory r uses now: builds grow towards their peak (linking,
// the last crates), from MemoryRamp of the peak at the start to the peak
// at the end (Options.MemoryRamp < 0: the peak for the whole run).
func (s *sim) mem(r *run) int64 {
	peak := int64(r.j.PeakMemory)
	if s.o.MemoryRamp < 0 || r.total <= 0 {
		return peak
	}
	done := math.Min(math.Max(1-r.work/r.total, 0), 1)
	return int64(float64(peak) * (s.o.MemoryRamp + (1-s.o.MemoryRamp)*done))
}

// ooms checks every machine for memory beyond its physical capacity
// (without swap: OOM kills).
func (s *sim) ooms() {
	if s.o.Swap {
		return
	}
	for _, m := range s.machines {
		s.oom(m)
	}
}

// oom kills the most recently started tasks until memory fits.
func (s *sim) oom(m *machine) {
	for {
		var mem int64
		for _, r := range m.runs {
			mem += s.mem(r)
		}
		if mem <= m.size.Capacity.Memory || len(m.runs) == 0 {
			return
		}
		k := len(m.runs) - 1
		r := m.runs[k]

		m.runs = m.runs[:k]
		s.r.OOMs++
		s.observe(r.j.Repo, scheduler.Usage{PeakMemory: int64(r.j.PeakMemory), OOM: true, Wall: s.now.Sub(r.start)})
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
	s.expireVolumes()
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
			if s.down(a.Size.Provider) {
				s.r.Actions["create-failed:no capacity"]++
				continue
			}
			if t, ok := s.calm[a.EnvHash]; ok && s.now.Sub(t) < 2*time.Minute {
				s.r.Flaps++
			}
			s.ids++
			m := &machine{id: fmt.Sprintf("m%04d", s.ids), prov: a.Size.Provider, env: a.EnvHash, size: a.Size,
				state: scheduler.Starting, readyAt: s.now.Add(latency(s.o.Boot, a.Size.Provider, 60*time.Second)),
				created: s.now, expires: s.now.Add(s.o.MaxLifetime), warm: map[string]bool{}, up: s.now,
				disk: a.Size.Capacity.Disk, slots: map[string]int64{}}
			if a.Size.Medium == medium.Volume {
				s.attachVolume(m, a.Volume)
			}
			s.machines = append(s.machines, m)
			s.r.Machines++
			s.r.Created[a.Size.Name]++
			continue
		}
		m := s.find(a.Machine)
		if m == nil {
			continue
		}
		switch a.Kind {
		case scheduler.Start:
			if t, ok := s.calm[m.env]; ok && s.now.Sub(t) < 2*time.Minute && m.state == scheduler.Stopped {
				s.r.Flaps++
			}
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
			if a.Reason == "consolidated" {
				s.calm[m.env] = s.now
			}
			s.saveSnapshots(m)
			if a.Kind == scheduler.Stop {
				m.state = scheduler.Stopped
			} else {
				s.detachVolume(m)
				s.remove(m)
			}
		case scheduler.Drain:
			m.draining = true
			if a.Reason == "consolidate" {
				s.calm[m.env] = s.now
			}
		case scheduler.Undrain:
			m.draining = false
		}
	}
}

// saveSnapshots models workers saving their slots when they stop: a
// snapshot per warm repository in the provider's store.
func (s *sim) saveSnapshots(m *machine) {
	for repo, b := range m.slots {
		if b <= 0 || !m.warm[repo] {
			continue
		}
		s.snapshot(m, repo, b)
	}
}

// snapshot records the slot of repo on m (bytes) in m's provider's store.
func (s *sim) snapshot(m *machine, repo string, b int64) {
	ss := append([]scheduler.Snapshot{{Repo: repo, Provider: m.prov, Store: m.prov, Bytes: b, At: s.now}}, s.snaps[repo]...)
	if len(ss) > 2 {
		ss = ss[:2]
	}
	s.snaps[repo] = ss
}

// attachVolume gives a new machine of a volume size its cache volume: the
// detached volume the policy chose (warm slots, attach latency) or a
// fresh one.
func (s *sim) attachVolume(m *machine, id string) {
	for i, v := range s.vols {
		if v.id != id || !s.now.Before(v.expires) {
			continue
		}
		s.vols = append(s.vols[:i], s.vols[i+1:]...)
		m.volume, m.disk, m.slots = v.id, v.bytes, v.slots
		for repo, b := range v.slots {
			if b > 0 {
				m.warm[repo] = true
			}
		}
		m.readyAt = m.readyAt.Add(scheduler.SlotMedium(m.size.Stack).Attach)
		s.r.VolumeAttaches++
		return
	}
	s.volIDs++
	s.r.VolumesCreated++
	m.volume = fmt.Sprintf("vol%04d", s.volIDs)
	m.disk = max(m.size.Volume.Min, m.size.Capacity.Disk)
}

// detachVolume keeps a destroyed machine's volume for reuse
// (keep_detached) with its warm slots until its ttl.
func (s *sim) detachVolume(m *machine) {
	if m.volume == "" || !m.size.Volume.KeepDetached || m.size.Volume.TTL <= 0 {
		return
	}
	slots := map[string]int64{}
	for repo, b := range m.slots {
		if m.warm[repo] {
			slots[repo] = b
		}
	}
	s.vols = append(s.vols, &volume{id: m.volume, prov: m.prov, size: m.size, bytes: m.disk, slots: slots, expires: s.now.Add(m.size.Volume.TTL)})
}

// expireVolumes deletes detached volumes past their ttl.
func (s *sim) expireVolumes() {
	kept := s.vols[:0]
	for _, v := range s.vols {
		if s.now.Before(v.expires) {
			kept = append(kept, v)
		} else {
			s.r.VolumesExpired++
		}
	}
	s.vols = kept
}

func (s *sim) remove(m *machine) {
	for i, x := range s.machines {
		if x == m {
			s.machines = append(s.machines[:i], s.machines[i+1:]...)
			return
		}
	}
}

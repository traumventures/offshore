package sim

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/scheduler"
)

// Scenario is an adversarial trace for policy evaluation with the
// simulation options it needs (outages, disks).
type Scenario struct {
	Name  string
	Trace Trace
	// Down are provider outages during the trace.
	Down []Outage
	// Tiers: the jobs carry slots and working sets; run them on sizes
	// with local disks (tier stacks).
	Tiers bool
}

// Scenarios returns the adversarial evaluation traces (deterministic for
// a seed):
//
//   - burst: every 20 minutes a burst of 12 coding agents runs short
//     checks of three repositories of one environment within 20 s, idle
//     in between (machines stop and are available stopped);
//   - mixed: long workspace builds (25-40 min, 8 cores) mixed with a
//     steady stream of short checks (30-90 s, 2 cores) in one environment;
//   - multienv: four environments with two repositories each,
//     interleaved moderate load;
//   - stopped: sparse builds of several repositories (a machine per
//     repository stopped in between), sometimes two at once: a second
//     build waits for the warm busy machine or starts a stopped one;
//   - outage: the burst trace with provider "aws" out of capacity for the
//     second hour;
//   - disk: repositories with 40-80G slots and 25-50G working sets
//     (tier stacks, thrashing, restores);
//   - warmbusy: the machine with a repository's warm slot is busy with
//     a long build of it (8 cores, 12G) when short builds of the same
//     repository arrive (4 cores, 5G), while another machine of the
//     environment (warm for a second repository, short builds every few
//     minutes) is idle: wait for the warm machine, restore the slot from
//     the store on the idle one, start or create one, or build cold.
func Scenarios(seed uint64) []Scenario {
	r := rand.New(rand.NewPCG(seed, seed^0x5eed))
	logn := func(mean float64) float64 { return math.Round(math.Max(mean*math.Exp(0.5*r.NormFloat64()-0.125), 5)) }
	job := func(at float64, repo, env string, dur, cpu float64, mem int64) Job {
		return Job{At: math.Round(at*10) / 10, Repo: repo, Env: env, Duration: dur, CPU: cpu, Memory: Bytes(mem),
			PeakMemory: Bytes(int64(float64(mem)*(0.5+0.3*r.Float64())) &^ (1<<20 - 1))}
	}
	var out []Scenario

	burst := Trace{Start: DefaultStart.Add(9 * time.Hour), End: 4 * 3600}
	for b := 0; b < 12; b++ {
		at := float64(b * 20 * 60)
		for k := 0; k < 12; k++ {
			repo := []string{"api", "core", "cli"}[k%3]
			burst.Jobs = append(burst.Jobs, job(at+20*r.Float64(), repo, "rust", logn(90), 4, 4*gib))
		}
	}
	burst.sort()
	out = append(out, Scenario{Name: "burst", Trace: burst})

	mixed := Trace{Start: DefaultStart.Add(9 * time.Hour), End: 6 * 3600}
	for at := 0.0; at < 6*3600; at += 1800 * (0.5 + r.Float64()) {
		mixed.Jobs = append(mixed.Jobs, job(at, "workspace", "rust", 1500+r.Float64()*900, 8, 12*gib))
	}
	for at := 0.0; at < 6*3600; at += 60 * r.ExpFloat64() {
		repo := []string{"api", "web"}[r.IntN(2)]
		mixed.Jobs = append(mixed.Jobs, job(at, repo, "rust", 30+r.Float64()*60, 2, 2*gib))
	}
	mixed.sort()
	out = append(out, Scenario{Name: "mixed", Trace: mixed})

	multi := Trace{Start: DefaultStart.Add(9 * time.Hour), End: 6 * 3600}
	for e, env := range []string{"rust", "node", "py", "go"} {
		for k := 0; k < 2; k++ {
			repo := env + []string{"-a", "-b"}[k]
			rate := []float64{20, 12, 6, 3}[e] // builds per hour
			for at := 0.0; at < 6*3600; at += 3600 / rate * r.ExpFloat64() {
				multi.Jobs = append(multi.Jobs, job(at, repo, env, logn([]float64{240, 120, 600, 60}[e]), []float64{4, 2, 8, 2}[e], int64([]float64{4, 3, 12, 2}[e])*gib))
			}
		}
	}
	multi.sort()
	out = append(out, Scenario{Name: "multienv", Trace: multi})

	stopped := Trace{Start: DefaultStart.Add(9 * time.Hour), End: 8 * 3600}
	for at := 0.0; at < 8*3600; at += 300 + 900*r.Float64() {
		repo := []string{"api", "core", "cli", "web"}[r.IntN(4)]
		stopped.Jobs = append(stopped.Jobs, job(at, repo, "rust", logn(180), 4, 5*gib))
		if r.Float64() < 0.4 { // a second build shortly after
			repo2 := []string{"api", "core", "cli", "web"}[r.IntN(4)]
			stopped.Jobs = append(stopped.Jobs, job(at+10+60*r.Float64(), repo2, "rust", logn(180), 4, 5*gib))
		}
	}
	stopped.sort()
	out = append(out, Scenario{Name: "stopped", Trace: stopped})

	out = append(out, Scenario{Name: "outage", Trace: burst, Down: []Outage{{Provider: "aws", From: 3600, To: 7200}}})

	disk := Trace{Start: DefaultStart.Add(9 * time.Hour), End: 6 * 3600}
	for k, repo := range []string{"mono", "game", "ml", "api"} {
		slot := []int64{80, 60, 50, 40}[k] * gib
		ws := []int64{50, 35, 30, 25}[k] * gib
		for at := 600 * r.Float64(); at < 6*3600; at += 900 * r.ExpFloat64() {
			j := job(at, repo, "rust", logn(300), 4, 6*gib)
			j.SlotBytes, j.SlotGrowth, j.WorkingSet = Bytes(slot), Bytes(slot/50), Bytes(ws)
			disk.Jobs = append(disk.Jobs, j)
		}
	}
	disk.sort()
	out = append(out, Scenario{Name: "disk", Trace: disk, Tiers: true})

	wb := Trace{Start: DefaultStart.Add(9 * time.Hour), End: 6 * 3600}
	slotted := func(j Job, slot int64) Job {
		j.SlotBytes, j.SlotGrowth, j.WorkingSet = Bytes(slot), Bytes(slot/40), Bytes(slot*3/8)
		return j
	}
	for h := 0.0; h < 6; h++ {
		at := h * 3600
		long := slotted(job(at, "api", "rust", 1500+300*r.Float64(), 8, 12*gib), 8*gib)
		long.Key = "api/workspace"
		wb.Jobs = append(wb.Jobs, long)
		for k := 1; k <= 4; k++ {
			wb.Jobs = append(wb.Jobs, slotted(job(at+float64(k)*240+60*r.Float64(), "api", "rust", logn(120), 4, 5*gib), 8*gib))
		}
	}
	for at := 30.0; at < 6*3600; at += 300 + 300*r.Float64() {
		wb.Jobs = append(wb.Jobs, slotted(job(at, "web", "rust", logn(60), 2, 3*gib), 4*gib))
	}
	wb.sort()
	out = append(out, Scenario{Name: "warmbusy", Trace: wb})
	return out
}

// EvalPool is a pool configuration of the evaluation: sizes, latencies
// and the policy configuration without variant and cost optimization.
type EvalPool struct {
	Name        string
	Sizes       []scheduler.Size
	Boot, Start map[string]time.Duration
	Cfg         scheduler.Config
	History     bool // history estimator (else static)
}

// Run simulates tr on the pool with policy variant and cost
// optimization r.
func (p EvalPool) Run(name string, tr Trace, variant string, r float64, down []Outage, resize bool) Report {
	sp := p.Spec(name, tr, variant, r, down, resize)
	return Run(sp.Trace, sp.Options)
}

// Spec is the simulation Run does, for a batch (RunAll).
func (p EvalPool) Spec(name string, tr Trace, variant string, r float64, down []Outage, resize bool) Spec {
	cfg := p.Cfg
	cfg.Variant, cfg.CostOptimization = variant, r
	var est scheduler.Estimator = scheduler.Static{}
	if p.History {
		est = scheduler.NewHistory("")
	}
	return Spec{Trace: tr, Options: Options{Name: name, Policy: scheduler.Default{Cfg: cfg}, Sizes: p.Sizes, Estimator: est,
		Boot: p.Boot, Start: p.Start, Down: down, Resize: resize, QueueTimeout: cfg.QueueTimeout}}
}

// EvalPools are the pool configurations of the evaluation: the c7a sample
// pool (testdata/pool.toml), the same with local disks (pool-tiers.toml)
// and the production pool of 2026-11-12 (AWS x8 = m8a.2xlarge 8/30G
// $0.58, x16 = c8a.4xlarge 16/30G $0.98, at most 4; OCI disabled;
// idle_stop 1m, history estimator, measured latencies).
func EvalPools() []EvalPool {
	c7a := func(disks bool) []scheduler.Size {
		var out []scheduler.Size
		for i, n := range []string{"c7a.large", "c7a.xlarge", "c7a.2xlarge", "c7a.4xlarge"} {
			cpu := float64(int(2) << i)
			s := scheduler.Size{Provider: "aws", Name: n, Capacity: scheduler.Resources{CPU: cpu, Memory: int64(cpu) * 2 * gib},
				PricePerHour: []float64{0.1027, 0.2053, 0.4106, 0.8211}[i]}
			if disks {
				s.Capacity.Disk = []int64{30, 30, 100, 200}[i] * gib
				s.Medium = medium.Local
				s.Stack = scheduler.StackOf(s.Capacity.Disk, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Store))
			}
			out = append(out, s)
		}
		return out
	}
	base := scheduler.Config{CPUOvercommit: 2, MemoryOvercommit: 1, MaxMachines: 16, IdleStop: 20 * time.Minute,
		ExpiryMargin: 15 * time.Minute, ConsolidateBelow: 0.5, ConsolidateTarget: 0.75, QueueTimeout: 10 * time.Minute}
	lat := func(d time.Duration) map[string]time.Duration { return map[string]time.Duration{"": d} }
	prod := base
	// OCI is disabled (max_machines = 0: free-tier tenancy).
	prod.MaxMachines, prod.IdleStop, prod.ProviderMax = 8, time.Minute, map[string]int{"aws": 4, "oci": 0}
	disk := int64(200) * gib
	return []EvalPool{
		{Name: "c7a", Sizes: c7a(false), Boot: lat(60 * time.Second), Start: lat(20 * time.Second), Cfg: base},
		{Name: "tiers", Sizes: c7a(true), Boot: lat(60 * time.Second), Start: lat(20 * time.Second), Cfg: base, History: true},
		{Name: "prod", Sizes: []scheduler.Size{
			{Provider: "aws", Name: "x8", Capacity: scheduler.Resources{CPU: 8, Memory: 30 * gib, Disk: disk}, PricePerHour: 0.58},
			{Provider: "aws", Name: "x16", Capacity: scheduler.Resources{CPU: 16, Memory: 30 * gib, Disk: disk}, PricePerHour: 0.98},
			{Provider: "oci", Name: "e5-8", Capacity: scheduler.Resources{CPU: 8, Memory: 30 * gib, Disk: disk}, PricePerHour: 0.19},
			{Provider: "oci", Name: "e5-16", Capacity: scheduler.Resources{CPU: 16, Memory: 60 * gib, Disk: disk}, PricePerHour: 0.37},
		}, Boot: map[string]time.Duration{"aws": 100 * time.Second, "oci": 330 * time.Second},
			Start: map[string]time.Duration{"aws": 16 * time.Second, "oci": 60 * time.Second}, Cfg: prod, History: true},
	}
}

package scheduler

import (
	"math"
	"strings"
	"testing"
	"time"
)

// prodSizes is the production pool of November 2026: AWS x4/x8 (fast
// start), OCI e5-8 (cheaper, slow create).
var prodSizes = []Size{
	{Provider: "aws", Name: "x4", Capacity: res(4, 7), PricePerHour: 0.23},
	{Provider: "aws", Name: "x8", Capacity: res(8, 15), PricePerHour: 0.46},
	{Provider: "oci", Name: "e5-8", Capacity: res(8, 30), PricePerHour: 0.19},
}

var prodProviders = map[string]ProviderState{"aws": {Start: 16 * time.Second, Boot: 100 * time.Second},
	"oci": {Start: 60 * time.Second, Boot: 330 * time.Second}}

func prodPolicy(r float64) Default {
	return Default{Cfg: Config{CostOptimization: r, CPUOvercommit: 2, MemoryOvercommit: 1,
		QueueTimeout: 10 * time.Minute, IdleStop: time.Minute}}
}

func started(acts []Action, id string) bool {
	for _, a := range acts {
		if a.Kind == Start && a.Machine == id {
			return true
		}
	}
	return false
}

// Production 2026-11-11 12:47: a cargo fix (need 5 cores, 4G) with a warm
// slot on a busy x4 (4 cores) waited 148s for it while a stopped x8 of
// the same environment starts in 16s, then ran on the x4 with fewer cores
// than it needs. The default policy neither waits for nor places on the
// x4 and starts the x8 right away, at every cost_optimization below 1.
func TestRegressionWaitForSmallWarmMachine(t *testing.T) {
	x4 := busy("x4", res(4, 7), res(5, 4), 1)
	x4.Size, x4.Provider, x4.WarmRepos = "x4", "aws", []string{"r"}
	x4.Releases = []Release{{At: now.Add(150 * time.Second), Start: now.Add(-2 * time.Minute), Res: res(5, 4)}}
	x8 := machine("x8", res(8, 15), Resources{})
	x8.Size, x8.Provider, x8.State = "x8", "aws", Stopped
	tk := task(5, 4)
	tk.Profile = Profile{Warm: 30 * time.Second, Cold: 3 * time.Minute, Spread: 1.2}
	f := Pool{Now: now, Sizes: prodSizes, Machines: []Machine{x4, x8}, Providers: prodProviders}
	for _, r := range []float64{0, 0.2, 0.5, 0.8} {
		p := prodPolicy(r)
		if pl := p.Place(f, tk); pl.Kind != Wait || pl.Machine != "" {
			t.Fatalf("r=%v: want Wait (Plan starts x8), got %+v", r, pl)
		}
		f.Queue = []Task{tk}
		if acts := p.Plan(f); !started(acts, "x8") {
			t.Fatalf("r=%v: want the stopped x8 started, got %+v", r, acts)
		}
		f.Queue = nil
	}
	// The x4 idle: still not placed there (5 cores > 4), the x8 starts.
	idle := x4
	idle.Reserved, idle.Sessions, idle.Releases, idle.IdleSince = Resources{}, 0, nil, now.Add(-time.Minute)
	f.Machines = []Machine{idle, x8}
	if pl := prodPolicy(0.5).Place(f, tk); pl.Kind == Assign {
		t.Fatalf("placed a 5-core task on a 4-core machine: %+v", pl)
	}
}

// Production 2026-11-11 13:12: a warm x8 had just stopped (idle_stop 1m);
// queued builds of its environment did not start it, the policy created
// a cheaper OCI e5-8 instead (slow create, cold slot). A warm stopped
// machine wins against a cheaper cold create at the production setting
// and the default.
func TestStartWarmStoppedOverCheapCreate(t *testing.T) {
	x8 := machine("x8", res(8, 15), Resources{})
	x8.Size, x8.Provider, x8.State, x8.WarmRepos = "x8", "aws", Stopped, []string{"r"}
	tk := task(3, 3)
	tk.Profile = Profile{Warm: 2 * time.Minute, Cold: 6 * time.Minute, Spread: 1.2}
	f := Pool{Now: now, Sizes: prodSizes, Machines: []Machine{x8}, Providers: prodProviders, Queue: []Task{tk}}
	for _, r := range []float64{0.2, 0.5} {
		acts := prodPolicy(r).Plan(f)
		if !started(acts, "x8") || len(acts) != 1 {
			t.Fatalf("r=%v: want only the warm x8 started, got %+v", r, acts)
		}
	}
}

// A task whose request exceeds a machine's physical cores or memory is
// never placed there while some size fits it physically; when nothing
// fits physically, overcommit admits it as before.
func TestPhysicalFit(t *testing.T) {
	small := machine("small", res(4, 8), Resources{})
	small.Size = "x4"
	p := Default{Cfg: Config{CPUOvercommit: 2, MemoryOvercommit: 1}}
	sizes := []Size{{Provider: "p", Name: "x4", Capacity: res(4, 8), PricePerHour: 0.2}, {Provider: "p", Name: "x8", Capacity: res(8, 16), PricePerHour: 0.4}}
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{small}}
	if pl := p.Place(f, task(6, 4)); pl.Kind != Wait {
		t.Fatalf("6 cores on a 4-core machine while x8 exists: %+v", pl)
	}
	f.Queue = []Task{task(6, 4)}
	acts := p.Plan(f)
	if len(acts) != 1 || acts[0].Kind != Create || acts[0].Size.Name != "x8" {
		t.Fatalf("want an x8 created, got %+v", acts)
	}
	f.Sizes, f.Queue = sizes[:1], nil
	if pl := p.Place(f, task(6, 4)); pl.Kind != Assign || pl.Machine != "small" {
		t.Fatalf("nothing fits 6 cores physically: overcommit admits it, got %+v", pl)
	}
	if pl := p.Place(f, task(2, 9)); pl.Kind == Assign {
		t.Fatalf("9G on an 8G machine: %+v", pl)
	}
}

// Waiting for a busy machine is bounded by the fastest start or create
// plus 30s/(1-r); at cost_optimization 1 only the queue timeout bounds it.
func TestHedgeBound(t *testing.T) {
	b := busy("busy", res(8, 16), res(8, 16), 1)
	b.Size, b.WarmRepos = "m", []string{"r"}
	b.Releases = []Release{{At: now.Add(3 * time.Minute), Start: now.Add(-time.Minute), Res: res(8, 16)}}
	idle := machine("idle", res(8, 16), res(4, 8)) // free, cold
	idle.Size = "m"
	tk := task(4, 8)
	tk.Profile = Profile{Warm: time.Minute, Cold: 10 * time.Minute, Spread: 1}
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{b, idle}, Providers: map[string]ProviderState{"p": {Boot: time.Minute}}}
	cfg := Config{CPUOvercommit: 1, MemoryOvercommit: 1, QueueTimeout: 10 * time.Minute}
	for _, c := range []struct {
		r    float64
		wait bool
	}{{0.5, false}, {0.8, true}, {1, true}} { // bound 60s+60s, 60s+150s, none
		cfg.CostOptimization = c.r
		pl := Default{Cfg: cfg}.Place(f, tk)
		if (pl.Kind == Wait) != c.wait {
			t.Fatalf("r=%v: wait for the warm slot in 3m = %v, got %+v", c.r, c.wait, pl)
		}
	}
	// No machine can be created (spend circuit breaker): the bound does
	// not apply.
	cfg.CostOptimization, cfg.MaxSpendPerHour = 0.5, 0.01
	if pl := (Default{Cfg: cfg}).Place(f, tk); pl.Kind != Wait || !strings.Contains(pl.Reason, "warm slot") {
		t.Fatalf("pool full: waiting for the warm slot is allowed, got %+v", pl)
	}
}

// cost_optimization is the single knob: an hour of turnaround is worth
// (1-r)/r hours of the cheapest size.
func TestTimeValue(t *testing.T) {
	f := Pool{Sizes: prodSizes}
	for _, c := range []struct{ r, want float64 }{{0.5, 0.19}, {0.2, 0.76}, {1, 0}} {
		got, ok := Default{Cfg: Config{CostOptimization: c.r}}.timeValue(f, "e")
		if !ok || math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("r=%v: value %v, want %v", c.r, got, c.want)
		}
	}
	if v, _ := (Default{Cfg: Config{CostOptimization: 0}}).timeValue(f, "e"); !math.IsInf(v, 1) {
		t.Fatalf("r=0: want +Inf, got %v", v)
	}
}

// A key that peaked at 12G once is not packed with a 4G fallback before
// it has enough runs; with enough runs one outlier does not set the
// reservation (p90).
func TestHistoryEarlyPeakAndQuantile(t *testing.T) {
	h := NewHistory("")
	fb := res(3, 3)
	h.Observe("k", Usage{PeakMemory: 12 * gib, CPUSeconds: 60, Wall: time.Minute})
	if got := h.Estimate("k", fb); got.Memory != 15*gib || got.CPU != 3 {
		t.Fatalf("after one 12G run: want 15G (peak + 25%%) and the configured cpu, got %+v", got)
	}
	h2 := NewHistory("")
	for i := 0; i < 19; i++ {
		h2.Observe("k", Usage{PeakMemory: 2 * gib, CPUSeconds: 120, Wall: time.Minute})
	}
	h2.Observe("k", Usage{PeakMemory: 12 * gib, CPUSeconds: 120, Wall: time.Minute})
	if got := h2.Estimate("k", fb); got.Memory != 5*gib/2 || got.CPU != 2 {
		t.Fatalf("19 runs at 2G and one at 12G: want 2.5G (p90 + 25%%) and 2 cores, got %+v", got)
	}
}

// A key without enough runs uses its parent key's statistic (another
// package scope of the same subcommand), raised by its own peaks.
func TestHistoryParentKey(t *testing.T) {
	h := NewHistory("")
	for i := 0; i < 5; i++ {
		h.Observe("r/cargo/check", Usage{PeakMemory: 2 * gib, CPUSeconds: 90, Wall: time.Minute})
	}
	e := h.Explain("r/cargo/check?p=api", res(5, 4))
	if e.Key != "r/cargo/check" || e.Requested != "r/cargo/check?p=api" || e.MemorySource != SourceLearnedParent || e.Need != (Resources{CPU: 1.5, Memory: 5 * gib / 2}) {
		t.Fatalf("parent estimate: %+v", e)
	}
	h.Observe("r/cargo/check?p=api", Usage{PeakMemory: 4 * gib, CPUSeconds: 60, Wall: time.Minute})
	if e := h.Explain("r/cargo/check?p=api", res(5, 4)); e.Need.Memory != 5*gib {
		t.Fatalf("own 4G peak raises the parent's 2.5G: %+v", e)
	}
	if got := ParentKeys("r/cargo/build/release?workspace"); len(got) != 2 || got[0] != "r/cargo/build/release" || got[1] != "r/cargo/build" {
		t.Fatalf("parents %v", got)
	}
}

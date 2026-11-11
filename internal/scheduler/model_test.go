package scheduler

import (
	"strings"
	"testing"
	"time"
)

var priced = []Size{{Provider: "p", Name: "m", Capacity: res(8, 16), PricePerHour: 0.4}}

func profiled(warm, cold time.Duration) Task {
	t := task(4, 8)
	t.Profile = Profile{Warm: warm, Cold: cold, Spread: 1}
	return t
}

// A warm slot freeing later than a cold build takes: cost-optimised
// waits for the warm slot (it postpones no machine), time-optimised runs
// cold on the idle machine now.
func TestCostOptimizationTradesWaitAgainstCold(t *testing.T) {
	warm := busy("warm", res(8, 16), res(8, 16), 1)
	warm.Size, warm.WarmRepos = "m", []string{"r"}
	warm.Releases = []Release{{At: now.Add(8 * time.Minute), Res: res(8, 16)}}
	idle := machine("idle", res(8, 16), Resources{})
	idle.Size, idle.IdleSince = "m", now.Add(-time.Minute)
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{warm, idle}}
	tk := profiled(5*time.Minute, 10*time.Minute)

	fast := Default{Cfg: Config{CostOptimization: 0, CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	if fast.Kind != Assign || fast.Machine != "idle" || fast.Reason != "cold" {
		t.Fatalf("cost_optimization=0: want cold on idle now, got %+v", fast)
	}
	cheap := Default{Cfg: Config{CostOptimization: 1, CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	if cheap.Kind != Wait || !strings.Contains(cheap.Reason, "warm slot on warm") {
		t.Fatalf("cost_optimization=1: want to wait for the warm slot, got %+v", cheap)
	}
	// A warm slot freeing soon is both cheaper and faster.
	f.Machines[0].Releases[0].At = now.Add(time.Minute)
	if pl := (Default{Cfg: Config{CostOptimization: 0, CPUOvercommit: 1, MemoryOvercommit: 1}}).Place(f, tk); pl.Kind != Wait {
		t.Fatalf("warm slot in 1m: want wait, got %+v", pl)
	}
}

func TestPlaceFullDiskPrunesOrResizes(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Size, m.DiskTotal, m.DiskUsed = "m", 100*gib, 88*gib
	m.SlotBytes = map[string]int64{"other": 40 * gib}
	tk := profiled(time.Minute, 3*time.Minute)
	tk.Profile.SlotBytes = 20 * gib
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}
	pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m}}, tk)
	if pl.Kind != Assign || pl.Slot.Kind != SlotPrune || pl.Slot.Bytes != 20*gib-(90-88)*gib {
		t.Fatalf("want prune of the missing bytes, got %+v", pl)
	}
	// The worker's reported budget replaces the default 90%.
	mb := m
	mb.DiskBudget = 99 * gib
	if pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{mb}}, tk); pl.Slot.Kind != SlotPrune || pl.Slot.Bytes != 20*gib-(99-88)*gib {
		t.Fatalf("budget 99G: want prune of 9G, got %+v", pl)
	}
	// Nothing to prune: resize when the provider can.
	m.SlotBytes = nil
	if pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m}}, tk); pl.Kind != Wait {
		t.Fatalf("no room and no resize: want wait, got %+v", pl)
	}
	m.CanResize = true
	pl = pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m}}, tk)
	if pl.Kind != Assign || pl.Slot.Kind != SlotResize || pl.Slot.Bytes <= m.DiskTotal {
		t.Fatalf("want resize, got %+v", pl)
	}
}

func TestPlaceRestoresSnapshot(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Size = "m"
	tk := profiled(time.Minute, 10*time.Minute)
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{m},
		Snapshots: map[string][]Snapshot{"r": {{Repo: "r", Provider: "p", Store: "p", Bytes: 2 * gib, At: now.Add(-time.Hour)}}}}
	pl := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	if pl.Kind != Assign || pl.Slot.Kind != SlotRestore || pl.Slot.Snapshot.Bytes != 2*gib {
		t.Fatalf("want restore, got %+v", pl)
	}
	// Across providers the egress makes a huge snapshot not worth it.
	f.Snapshots["r"][0].Store, f.Snapshots["r"][0].Bytes = "other", 400*gib
	if pl := (Default{Cfg: Config{CostOptimization: 1, CPUOvercommit: 1, MemoryOvercommit: 1}}).Place(f, tk); pl.Slot.Kind == SlotRestore {
		t.Fatalf("400G across providers: want cold, got %+v", pl)
	}
}

func TestEgressMostSpecificWins(t *testing.T) {
	p := Default{Cfg: Config{Egress: []Egress{{PricePerGB: 0.05}, {From: "aws", PricePerGB: 0.09}, {From: "aws", To: "oci", PricePerGB: 0.02, Bandwidth: 1 << 20}}}}
	f := Pool{}
	if pr, bw := p.egress(f, "aws", "oci"); pr != 0.02 || bw != 1<<20 {
		t.Fatalf("aws->oci: %v %v", pr, bw)
	}
	if pr, _ := p.egress(f, "aws", "gcp"); pr != 0.09 {
		t.Fatalf("aws->gcp: %v", pr)
	}
	if pr, _ := (Default{}).egress(f, "aws", "aws"); pr != 0 {
		t.Fatalf("same store must be free by default: %v", pr)
	}
	if pr, _ := (Default{}).egress(f, "aws", "oci"); pr != DefaultCrossEgress {
		t.Fatalf("cross store default: %v", pr)
	}
}

func TestPlanSkipsProviderInCooldown(t *testing.T) {
	sizes := []Size{{Provider: "a", Name: "m", Capacity: res(8, 16), PricePerHour: 0.1}, {Provider: "b", Name: "m", Capacity: res(8, 16), PricePerHour: 0.3}}
	f := Pool{Now: now, Sizes: sizes, Queue: queue(task(2, 4)), Providers: map[string]ProviderState{"a": {Down: now.Add(time.Minute)}}}
	for _, v := range []string{VariantDefault, VariantBinpack, VariantClassic} {
		acts := Default{Cfg: Config{Variant: v, MaxMachines: 4}}.Plan(f)
		if len(acts) != 1 || acts[0].Kind != Create || acts[0].Size.Provider != "b" {
			t.Fatalf("%s: want create on b, got %+v", v, acts)
		}
	}
	f.Providers["a"] = ProviderState{Down: now.Add(-time.Second)}
	if acts := (Default{Cfg: Config{MaxMachines: 4}}).Plan(f); acts[0].Size.Provider != "a" {
		t.Fatalf("cooldown over: want a, got %+v", acts)
	}
}

// Waiting for sessions that end within seconds beats booting a machine.
func TestPlanWaitsForReleasesInsteadOfCreating(t *testing.T) {
	m := busy("m", res(8, 16), res(8, 16), 2)
	m.Size = "m"
	m.Releases = []Release{{At: now.Add(10 * time.Second), Res: res(8, 16)}}
	tk := profiled(time.Minute, 2*time.Minute)
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{m}, Queue: []Task{tk}}
	for _, r := range []float64{0, 0.5, 1} {
		if acts := (Default{Cfg: Config{CostOptimization: r, MaxMachines: 4, CPUOvercommit: 1, MemoryOvercommit: 1}}).Plan(f); len(creates(acts)) != 0 {
			t.Fatalf("r=%v: want no create, got %+v", r, acts)
		}
	}
	// Sessions running for an hour more: create.
	f.Machines[0].Releases[0].At = now.Add(time.Hour)
	if acts := (Default{Cfg: Config{CostOptimization: 0.5, MaxMachines: 4, CPUOvercommit: 1, MemoryOvercommit: 1}}).Plan(f); len(creates(acts)) != 1 {
		t.Fatalf("long sessions: want create, got %+v", acts)
	}
}

// The production flapping case: two machines, a short lull, consolidation
// drains one, demand comes back. The forecast keeps both; a recent scale
// up holds consolidation; a drained machine is undrained, not replaced.
func TestConsolidationUsesForecast(t *testing.T) {
	a := busy("a", res(8, 16), res(1, 2), 1)
	b := busy("b", res(8, 16), res(1, 2), 1)
	cfg := Config{MaxMachines: 4, ConsolidateBelow: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{a, b}}
	if got := kinds(Default{Cfg: cfg}.Plan(f))[Drain]; len(got) != 1 {
		t.Fatalf("no forecast: want a drain, got %v", got)
	}
	f.Demand = map[string]Demand{"e": {Peak: res(12, 20)}}
	if got := kinds(Default{Cfg: cfg}.Plan(f))[Drain]; len(got) != 0 {
		t.Fatalf("peak demand needs both: want no drain, got %v", got)
	}
	f.Demand = map[string]Demand{"e": {Peak: res(2, 4), LastScaleUp: now.Add(-time.Minute)}}
	if got := kinds(Default{Cfg: cfg}.Plan(f))[Drain]; len(got) != 0 {
		t.Fatalf("recent scale up: want no drain, got %v", got)
	}
	// A drained machine and queued work: undrain, never create.
	f.Demand = nil
	f.Machines[1].Draining = true
	f.Queue = queue(task(4, 8))
	acts := Default{Cfg: cfg}.Plan(f)
	if k := kinds(acts); len(k[Undrain]) != 1 || len(creates(acts)) != 0 {
		t.Fatalf("want undrain without create, got %+v", acts)
	}
	// An emptied consolidated machine whose demand the other cannot take
	// is undrained instead of stopped.
	f.Queue = nil
	f.Machines[1].Sessions, f.Machines[1].Reserved = 0, Resources{}
	f.Machines[1].IdleSince = now
	f.Demand = map[string]Demand{"e": {Peak: res(12, 20)}}
	acts = Default{Cfg: cfg}.Plan(f)
	if k := kinds(acts); len(k[Stop]) != 0 || len(k[Undrain]) != 1 {
		t.Fatalf("want undrain by forecast, got %+v", acts)
	}
}

func TestForecastPeakAndScaleUp(t *testing.T) {
	fc := NewForecast()
	fc.Window = 5 * time.Minute
	m := machine("m", res(8, 16), res(6, 12))
	fc.Observe(Pool{Now: now, Machines: []Machine{m}})
	m.Reserved = res(1, 1)
	fc.Observe(Pool{Now: now.Add(time.Minute), Machines: []Machine{m}, Queue: queue(task(1, 1))})
	fc.Note(Pool{Now: now.Add(time.Minute)}, []Action{{Kind: Create, EnvHash: "e"}})
	f := Pool{Now: now.Add(2 * time.Minute)}
	fc.Apply(&f)
	if d := f.Demand["e"]; d.Peak != res(6, 12) || !d.LastScaleUp.Equal(now.Add(time.Minute)) {
		t.Fatalf("got %+v", d)
	}
	f = Pool{Now: now.Add(10 * time.Minute)}
	fc.Observe(f)
	fc.Apply(&f)
	if d := f.Demand["e"]; d.Peak != (Resources{}) {
		t.Fatalf("window expired: got %+v", d)
	}
}

func TestProfileLearnsColdWarmGrowth(t *testing.T) {
	h := NewHistory("")
	for _, u := range []Usage{
		{Wall: 10 * time.Minute, Provider: "aws", SlotBytesAfter: 10 * gib},
		{Wall: 2 * time.Minute, Warm: true, SlotBytesBefore: 10 * gib, SlotBytesAfter: 11 * gib},
		{Wall: 3 * time.Minute, Warm: true, SlotBytesBefore: 11 * gib, SlotBytesAfter: 13 * gib},
		{Wall: 150 * time.Second, Restored: true, RestoreSeconds: 30, SlotBytesBefore: 13 * gib, SlotBytesAfter: 14 * gib},
	} {
		h.Observe("k", u)
	}
	p := h.Profile("k")
	if p.Cold != 10*time.Minute || p.ColdBy["aws"] != 10*time.Minute || p.Warm != 2*time.Minute || p.Restore != 30*time.Second {
		t.Fatalf("durations: %+v", p)
	}
	if p.SlotGrowth != gib || p.SlotBytes != 11*gib {
		t.Fatalf("slots: %+v", p)
	}
	if (Profile{}).ColdRun("x") != time.Duration(float64(DefaultRun)*(1+DefaultColdPenalty)) {
		t.Fatal("default cold run")
	}
}

func TestPlaceMovesWarmSlotFromDrainingMachine(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Size = "m"
	src := machine("src", res(8, 16), Resources{})
	src.Size, src.Draining, src.WarmRepos = "m", true, []string{"r"}
	src.SlotBytes = map[string]int64{"r": 2 * gib}
	src.IdleSince = now.Add(-10 * time.Minute)
	tk := profiled(time.Minute, 20*time.Minute)
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}
	pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m, src}}, tk)
	if pl.Kind != Assign || pl.Machine != "m" || pl.Slot.Kind != SlotMove || pl.Slot.From != "src" || pl.Slot.Bytes != 2*gib {
		t.Fatalf("want move from src, got %+v", pl)
	}
	// A snapshot taken after the source went idle holds the same slot:
	// restore it instead of saving again.
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{m, src},
		Snapshots: map[string][]Snapshot{"r": {{Repo: "r", Provider: "p", Store: "p", Bytes: 2 * gib, At: now.Add(-5 * time.Minute)}}}}
	if pl := pol.Place(f, tk); pl.Slot.Kind != SlotRestore {
		t.Fatalf("want restore of the current snapshot, got %+v", pl)
	}
	// A busy source is not a move source (its slot may be in use).
	src.Sessions = 1
	if pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m, src}}, tk); pl.Slot.Kind == SlotMove {
		t.Fatalf("moved from a busy machine: %+v", pl)
	}
}

// Production regression (task 12f7b83a66e0935f): a 5-CPU request waited
// 148 s for the warm slot on a busy x4 (4 CPU, its cargo check overran
// its estimate) instead of starting the stopped x8 of the environment.
// Once the request waited as long as a start takes, the stopped machine
// starts; the request never waits for a machine smaller than itself.
func TestStartStoppedInsteadOfWaitingForOverrunningSession(t *testing.T) {
	x4 := busy("x4", res(4, 7), res(5, 4), 1)
	x4.Size, x4.Provider, x4.WarmRepos = "x4", "aws", []string{"r"}
	x4.Releases = []Release{{At: now.Add(-time.Minute), Start: now.Add(-113 * time.Second), Res: res(5, 4)}}
	x8 := machine("x8", res(8, 15), Resources{})
	x8.Size, x8.Provider, x8.State = "x8", "aws", Stopped
	sizes := []Size{{Provider: "aws", Name: "x8", Capacity: res(8, 15), PricePerHour: 0.46}, {Provider: "aws", Name: "x16", Capacity: res(16, 30), PricePerHour: 0.93}}
	tk := task(5, 4)
	tk.Profile = Profile{Warm: time.Second, Cold: 4 * time.Minute, Spread: 1}
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{x4, x8},
		Providers: map[string]ProviderState{"aws": {Start: 16 * time.Second, Boot: 110 * time.Second}}}
	for _, r := range []float64{0.2, 0.5} {
		p := Default{Cfg: Config{CostOptimization: r, MaxMachines: 8, CPUOvercommit: 2, MemoryOvercommit: 1, QueueTimeout: 10 * time.Minute, IdleStop: 5 * time.Minute}}
		if pl := p.Place(f, tk); pl.Kind != Wait || strings.Contains(pl.Reason, "on x4") {
			t.Fatalf("r=%v: must not wait for the smaller x4: %+v", r, pl)
		}
		tk.Since = now.Add(-time.Minute)
		f.Queue = []Task{tk}
		acts := p.Plan(f)
		started := false
		for _, a := range acts {
			started = started || a.Kind == Start && a.Machine == "x8"
		}
		if !started {
			t.Fatalf("r=%v: waited 60s (start takes 16s), want x8 started, got %+v", r, acts)
		}
	}
}

// The same with a busy machine big enough for the request: its session
// overruns, so the expected release slips on every plan; after waiting
// as long as a start takes the stopped machine starts anyway.
func TestQueueNoLongerThanAStart(t *testing.T) {
	b := busy("busy", res(8, 15), res(8, 8), 1)
	b.Size, b.Provider, b.WarmRepos = "x8", "aws", []string{"r"}
	b.Releases = []Release{{At: now.Add(-time.Minute), Start: now.Add(-113 * time.Second), Res: res(8, 8)}}
	st := machine("stopped", res(8, 15), Resources{})
	st.Size, st.Provider, st.State = "x8", "aws", Stopped
	sizes := []Size{{Provider: "aws", Name: "x8", Capacity: res(8, 15), PricePerHour: 0.46}}
	tk := task(5, 4)
	tk.Profile = Profile{Warm: time.Second, Cold: 4 * time.Minute, Spread: 1}
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{b, st},
		Providers: map[string]ProviderState{"aws": {Start: 16 * time.Second, Boot: 110 * time.Second}}}
	p := Default{Cfg: Config{CostOptimization: 0.5, MaxMachines: 8, CPUOvercommit: 1, MemoryOvercommit: 1, QueueTimeout: 10 * time.Minute, IdleStop: 5 * time.Minute}}
	starts := func(waited time.Duration) bool {
		tk.Since = now.Add(-waited)
		f.Queue = []Task{tk}
		for _, a := range p.Plan(f) {
			if a.Kind == Start {
				return true
			}
		}
		return false
	}
	// The overrunning session is expected to need as long again (113s):
	// longer than a start (16s) plus the margin of r=0.5 (60s).
	if !starts(time.Second) {
		t.Fatal("fresh request, warm slot expected in 113s, a start takes 16s: want the stopped machine started")
	}
	if !starts(30 * time.Second) {
		t.Fatal("waited 30s, a start takes 16s: want the stopped machine started")
	}
	// A warm slot expected within the margin is worth waiting for.
	b.Releases = []Release{{At: now.Add(30 * time.Second), Start: now.Add(-time.Minute), Res: res(8, 8)}}
	f.Machines[0] = b
	if starts(time.Second) {
		t.Fatal("fresh request, warm slot in 30s: waiting is cheaper than a start and a cold build")
	}
}

// providers.X.max_machines = 0 disables a provider: no create there and
// its stopped machines are not started; unset means no own limit.
func TestProviderMaxZeroDisables(t *testing.T) {
	sizes := []Size{{Provider: "oci", Name: "m", Capacity: res(8, 16), PricePerHour: 0.1}, {Provider: "aws", Name: "m", Capacity: res(8, 16), PricePerHour: 0.4}}
	st := machine("st", res(8, 16), Resources{})
	st.Provider, st.State, st.Size = "oci", Stopped, "m"
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{st}, Queue: queue(task(2, 4))}
	for _, r := range []float64{0, 0.5, 1} {
		for _, v := range []string{VariantDefault, VariantBinpack, VariantClassic} {
			cfg := Config{Variant: v, CostOptimization: r, MaxMachines: 4, CPUOvercommit: 1, MemoryOvercommit: 1, ProviderMax: map[string]int{"oci": 0}}
			for _, a := range (Default{Cfg: cfg}).Plan(f) {
				if a.Kind == Start || a.Kind == Create && a.Size.Provider == "oci" {
					t.Fatalf("%s r=%v: disabled provider used: %+v", v, r, a)
				}
			}
		}
	}
	if !(Config{ProviderMax: map[string]int{"oci": 0}}).Disabled("oci") || (Config{ProviderMax: map[string]int{}}).Disabled("oci") {
		t.Fatal("Disabled")
	}
}

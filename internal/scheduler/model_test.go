package scheduler

import (
	"github.com/traumventures/offshore/internal/medium"
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
	warm.Releases = []Release{{At: now.Add(8 * time.Minute), Start: now.Add(-2 * time.Minute), Run: 10 * time.Minute, Res: res(8, 16)}}
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

func TestPlaceFullDiskReclaimsOrResizes(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Size, m.DiskTotal = "m", 100*gib
	m.SlotBytes = map[string]int64{"other": 40 * gib}
	m.Disk = &DiskLedger{Total: 100 * gib, Free: 12 * gib, Pinned: 48 * gib, Reserve: 10 * gib,
		Reclaim: []Reclaimable{{Class: ReclaimSlot, Repo: "other", Bytes: 40 * gib, Cost: 30}}}
	tk := profiled(time.Minute, 3*time.Minute)
	tk.Profile.SlotBytes = 20 * gib
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}
	pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m}}, tk)
	if pl.Kind != Assign || pl.Slot.Reclaim != 20*gib || pl.Slot.Steps() != "reclaim" {
		t.Fatalf("want a reclaim of the slot's bytes, got %+v", pl)
	}
	// Nothing reclaimable: the machine is no option (a new one is).
	m.Disk.Reclaim, m.Disk.Pinned = nil, 88*gib
	if pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m}}, tk); pl.Kind != Wait {
		t.Fatalf("no room and no resize: want wait, got %+v", pl)
	}
	// Resizable: with other capacity (a create of about the latency of a
	// resize) the session does not wait for a disk; without, it grows.
	m.CanResize = true
	if pl := pol.Place(Pool{Now: now, Sizes: priced, Machines: []Machine{m}}, tk); pl.Kind != Wait {
		t.Fatalf("resizable with a size to create: want the create, got %+v", pl)
	}
	pl = pol.Place(Pool{Now: now, Machines: []Machine{m}}, tk)
	if pl.Kind != Assign || pl.Slot.Grow <= m.DiskTotal || pl.Slot.Reclaim != 0 {
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
	for _, v := range []string{""} {
		acts := Default{Cfg: Config{}}.Plan(f)
		if len(acts) != 1 || acts[0].Kind != Create || acts[0].Size.Provider != "b" {
			t.Fatalf("%s: want create on b, got %+v", v, acts)
		}
	}
	f.Providers["a"] = ProviderState{Down: now.Add(-time.Second)}
	if acts := (Default{Cfg: Config{}}).Plan(f); acts[0].Size.Provider != "a" {
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
		if acts := (Default{Cfg: Config{CostOptimization: r, CPUOvercommit: 1, MemoryOvercommit: 1}}).Plan(f); len(creates(acts)) != 0 {
			t.Fatalf("r=%v: want no create, got %+v", r, acts)
		}
	}
	// Sessions running for an hour more: create.
	f.Machines[0].Releases[0].At = now.Add(time.Hour)
	if acts := (Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}}).Plan(f); len(creates(acts)) != 1 {
		t.Fatalf("long sessions: want create, got %+v", acts)
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
		p := Default{Cfg: Config{CostOptimization: r, CPUOvercommit: 2, MemoryOvercommit: 1, QueueTimeout: 10 * time.Minute, IdleStop: 5 * time.Minute}}
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
	p := Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1, QueueTimeout: 10 * time.Minute, IdleStop: 5 * time.Minute}}
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

// providers.X.enabled = false disables a provider: no create there and
// its stopped machines are not started.
func TestDisabledProviderNotUsed(t *testing.T) {
	sizes := []Size{{Provider: "oci", Name: "m", Capacity: res(8, 16), PricePerHour: 0.1}, {Provider: "aws", Name: "m", Capacity: res(8, 16), PricePerHour: 0.4}}
	st := machine("st", res(8, 16), Resources{})
	st.Provider, st.State, st.Size = "oci", Stopped, "m"
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{st}, Queue: queue(task(2, 4))}
	for _, r := range []float64{0, 0.5, 1} {
		for _, v := range []string{""} {
			cfg := Config{CostOptimization: r, CPUOvercommit: 1, MemoryOvercommit: 1, DisabledProviders: map[string]bool{"oci": true}}
			for _, a := range (Default{Cfg: cfg}).Plan(f) {
				if a.Kind == Start || a.Kind == Create && a.Size.Provider == "oci" {
					t.Fatalf("%s r=%v: disabled provider used: %+v", v, r, a)
				}
			}
		}
	}
	if !(Config{DisabledProviders: map[string]bool{"oci": true}}).Disabled("oci") || (Config{}).Disabled("oci") {
		t.Fatal("Disabled")
	}
}

// Production, Nov 11 (task 38554554): a 22 GB head in the machine's own
// store lost to a cold build because the restore was planned as the
// whole working set (reads plus what the run writes, 25 GB) faulted in
// before the build. A lazy restore mounts in about a second and faults
// in what warm runs read of the slot they start on (here 3 GB).
func TestRestoreFaultsReadSetNotSlot(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Size = "m"
	m.Stack = StackOf(0, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Store))
	tk := profiled(30*time.Second, 2*time.Minute)
	tk.Profile.WorkingSet = 25 * gib
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{m},
		Snapshots: map[string][]Snapshot{"r": {{Repo: "r", Provider: "p", Store: "p", Bytes: 22 * gib, At: now.Add(-time.Hour)}}}}
	pol := Default{Cfg: Config{CostOptimization: 0.2, CPUOvercommit: 1, MemoryOvercommit: 1}}
	if pl := pol.Place(f, tk); pl.Slot.Kind == SlotRestore {
		t.Fatalf("without a read set the whole slot faults in: want cold, got %+v", pl)
	}
	tk.Profile.ReadSet = 3 * gib
	if pl := pol.Place(f, tk); pl.Kind != Assign || pl.Slot.Kind != SlotRestore {
		t.Fatalf("read set 3G: want restore, got %+v", pl)
	}
	// Egress is priced on what moves (read set + readahead), not the slot.
	if b := restoreTransfer(22*gib, tk.Profile); b != 3*gib+RestorePrefetch {
		t.Fatalf("transfer %d", b)
	}
}

// A new worktree (no head of its own) is seeded from the newest head of a
// sibling worktree of the same family and environment; heads of other
// environments or families never seed it.
func TestNewWorktreeSeededFromSibling(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Size = "m"
	tk := profiled(30*time.Second, 6*time.Minute)
	tk.RepoID, tk.Family = "new", "fam"
	tk.Profile.ReadSet = gib
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{m},
		Families: map[string][]string{"fam": {"new", "sib", "old"}},
		Snapshots: map[string][]Snapshot{
			"sib":   {{Repo: "sib", Provider: "p", Store: "p", Bytes: 10 * gib, At: now.Add(-time.Hour), Env: "e"}},
			"old":   {{Repo: "old", Provider: "p", Store: "p", Bytes: 10 * gib, At: now.Add(-time.Minute), Env: "env0"}},
			"other": {{Repo: "other", Provider: "p", Store: "p", Bytes: 10 * gib, At: now, Env: "e"}},
		}}
	pol := Default{Cfg: Config{CostOptimization: 0.2, CPUOvercommit: 1, MemoryOvercommit: 1}}
	pl := pol.Place(f, tk)
	if pl.Slot.Kind != SlotRestore || pl.Slot.Repo != "new" || pl.Slot.Snapshot.Repo != "sib" {
		t.Fatalf("want a seed from sib, got %+v", pl)
	}
	tk.Family = ""
	if pl := pol.Place(f, tk); pl.Slot.Kind == SlotRestore {
		t.Fatalf("no family: want cold, got %+v", pl)
	}
}

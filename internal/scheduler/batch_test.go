package scheduler

import (
	"testing"
	"time"
)

// Warm but busy: the request waits for its warm machine when that frees
// soon, and restores its slot on another running machine of the
// environment (copy cost of the second warm copy included) when it frees
// late; also with the remaining-time distribution (experiment "dist").
func TestWarmBusyTradeoff(t *testing.T) {
	for _, e := range []string{"", "dist"} {
		t.Setenv("OFFSHORE_EXPERIMENT", e)
		warmBusy(t)
	}
}

func warmBusy(t *testing.T) {
	place := func(left time.Duration) Placement {
		b := busy("warm", res(8, 16), res(8, 16), 1)
		b.Size, b.WarmRepos = "m", []string{"r"}
		b.Releases = []Release{{At: now.Add(left), Start: now.Add(-10 * time.Minute), Run: 10*time.Minute + left/2, Res: res(8, 16)}}
		idle := machine("idle", res(8, 16), Resources{})
		idle.Size = "m"
		tk := task(4, 8)
		tk.Profile = Profile{Warm: 2 * time.Minute, Cold: 12 * time.Minute, Spread: 1}
		f := Pool{Now: now, Sizes: priced, Machines: []Machine{b, idle}, Providers: map[string]ProviderState{"p": {Boot: time.Minute}},
			Snapshots: map[string][]Snapshot{"r": {{Repo: "r", Provider: "p", Store: "p", Bytes: 2 * gib, At: now.Add(-time.Minute)}}}}
		return Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, CostOptimization: 0.5, QueueTimeout: 10 * time.Minute}}.Place(f, tk)
	}
	if pl := place(20 * time.Second); pl.Kind != Wait {
		t.Fatalf("warm machine frees in 20s: want a wait, got %+v", pl)
	}
	pl := place(30 * time.Minute)
	if pl.Kind != Assign || pl.Machine != "idle" || pl.Slot.Kind != SlotRestore {
		t.Fatalf("warm machine frees in 30m: want a restore on the idle machine, got %+v (slot %+v)", pl, pl.Slot)
	}
}

// Statistical admission (experiment "stat", off by default) packs on peak
// beliefs beyond the reservations but is never stricter than them.
func TestStatisticalAdmission(t *testing.T) {
	t.Setenv("OFFSHORE_EXPERIMENT", "stat")
	p := Default{Cfg: Config{CPUOvercommit: 2, MemoryOvercommit: 1, CostOptimization: 0.5}}
	m := machine("m", res(16, 30), res(4, 20))
	m.Releases = []Release{{At: now.Add(time.Minute), Res: res(4, 20), MemMean: 6 * gib, MemSD: gib}}
	tk := task(2, 12) // 20G + 12G reserved > 30G
	tk.Profile = Profile{PeakMean: 6 * gib, PeakSD: gib, Runs: 10}
	if !p.admits(Pool{Now: now}, m, tk) {
		t.Fatal("peaks 6G±1G + 6G±1G fit 30G: want admitted")
	}
	tk.Profile.PeakMean = 22 * gib
	if p.admits(Pool{Now: now}, m, tk) {
		t.Fatal("peaks 6G + 22G + 2.7σ exceed 30G: want refused")
	}
	small := task(1, 4)
	small.Profile = Profile{PeakMean: 9 * gib, PeakSD: 4 * gib, Runs: 10}
	if !p.admits(Pool{Now: now}, m, small) {
		t.Fatal("the reservation fits: statistics must not refuse it")
	}
}

// prioritize: shortest remaining processing first, aged by waiting, and
// round robin across owners.
func TestPrioritize(t *testing.T) {
	mk := func(id, owner string, run, waited time.Duration) Task {
		return Task{ID: id, Owner: owner, Since: now.Add(-waited), Profile: Profile{Warm: run}}
	}
	got := prioritize(now, []Task{mk("long", "a", 20*time.Minute, 0), mk("short", "a", time.Minute, 0), mk("old", "a", 20*time.Minute, 10*time.Hour)})
	if got[0].ID != "old" || got[1].ID != "short" {
		t.Fatalf("aging and SRPT: %v", ids(got))
	}
	got = prioritize(now, []Task{mk("a1", "a", time.Minute, 0), mk("a2", "a", time.Minute, 0), mk("b1", "b", 2*time.Minute, 0)})
	if got[1].ID != "b1" {
		t.Fatalf("owner round robin: %v", ids(got))
	}
}

func ids(ts []Task) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

// Idle stops decay one machine per environment per StopDecay (experiment
// "decay").
func TestStopDecay(t *testing.T) {
	t.Setenv("OFFSHORE_EXPERIMENT", "decay")
	var ms []Machine
	for _, id := range []string{"a", "b", "c"} {
		m := machine(id, res(4, 8), Resources{})
		m.IdleSince = now.Add(-time.Hour)
		ms = append(ms, m)
	}
	p := Default{Cfg: Config{IdleStop: time.Minute}}
	f := Pool{Now: now, Machines: ms}
	if n := len(kinds(p.Plan(f))[Stop]); n != 1 {
		t.Fatalf("want one stop per plan, got %d", n)
	}
	f.Demand = map[string]Demand{"e": {LastStop: now.Add(-30 * time.Second)}}
	if n := len(kinds(p.Plan(f))[Stop]); n != 0 {
		t.Fatalf("stopped 30s ago: want no stop, got %d", n)
	}
	f.Demand["e"] = Demand{LastStop: now.Add(-2 * time.Minute)}
	if n := len(kinds(p.Plan(f))[Stop]); n != 1 {
		t.Fatalf("stopped 2m ago: want one stop, got %d", n)
	}
}

// The projected-start gate: no machine for a request that starts on the
// capacity there (a known release) sooner than a create would be ready; a
// reservation without a lease estimate is not counted on to end.
func TestProjectedGate(t *testing.T) {
	plan := func(rel []Release) []string {
		b := busy("b", res(8, 16), res(8, 16), 1)
		b.Size, b.Releases = "m", rel
		f := Pool{Now: now, Sizes: priced, Machines: []Machine{b}, Queue: queue(task(4, 8)),
			Providers: map[string]ProviderState{"p": {Boot: 3 * time.Minute}}}
		return creates(Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, CostOptimization: 0.5}}.Plan(f))
	}
	if got := plan([]Release{{At: now.Add(30 * time.Second), Start: now.Add(-5 * time.Minute), Res: res(8, 16)}}); len(got) != 0 {
		t.Fatalf("frees in 30s, create takes 3m: want no create, got %v", got)
	}
	if got := plan([]Release{{At: now.Add(time.Hour), Start: now.Add(-5 * time.Minute), Res: res(8, 16)}}); len(got) != 1 {
		t.Fatalf("frees in 1h: want a create, got %v", got)
	}
	p := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}
	b := busy("b", res(8, 16), res(8, 16), 1)
	f := Pool{Now: now, Machines: []Machine{b}}
	if starts := p.project(f, []*tlMachine{p.tlOf(f, b)}, queue(task(4, 8))); !starts["a"].IsZero() && starts["a"].Sub(now) < time.Hour {
		t.Fatalf("unknown remaining time projected to end at %v", starts["a"])
	}
}

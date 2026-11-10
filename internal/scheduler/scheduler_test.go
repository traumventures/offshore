package scheduler

import (
	"path/filepath"
	"testing"
	"time"
)

const gib = int64(1) << 30

var now = time.Unix(1_800_000_000, 0)

func res(cpu float64, memG int64) Resources { return Resources{CPU: cpu, Memory: memG * gib} }

func machine(id string, cap, reserved Resources) Machine {
	return Machine{ID: id, Provider: "p", EnvHash: "e", State: Running, Capacity: cap, Reserved: reserved,
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(23 * time.Hour)}
}

func task(cpu float64, memG int64) Task {
	return Task{ID: "t", EnvHash: "e", RepoID: "r", Request: res(cpu, memG), Since: now}
}

func TestPlaceOvercommit(t *testing.T) {
	f := Pool{Now: now, Machines: []Machine{machine("m", res(4, 8), res(4, 6))}}
	strict := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}
	if pl := strict.Place(f, task(2, 2)); pl.Kind != Wait {
		t.Fatalf("CPU is full without overcommit, got %+v", pl)
	}
	loose := Default{Cfg: Config{CPUOvercommit: 2, MemoryOvercommit: 1}}
	if pl := loose.Place(f, task(2, 2)); pl.Kind != Assign {
		t.Fatalf("CPU overcommit 2 should admit, got %+v", pl)
	}
	if pl := loose.Place(f, task(2, 3)); pl.Kind != Wait {
		t.Fatalf("memory must not be overcommitted at factor 1, got %+v", pl)
	}
	mem := Default{Cfg: Config{CPUOvercommit: 2, MemoryOvercommit: 1.5, SoftLimit: 1.2, Swap: true}}
	pl := mem.Place(f, task(2, 3))
	if pl.Kind != Assign {
		t.Fatalf("memory overcommit 1.5 should admit, got %+v", pl)
	}
	if pl.Grant.MemoryHigh != (3*gib*12)/10 || pl.Grant.Swap != -1 {
		t.Fatalf("grant limits: %+v", pl.Grant)
	}
}

func TestPlaceRejectsOversized(t *testing.T) {
	f := Pool{Now: now, Sizes: []Size{{Provider: "p", Name: "s", Capacity: res(8, 16)}}}
	if pl := (Default{}).Place(f, task(2, 32)); pl.Kind != Reject {
		t.Fatalf("want reject, got %+v", pl)
	}
}

func TestPlaceSkipsDrainingPressuredExpiring(t *testing.T) {
	cfg := Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMemoryPressure: 0.3, ExpiryMargin: 15 * time.Minute}
	d, p, e, ok := machine("d", res(8, 8), Resources{}), machine("p", res(8, 8), Resources{}), machine("e", res(8, 8), Resources{}), machine("ok", res(8, 8), Resources{})
	d.Draining = true
	p.Pressure.Memory = 0.5
	e.ExpiresAt = now.Add(5 * time.Minute)
	f := Pool{Now: now, Machines: []Machine{d, p, e, ok}}
	if pl := (Default{Cfg: cfg}).Place(f, task(1, 1)); pl.Machine != "ok" {
		t.Fatalf("got %+v", pl)
	}
}

func TestPlanScaleOutPicksSmallestFittingSize(t *testing.T) {
	sizes := []Size{
		{Provider: "p", Name: "large", Capacity: res(32, 64), PricePerHour: 1.6},
		{Provider: "p", Name: "small", Capacity: res(4, 8), PricePerHour: 0.2},
	}
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMachines: 10}}
	one := pol.Plan(Pool{Now: now, Sizes: sizes, Queue: []Task{task(2, 4)}})
	if len(one) != 1 || one[0].Kind != Create || one[0].Size.Name != "small" {
		t.Fatalf("one small task: %+v", one)
	}
	big := pol.Plan(Pool{Now: now, Sizes: sizes, Queue: []Task{task(2, 20)}})
	if len(big) != 1 || big[0].Size.Name != "large" {
		t.Fatalf("task larger than small size: %+v", big)
	}
	// Capacity on its way is not created twice.
	starting := machine("s", res(4, 8), Resources{})
	starting.State = Starting
	none := pol.Plan(Pool{Now: now, Sizes: sizes, Machines: []Machine{starting}, Queue: []Task{task(2, 4)}})
	if len(none) != 0 {
		t.Fatalf("demand covered by a starting machine: %+v", none)
	}
}

func TestPlanStartsStoppedBeforeCreating(t *testing.T) {
	st := machine("st", res(4, 8), Resources{})
	st.State = Stopped
	pol := Default{Cfg: Config{MaxMachines: 3}}
	acts := pol.Plan(Pool{Now: now, Sizes: []Size{{Provider: "p", Name: "s", Capacity: res(4, 8)}}, Machines: []Machine{st}, Queue: []Task{task(2, 4)}})
	if len(acts) != 1 || acts[0].Kind != Start || acts[0].Machine != "st" {
		t.Fatalf("%+v", acts)
	}
}

func TestPlanProviderLimitBursts(t *testing.T) {
	local := machine("l", res(4, 4), res(4, 4))
	local.Provider = "local"
	sizes := []Size{
		{Provider: "local", Name: "box", Capacity: res(4, 4)},
		{Provider: "aws", Name: "c7a", Capacity: res(4, 4), PricePerHour: 0.2},
	}
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMachines: 5, ProviderMax: map[string]int{"local": 1}}}
	acts := pol.Plan(Pool{Now: now, Sizes: sizes, Machines: []Machine{local}, Queue: []Task{task(2, 2)}})
	if len(acts) != 1 || acts[0].Size.Provider != "aws" {
		t.Fatalf("local is at its limit, want burst to aws: %+v", acts)
	}
}

func TestPlanIdleStopKeepsWarmPool(t *testing.T) {
	a, b := machine("a", res(4, 8), Resources{}), machine("b", res(4, 8), Resources{})
	a.IdleSince, b.IdleSince = now.Add(-time.Hour), now.Add(-time.Hour)
	pol := Default{Cfg: Config{MinMachines: 1, MaxMachines: 3, IdleStop: time.Minute}}
	acts := pol.Plan(Pool{Now: now, Machines: []Machine{a, b}, Recent: map[string]time.Time{"e": now}})
	if len(acts) != 1 || acts[0].Kind != Stop {
		t.Fatalf("want exactly one stop: %+v", acts)
	}
}

func TestHistoryEstimator(t *testing.T) {
	h := NewHistory(filepath.Join(t.TempDir(), "h.json"))
	fallback := res(4, 8)
	if got := h.Estimate("k", fallback); got != fallback {
		t.Fatalf("no history: %+v", got)
	}
	for _, peak := range []int64{1, 1, 2} {
		h.Observe("k", Usage{PeakMemory: peak * gib, CPUSeconds: 20, Wall: 10 * time.Second})
	}
	got := h.Estimate("k", fallback)
	if got.Memory != int64(2.5*float64(gib)) || got.CPU != 2 {
		t.Fatalf("p95 + 25%% headroom and mean cores expected, got %+v", got)
	}
	h.Observe("k", Usage{PeakMemory: 8 * gib, OOM: true})
	if got := h.Estimate("k", fallback); got.Memory != 12*gib {
		t.Fatalf("OOM backoff expected, got %+v", got)
	}
	// Persisted.
	h2 := NewHistory(h.Path)
	if got := h2.Estimate("k", fallback); got.Memory != 12*gib {
		t.Fatalf("history not persisted: %+v", got)
	}
}

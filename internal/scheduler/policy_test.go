package scheduler

import (
	"testing"
	"time"
)

var aws = []Size{
	{Provider: "aws", Name: "large", Capacity: res(2, 4), PricePerHour: 0.1},
	{Provider: "aws", Name: "xlarge", Capacity: res(4, 8), PricePerHour: 0.2},
	{Provider: "aws", Name: "4xlarge", Capacity: res(16, 32), PricePerHour: 0.8},
}

func busy(id string, cap, reserved Resources, sessions int) Machine {
	m := machine(id, cap, reserved)
	m.Sessions = sessions
	return m
}

func TestPlaceBestFit(t *testing.T) {
	strict := Config{CPUOvercommit: 1, MemoryOvercommit: 1}
	warm := busy("warm", res(8, 16), res(6, 12), 3)
	warm.WarmRepos = []string{"r"}
	tests := []struct {
		name     string
		cfg      Config
		machines []Machine
		task     Task
		want     string
		reason   string
	}{
		{"tightest fit wins", strict, []Machine{busy("empty", res(8, 16), Resources{}, 0), busy("half", res(8, 16), res(4, 8), 2), busy("full", res(8, 16), res(7, 15), 4)}, task(2, 4), "half", "best fit"},
		{"smaller machine is the tighter fit", strict, []Machine{machine("big", res(16, 32), Resources{}), machine("small", res(4, 8), Resources{})}, task(2, 4), "small", "best fit"},
		{"warm cache beats fit", strict, []Machine{busy("half", res(8, 16), res(4, 8), 2), warm}, task(2, 4), "warm", "warm cache"},
		{"physical room beats overcommit", Config{CPUOvercommit: 2, MemoryOvercommit: 1}, []Machine{busy("over", res(4, 16), res(4, 8), 2), busy("room", res(8, 16), res(2, 4), 1)}, task(2, 4), "room", "best fit"},
		{"single candidate", strict, []Machine{busy("full", res(8, 16), res(8, 16), 4), busy("only", res(8, 16), res(2, 2), 1)}, task(2, 4), "only", "rendezvous"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pl := Default{Cfg: tc.cfg}.Place(Pool{Now: now, Machines: tc.machines}, tc.task)
			if pl.Kind != Assign || pl.Machine != tc.want || pl.Reason != tc.reason {
				t.Fatalf("got %+v, want %s (%s)", pl, tc.want, tc.reason)
			}
		})
	}
}

func TestPlaceTieBreakRendezvous(t *testing.T) {
	ms := []Machine{machine("a", res(8, 16), Resources{}), machine("b", res(8, 16), Resources{}), machine("c", res(8, 16), Resources{})}
	pref := "a"
	for _, m := range ms {
		if Score("r", m.ID) > Score("r", pref) {
			pref = m.ID
		}
	}
	for _, v := range []string{VariantDefault, VariantClassic} {
		pl := Default{Cfg: Config{Variant: v}}.Place(Pool{Now: now, Machines: ms}, task(1, 1))
		if pl.Machine != pref {
			t.Fatalf("%s: equal machines must fall back to rendezvous %s, got %s", v, pref, pl.Machine)
		}
	}
}

func queue(tasks ...Task) []Task {
	for i := range tasks {
		tasks[i].ID = string(rune('a' + i))
		tasks[i].Since = now.Add(time.Duration(i) * time.Second)
	}
	return tasks
}

func creates(acts []Action) []string {
	var out []string
	for _, a := range acts {
		if a.Kind == Create {
			out = append(out, a.Size.Name)
		}
	}
	return out
}

func TestPlanSizing(t *testing.T) {
	cheapLarge := []Size{
		{Provider: "p", Name: "small", Capacity: res(4, 8), PricePerHour: 0.2},
		{Provider: "p", Name: "large", Capacity: res(16, 32), PricePerHour: 0.6}, // cheaper per core
	}
	unpriced := []Size{{Provider: "p", Name: "small", Capacity: res(4, 8)}, {Provider: "p", Name: "large", Capacity: res(16, 32)}}
	tests := []struct {
		name  string
		sizes []Size
		max   int
		queue []Task
		want  []string
	}{
		{"single small task gets cheapest fitting", aws, 10, queue(task(1, 2)), []string{"large"}},
		{"task too big for small sizes", aws, 10, queue(task(2, 20)), []string{"4xlarge"}},
		{"linear prices: equal cost per unit, fewer machines", aws, 10, queue(task(2, 4), task(2, 4), task(2, 4)), []string{"xlarge", "large"}},
		{"queued demand justifies a large machine cheaper per unit", cheapLarge, 10, queue(task(4, 8), task(4, 8), task(4, 8), task(4, 8)), []string{"large"}},
		{"one task on cheap-large prices still gets small", cheapLarge, 10, queue(task(4, 8)), []string{"small"}},
		{"few machine slots left: absorb the most", aws, 1, queue(task(2, 4), task(2, 4), task(2, 4)), []string{"4xlarge"}},
		{"unknown prices: fewest machines", unpriced, 10, queue(task(2, 4), task(2, 4), task(2, 4)), []string{"large"}},
		{"unknown prices, single task: smaller size", unpriced, 10, queue(task(2, 4)), []string{"small"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMachines: tc.max}}
			got := creates(pol.Plan(Pool{Now: now, Sizes: tc.sizes, Queue: tc.queue}))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPlanPacksIntoFragmentedCapacity(t *testing.T) {
	// 2 CPU free on each of two machines does not hold a 4 CPU task: the
	// default variant scales out, classic counts the fragments as capacity.
	ms := []Machine{busy("a", res(4, 8), res(2, 4), 1), busy("b", res(4, 8), res(2, 4), 1)}
	f := Pool{Now: now, Sizes: aws, Machines: ms, Queue: queue(task(4, 4))}
	if got := creates(Default{Cfg: Config{MaxMachines: 5}}.Plan(f)); len(got) != 1 || got[0] != "xlarge" {
		t.Fatalf("default: want one xlarge, got %v", got)
	}
	if got := creates(Default{Cfg: Config{MaxMachines: 5, Variant: VariantClassic}}.Plan(f)); len(got) != 0 {
		t.Fatalf("classic: want no create, got %v", got)
	}
}

func TestPlanRespectsMaxMachinesWhenStarting(t *testing.T) {
	run := busy("run", res(4, 8), res(4, 8), 2)
	st := machine("st", res(4, 8), Resources{})
	st.State = Stopped
	for _, v := range []string{VariantDefault, VariantClassic} {
		acts := Default{Cfg: Config{Variant: v, MaxMachines: 1}}.Plan(Pool{Now: now, Sizes: aws, Machines: []Machine{run, st}, Queue: queue(task(2, 4))})
		if len(acts) != 0 {
			t.Fatalf("%s: pool at max_machines, got %+v", v, acts)
		}
	}
}

func TestPlanFreeSlotForLargerMachine(t *testing.T) {
	small := machine("small", res(2, 4), Resources{})
	small.IdleSince = now
	acts := Default{Cfg: Config{MaxMachines: 1}}.Plan(Pool{Now: now, Sizes: aws, Machines: []Machine{small}, Queue: queue(task(4, 8))})
	if len(acts) != 2 || acts[0].Kind != Stop || acts[0].Machine != "small" || acts[1].Kind != Create || acts[1].Size.Name != "xlarge" {
		t.Fatalf("want stop small + create xlarge, got %+v", acts)
	}
}

func kinds(acts []Action) map[ActionKind][]string {
	out := map[ActionKind][]string{}
	for _, a := range acts {
		out[a.Kind] = append(out[a.Kind], a.Machine)
	}
	return out
}

func TestPlanConsolidation(t *testing.T) {
	cfg := Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMachines: 10, IdleStop: time.Hour, ConsolidateBelow: 0.5}
	light := func() []Machine {
		return []Machine{busy("a", res(8, 16), res(2, 4), 1), busy("b", res(8, 16), res(1, 2), 1), busy("c", res(8, 16), res(3, 6), 2)}
	}
	drained := light()
	drained[1].Draining = true
	empty := light()
	empty[1].Sessions, empty[1].Reserved, empty[1].Draining, empty[1].IdleSince = 0, Resources{}, true, now
	tests := []struct {
		name     string
		cfg      Config
		machines []Machine
		queue    []Task
		recent   map[string]time.Time
		want     map[ActionKind][]string
	}{
		{"drains the least loaded", cfg, light(), nil, nil, map[ActionKind][]string{Drain: {"b"}}},
		{"off by default", Config{MaxMachines: 10}, light(), nil, nil, map[ActionKind][]string{}},
		{"classic never consolidates", Config{Variant: VariantClassic, MaxMachines: 10, ConsolidateBelow: 0.5}, light(), nil, nil, map[ActionKind][]string{}},
		{"busy pool stays", cfg, []Machine{busy("a", res(8, 16), res(6, 12), 3), busy("b", res(8, 16), res(4, 8), 2)}, nil, nil, map[ActionKind][]string{}},
		{"others cannot absorb", Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMachines: 10, ConsolidateBelow: 0.5, ConsolidateTarget: 0.3}, light(), nil, nil, map[ActionKind][]string{}},
		{"one drain at a time", cfg, drained, nil, nil, map[ActionKind][]string{}},
		{"queued work blocks consolidation", cfg, light(), queue(task(16, 64)), nil, nil},
		{"warm pool keeps machines", Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMachines: 10, ConsolidateBelow: 0.5, MinMachines: 3}, light(), nil, map[string]time.Time{"e": now}, map[ActionKind][]string{}},
		{"drained machine stops once empty", cfg, empty, nil, nil, map[ActionKind][]string{Stop: {"b"}}},
		{"drained machine kept for the warm pool", Config{CPUOvercommit: 1, MemoryOvercommit: 1, MaxMachines: 10, ConsolidateBelow: 0.5, MinMachines: 3}, empty, nil, map[string]time.Time{"e": now}, map[ActionKind][]string{Undrain: {"b"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := kinds(Default{Cfg: tc.cfg}.Plan(Pool{Now: now, Sizes: aws, Machines: tc.machines, Queue: tc.queue, Recent: tc.recent}))
			if tc.want == nil { // only check that no drain happened
				if len(got[Drain]) > 0 {
					t.Fatalf("unexpected drain: %v", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, ids := range tc.want {
				if len(got[k]) != len(ids) || got[k][0] != ids[0] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPlanUndrainsBeforeScalingOut(t *testing.T) {
	d := busy("d", res(8, 16), res(2, 4), 1)
	d.Draining = true
	acts := Default{Cfg: Config{MaxMachines: 5, ConsolidateBelow: 0.5}}.Plan(Pool{Now: now, Sizes: aws, Machines: []Machine{d}, Queue: queue(task(2, 4))})
	if len(acts) != 1 || acts[0].Kind != Undrain || acts[0].Machine != "d" {
		t.Fatalf("want undrain, got %+v", acts)
	}
	// An expiring drained machine is not brought back.
	d.ExpiresAt = now.Add(time.Minute)
	acts = Default{Cfg: Config{MaxMachines: 5, ExpiryMargin: 15 * time.Minute}}.Plan(Pool{Now: now, Sizes: aws, Machines: []Machine{d}, Queue: queue(task(2, 4))})
	if len(acts) != 1 || acts[0].Kind != Create {
		t.Fatalf("want create, got %+v", acts)
	}
}

func TestWarmWindowActive(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}
	weekdays := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	day := WarmWindow{Days: weekdays, From: 8 * time.Hour, To: 19 * time.Hour, Machines: 1, Location: berlin}
	night := WarmWindow{Days: []time.Weekday{time.Friday}, From: 22 * time.Hour, To: 6 * time.Hour, Machines: 2}
	mon := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) // Monday
	tests := []struct {
		name string
		w    WarmWindow
		at   time.Time
		want bool
	}{
		{"monday 09:00 Berlin", day, time.Date(2026, 11, 9, 9, 0, 0, 0, berlin), true},
		{"monday 07:30 UTC is 08:30 Berlin", day, mon.Add(7*time.Hour + 30*time.Minute), true},
		{"monday 18:30 UTC is 19:30 Berlin", day, mon.Add(18*time.Hour + 30*time.Minute), false},
		{"to is exclusive", day, time.Date(2026, 11, 9, 19, 0, 0, 0, berlin), false},
		{"saturday", day, time.Date(2026, 11, 14, 10, 0, 0, 0, berlin), false},
		{"friday 23:00 overnight", night, mon.Add(4*24*time.Hour + 23*time.Hour), true},
		{"saturday 05:00 belongs to friday", night, mon.Add(5*24*time.Hour + 5*time.Hour), true},
		{"friday 05:00 belongs to thursday", night, mon.Add(4*24*time.Hour + 5*time.Hour), false},
		{"saturday 07:00", night, mon.Add(5*24*time.Hour + 7*time.Hour), false},
		{"every day", WarmWindow{From: 0, To: 24 * time.Hour}, mon.Add(6*24*time.Hour + 12*time.Hour), true},
	}
	for _, tc := range tests {
		if got := tc.w.Active(tc.at); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestPlanScheduledWarmPool(t *testing.T) {
	mon9 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	cfg := Config{MaxMachines: 3, IdleStop: time.Minute,
		WarmPool: []WarmWindow{{From: 8 * time.Hour, To: 19 * time.Hour, Machines: 2, Days: []time.Weekday{time.Monday}}}}
	st := machine("st", res(4, 8), Resources{})
	st.State = Stopped
	st.ExpiresAt = mon9.Add(time.Hour)
	recent := map[string]time.Time{"e": mon9.Add(-48 * time.Hour)} // older than a day: min_machines would not apply
	acts := Default{Cfg: cfg}.Plan(Pool{Now: mon9, Sizes: aws, Machines: []Machine{st}, Recent: recent})
	got := kinds(acts)
	if len(got[Start]) != 1 || len(got[Create]) != 1 {
		t.Fatalf("want start + create for a pool of 2, got %+v", acts)
	}
	// Outside the window idle machines stop.
	idle := machine("i", res(4, 8), Resources{})
	idle.IdleSince = mon9.Add(-time.Hour)
	evening := mon9.Add(11 * time.Hour)
	idle.ExpiresAt = evening.Add(time.Hour)
	got = kinds(Default{Cfg: cfg}.Plan(Pool{Now: evening, Sizes: aws, Machines: []Machine{idle}, Recent: recent}))
	if len(got[Stop]) != 1 {
		t.Fatalf("want stop after hours, got %v", got)
	}
	// Inside the window the idle machine is kept.
	got = kinds(Default{Cfg: cfg}.Plan(Pool{Now: mon9, Sizes: aws, Machines: []Machine{idle}, Recent: recent}))
	if len(got[Stop]) != 0 || len(got[Create]) != 1 {
		t.Fatalf("want idle machine kept and one more created, got %v", got)
	}
}

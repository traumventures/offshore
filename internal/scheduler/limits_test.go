package scheduler

import (
	"strings"
	"testing"
	"time"
)

// A runaway loop (many idle machines, a small queue) does not grow the
// pool: the guard is ⌈R+β√R⌉ plus one per environment. A request that
// waited longer than a create takes lifts it (capacity exception).
func TestRunawayGuard(t *testing.T) {
	var ms []Machine
	for i := range 6 {
		ms = append(ms, busy(string(rune('a'+i)), res(4, 8), res(4, 8), 1))
	}
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, CostOptimization: 0.5}}
	f := Pool{Now: now, Sizes: aws, Machines: ms, Queue: queue(task(4, 8))}
	st := pol.newPlanState(f)
	// R = 7 machines of load on xlarge-sized means; guard well above 6.
	if st.guard <= 6 {
		t.Fatalf("guard %d: want room above the 6 busy machines for the queued request", st.guard)
	}
	// The same pool, but every machine idle: R is the queued request
	// only, so the guard is small and no request has waited long.
	idle := make([]Machine, len(ms))
	for i, m := range ms {
		m.Reserved, m.Sessions = Resources{}, 0
		idle[i] = m
	}
	f.Machines = idle
	st = pol.newPlanState(f)
	if st.guard >= 6 || pol.room(st, "e") {
		t.Fatalf("guard %d with 6 idle machines and one queued request: want no room", st.guard)
	}
	// A request that waited longer than create + margin lifts the guard.
	f.Queue = queue(task(4, 8))
	f.Queue[0].Since = now.Add(-10 * time.Minute)
	st = pol.newPlanState(f)
	if !pol.room(st, "e") || !strings.Contains(st.exception["e"], "waited 10m0s") {
		t.Fatalf("want a capacity exception: %+v", st.exception)
	}
	if a := pol.note(st, "e", Action{Kind: Create, Reason: "queued demand"}); !strings.Contains(a.Reason, "capacity exception") {
		t.Fatalf("reason %q", a.Reason)
	}
}

// The time-averaged load keeps the guard up through a lull.
func TestGuardUsesLoad(t *testing.T) {
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, CostOptimization: 1}}
	f := Pool{Now: now, Sizes: aws}
	if g := pol.guard(f, pol.newPlanState(f)); g != 0 {
		t.Fatalf("empty pool: guard %d", g)
	}
	f.Machines = []Machine{machine("a", res(4, 8), Resources{})}
	f.Demand = map[string]Demand{"e": {Load: res(40, 80)}}
	// 40 cores of load / mean admissible (22/3 cores) ≈ 5.5 machines,
	// + 0.5·√5.5 → 7, + 1 environment.
	if g := pol.guard(f, pol.newPlanState(f)); g != 8 {
		t.Fatalf("guard %d, want 8", g)
	}
}

func TestForecastLoad(t *testing.T) {
	fc := NewForecast()
	m := machine("a", res(4, 8), res(4, 8))
	fc.Observe(Pool{Now: now.Add(-LoadWindow), Machines: []Machine{m}})
	m.Reserved = Resources{}
	fc.Observe(Pool{Now: now.Add(-LoadWindow / 2), Machines: []Machine{m}}) // demand ended
	f := Pool{Now: now}
	fc.Apply(&f)
	if l := f.Demand["e"].Load; l.CPU != 2 || l.Memory != 4*gib {
		t.Fatalf("half the window at 4 cores/8G: load %+v", l)
	}
}

// Hard limits: a provider's vCPU quota, a disabled provider and the spend
// circuit breaker forbid creates and starts whichever step proposed them.
func TestHardLimits(t *testing.T) {
	sizes := []Size{{Provider: "a", Name: "s", Capacity: res(8, 16), PricePerHour: 0.4}, {Provider: "b", Name: "s", Capacity: res(8, 16), PricePerHour: 0.5}}
	run := busy("run", res(8, 16), res(8, 16), 1)
	run.Provider, run.Size = "a", "s"
	stopped := machine("st", res(8, 16), Resources{})
	stopped.Provider, stopped.Size, stopped.State = "a", "s", Stopped
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{run, stopped}, Queue: queue(task(4, 8))}
	f.Queue[0].Since = now.Add(-time.Hour)
	for _, v := range []string{VariantDefault, VariantOption, VariantBinpack, VariantClassic} {
		for _, c := range []struct {
			name  string
			cfg   Config
			limit map[string]Limit
			want  string // provider of the scale-out, "" = none
		}{
			{"no limit", Config{}, nil, "a"},
			{"vCPU quota of a used up", Config{}, map[string]Limit{"a": {CPU: 8, Source: "quota"}}, "b"},
			{"a disabled", Config{DisabledProviders: map[string]bool{"a": true}}, nil, "b"},
			{"spend breaker", Config{MaxSpendPerHour: 0.6}, nil, ""},
		} {
			c.cfg.Variant, c.cfg.CPUOvercommit, c.cfg.MemoryOvercommit = v, 1, 1
			f.ProviderLimit = c.limit
			got := ""
			for _, a := range (Default{Cfg: c.cfg}).Plan(f) {
				switch a.Kind {
				case Create:
					got = a.Size.Provider
				case Start:
					got = "a"
				}
			}
			if got != c.want {
				t.Fatalf("%s/%s: scale-out on %q, want %q", v, c.name, got, c.want)
			}
		}
	}
}

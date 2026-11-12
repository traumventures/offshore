package scheduler

import (
	"testing"
	"time"
)

// A request that waited long gets one machine, not one per plan tick
// while it boots: the starting machine is booked like any other (prod
// 2026-11-12: 8 creates for 3 requests under the old option policy).
func TestStartingMachineCountsForQueuedRequest(t *testing.T) {
	tk := profiled(2*time.Minute, 3*time.Minute)
	tk.ID, tk.Since = "a", now.Add(-20*time.Minute)
	f := Pool{Now: now, Sizes: priced, Queue: []Task{tk}}
	pol := Default{Cfg: Config{CostOptimization: 0.2, CPUOvercommit: 1, MemoryOvercommit: 1}}
	if got := creates(pol.Plan(f)); len(got) != 1 {
		t.Fatalf("first plan: want one create, got %v", got)
	}
	boot := machine("new", res(8, 16), Resources{})
	boot.State, boot.Size, boot.ReadyAt = Starting, "m", now.Add(30*time.Second)
	f.Machines = []Machine{boot}
	if got := creates(pol.Plan(f)); len(got) != 0 {
		t.Fatalf("machine on its way: want no create, got %v", got)
	}
	// Three queued requests that fit one machine together: one create.
	f.Machines = nil
	f.Queue = queue(profiled(2*time.Minute, 3*time.Minute), profiled(2*time.Minute, 3*time.Minute))
	f.Queue[0].Request, f.Queue[1].Request = res(2, 4), res(2, 4)
	if got := creates(pol.Plan(f)); len(got) != 1 {
		t.Fatalf("two small requests: want one create, got %v", got)
	}
}

// Creates of one solve are merged into a larger size when that scores
// better than a machine per request.
func TestPackCreates(t *testing.T) {
	sizes := []Size{
		{Provider: "p", Name: "s", Capacity: res(4, 8), PricePerHour: 0.2},
		{Provider: "p", Name: "l", Capacity: res(16, 32), PricePerHour: 0.5},
	}
	var ts []Task
	for range 4 {
		tk := profiled(10*time.Minute, 15*time.Minute)
		ts = append(ts, tk)
	}
	f := Pool{Now: now, Sizes: sizes, Queue: queue(ts...)}
	got := creates(Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1, IdleStop: 20 * time.Minute}}.Plan(f))
	if len(got) != 1 || got[0] != "l" {
		t.Fatalf("four 4-core requests: want one 16-core machine, got %v", got)
	}
}

// The rent-or-buy rule: keep while some further keeping is cheaper in
// expectation than the penalty of letting go.
func TestLetGo(t *testing.T) {
	min := time.Minute
	// Requests every 5 minutes, keeping costs 1/h, a start costs 0.5:
	// keep (5 minutes cost 0.083).
	if letGo([]time.Duration{5 * min, 5 * min, 5 * min}, min, 1, 0.5) {
		t.Fatal("frequent requests: want keep")
	}
	// Requests every 5 hours: stop.
	if !letGo([]time.Duration{5 * time.Hour, 5 * time.Hour}, min, 1, 0.5) {
		t.Fatal("rare requests: want stop")
	}
	// Longer idle than every gap seen: stop.
	if !letGo(nil, time.Hour, 1, 0.5) {
		t.Fatal("no remaining gap: want stop")
	}
}

// Idle machines stop by the learned arrivals when idle_stop is unset:
// a machine of an environment used every few minutes stays, one used a
// few times a day stops after the minimum idle time.
func TestLifecycleDerivedIdleStop(t *testing.T) {
	idle := machine("m", res(8, 16), Resources{})
	idle.Size, idle.IdleSince = "m", now.Add(-2*time.Minute)
	pol := Default{Cfg: Config{CostOptimization: 0.2, CPUOvercommit: 1, MemoryOvercommit: 1}}
	stops := func(arr []time.Time) bool {
		f := Pool{Now: now, Sizes: priced, Machines: []Machine{idle}, Arrivals: map[string][]time.Time{"e": arr}}
		for _, a := range pol.Plan(f) {
			if a.Kind == Stop {
				return true
			}
		}
		return false
	}
	var busyEnv, quiet []time.Time
	for i := 30; i > 0; i-- {
		busyEnv = append(busyEnv, now.Add(-time.Duration(2*i-1)*time.Minute))
		quiet = append(quiet, now.Add(-time.Duration(i)*6*time.Hour))
	}
	if stops(busyEnv) {
		t.Fatal("requests every 2 minutes: want the idle machine kept")
	}
	if !stops(quiet) {
		t.Fatal("requests every 6 hours: want the idle machine stopped")
	}
	// A configured idle_stop overrides the rule.
	pol.Cfg.IdleStop = time.Hour
	if stops(quiet) {
		t.Fatal("idle_stop 1h: want the machine kept")
	}
}

// The last machine of a recently used environment is renewed before it
// expires when a start instead of a create is worth it.
func TestRenewExpiringMachine(t *testing.T) {
	sizes := []Size{{Provider: "p", Name: "m", Capacity: Resources{CPU: 8, Memory: 16 * gib, Disk: 20 * gib}, PricePerHour: 0.4}}
	old := machine("old", res(8, 16), Resources{})
	old.Size, old.State, old.ExpiresAt = "m", Stopped, now.Add(5*time.Minute)
	var arr []time.Time
	for i := 20; i > 0; i-- {
		arr = append(arr, now.Add(-time.Duration(30*i-20)*time.Minute))
	}
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{old}, Arrivals: map[string][]time.Time{"e": arr},
		Providers: map[string]ProviderState{"p": {Boot: 2 * time.Minute, Start: 15 * time.Second}}}
	if got := creates((Default{Cfg: Config{CostOptimization: 0.2, CPUOvercommit: 1, MemoryOvercommit: 1, ExpiryMargin: 15 * time.Minute}}).Plan(f)); len(got) != 1 {
		t.Fatalf("want the expiring machine renewed, got %v", got)
	}
	// Not used for two days: no renewal.
	f.Arrivals = map[string][]time.Time{"e": {now.Add(-49 * time.Hour), now.Add(-48 * time.Hour)}}
	if got := creates((Default{Cfg: Config{CostOptimization: 0.2, CPUOvercommit: 1, MemoryOvercommit: 1, ExpiryMargin: 15 * time.Minute}}).Plan(f)); len(got) != 0 {
		t.Fatalf("unused environment: want no renewal, got %v", got)
	}
}

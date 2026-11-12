package scheduler

import (
	"strings"
	"testing"
	"time"
)

// Production 2026-11-12 13:23-13:25: three queued sessions led to eight
// AWS x8 creates. Once a request had waited longer than its hedge bound
// (create + margin) the timeline no longer counted the machines already
// booting for it, so every plan tick created another one ("capacity
// exception (guard 4): 9d53e2be32ea8001 waited 1m49s" at :06, :12, :13).

func overdue(ts ...Task) []Task {
	ts = queue(ts...)
	for i := range ts {
		ts[i].Since = now.Add(-2*time.Minute + time.Duration(i)*time.Second)
		ts[i].Profile = Profile{Warm: 2 * time.Minute, Cold: 5 * time.Minute, Spread: 1.2}
	}
	return ts
}

// booting is the pool's view of a machine being created for env.
func booting(id string, s Size, since time.Duration) Machine {
	return Machine{ID: id, Provider: s.Provider, EnvHash: "e", Size: s.Name, State: Starting, Capacity: s.Capacity,
		CreatedAt: now.Add(-since), ExpiresAt: now.Add(23 * time.Hour), ReadyAt: now.Add(prodProviders[s.Provider].Boot - since)}
}

func countKind(acts []Action, k ActionKind) int {
	n := 0
	for _, a := range acts {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func TestRegressionOverdueRequestGetsOneMachine(t *testing.T) {
	awsOnly := prodSizes[:2]
	x8 := awsOnly[1]
	for _, r := range []float64{0, 0.2, 0.5} {
		p := prodPolicy(r)
		f := Pool{Now: now, Sizes: awsOnly, Providers: prodProviders, Queue: overdue(task(6, 6))}
		if n := countKind(p.Plan(f), Create); n != 1 {
			t.Fatalf("r=%v: overdue request, empty pool: want 1 create, got %d", r, n)
		}
		// The next ticks: its machine boots (just created, half way,
		// overrunning its expected readiness a little).
		for _, since := range []time.Duration{time.Second, 50 * time.Second, 130 * time.Second} {
			f.Machines = []Machine{booting("new", x8, since)}
			if acts := p.Plan(f); countKind(acts, Create)+countKind(acts, Start) != 0 {
				t.Fatalf("r=%v, booting %s: a machine is on its way, want no scale-out, got %+v", r, since, acts)
			}
		}
		// It became ready: considered before any create.
		ready := machine("new", x8.Capacity, Resources{})
		ready.Provider, ready.Size, ready.IdleSince = "aws", "x8", now
		f.Machines = []Machine{ready}
		if acts := p.Plan(f); countKind(acts, Create)+countKind(acts, Start) != 0 {
			t.Fatalf("r=%v: ready machine fits the request: want no scale-out, got %+v", r, acts)
		}
	}
}

// Three overdue requests that fit one machine together get at most one
// machine; with one machine booting that fits them all, none more.
func TestRegressionThreeQueuedOneBooting(t *testing.T) {
	awsOnly := prodSizes[:2]
	p := prodPolicy(0.2)
	f := Pool{Now: now, Sizes: awsOnly, Providers: prodProviders, Queue: overdue(task(2, 3), task(2, 3), task(2, 3))}
	if n := countKind(p.Plan(f), Create); n < 1 || n > 3 {
		t.Fatalf("want 1..3 creates for 3 requests, got %d", n)
	}
	f.Machines = []Machine{booting("new", awsOnly[1], 10*time.Second)}
	if acts := p.Plan(f); countKind(acts, Create) != 0 {
		t.Fatalf("x8 booting fits all three: want no create, got %+v", acts)
	}
}

// Whatever a policy step proposes, queued demand never gets more machines
// than queued requests minus the machines on their way.
func TestCapQueuedDemand(t *testing.T) {
	p := prodPolicy(0.2)
	x8 := prodSizes[1]
	f := Pool{Now: now, Sizes: prodSizes, Providers: prodProviders, Queue: overdue(task(2, 3), task(2, 3))}
	st := p.newPlanState(f)
	for range 4 {
		st.acts = append(st.acts, Action{Kind: Create, EnvHash: "e", Size: x8, Reason: "queued demand"})
	}
	st.acts = append(st.acts, Action{Kind: Create, EnvHash: "e", Size: x8, Reason: "warm pool"})
	p.capQueuedDemand(st)
	if n := countKind(st.acts, Create); n != 3 {
		t.Fatalf("2 requests: want 2 queued-demand creates + the warm pool one, got %+v", st.acts)
	}
	f.Machines = []Machine{booting("b", x8, time.Second)}
	st = p.newPlanState(f)
	st.acts = []Action{{Kind: Create, EnvHash: "e", Size: x8, Reason: "queued demand"}, {Kind: Create, EnvHash: "e", Size: x8, Reason: "queued demand"}}
	p.capQueuedDemand(st)
	if n := countKind(st.acts, Create); n != 1 {
		t.Fatalf("2 requests, 1 machine booting: want 1 create, got %+v", st.acts)
	}
}

// The capacity exception is per overdue request and fires once: a
// machine on its way uses it up.
func TestCapacityExceptionOncePerRequest(t *testing.T) {
	var ms []Machine
	for i := range 6 {
		m := machine(string(rune('a'+i)), res(4, 8), Resources{})
		m.EnvHash = "other"
		ms = append(ms, m)
	}
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1, CostOptimization: 0.5}}
	f := Pool{Now: now, Sizes: aws, Machines: ms, Queue: queue(task(4, 8))}
	f.Queue[0].Since = now.Add(-10 * time.Minute)
	st := pol.newPlanState(f)
	if st.active < st.guard || !pol.room(st, "e") {
		t.Fatalf("want the pool at its guard and an exception: active %d guard %d", st.active, st.guard)
	}
	if a := pol.note(st, "e", Action{Kind: Create, Reason: "queued demand"}); !strings.Contains(a.Reason, "capacity exception") {
		t.Fatalf("reason %q", a.Reason)
	}
	if pol.room(st, "e") {
		t.Fatal("one overdue request: the exception fires once per plan")
	}
	b := machine("b", res(4, 8), Resources{})
	b.State = Starting
	f.Machines = append(f.Machines, b)
	if st = pol.newPlanState(f); pol.room(st, "e") {
		t.Fatalf("its machine boots: no further exception, got %q", st.exception["e"])
	}
}

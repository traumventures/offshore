package learning

import (
	"math"
	"testing"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/scheduler"
)

const gib = 1 << 30

// thrashBuild feeds the events of one thrashing build on machine m: a
// sample over capacity, page-ins and reclaims, then its task_end.
func thrashBuild(r *Refetch, m, prov string, ws, capacity, pageIn, reclaim int64) {
	vm := scheduler.VM{Capacity: capacity, WorkingSet: ws}
	r.Observe(events.Event{Type: events.MachineSample, Machine: m, Provider: prov, VM: &vm})
	r.Observe(events.Event{Type: events.PageIn, Machine: m, Provider: prov, Bytes: pageIn / 2})
	r.Observe(events.Event{Type: events.Reclaim, Machine: m, Provider: prov, Bytes: reclaim})
	r.Observe(events.Event{Type: events.PageIn, Machine: m, Provider: prov, Bytes: pageIn - pageIn/2})
	r.Observe(events.Event{Type: events.TaskEnd, Machine: m, Provider: prov})
}

func TestRefetchCalibration(t *testing.T) {
	r := NewRefetch()
	if r.Factors() != nil {
		t.Fatal("no data: nil (scheduler default)")
	}
	// Page-ins outside a thrash episode (lazy restores) do not count.
	r.Observe(events.Event{Type: events.PageIn, Machine: "a", Provider: "aws", Bytes: 100 * gib})
	r.Observe(events.Event{Type: events.TaskEnd, Machine: "a", Provider: "aws"})
	if r.Factors() != nil {
		t.Fatal("page-ins without thrashing are no refaults")
	}
	// 40G working set on 20G: 80G refaulted = 4 passes over the excess.
	thrashBuild(r, "a", "aws", 40*gib, 20*gib, 80*gib, 90*gib)
	if f := r.Factors(); f["aws"] != 4 || f[""] != 4 || r.Samples("aws") != 1 {
		t.Fatalf("first sample: %v", f)
	}
	// Refaults are bounded by what was reclaimed (first faults of a lazy
	// restore page in without a reclaim).
	thrashBuild(r, "a", "aws", 40*gib, 20*gib, 80*gib, 20*gib)
	if f := r.Factors()["aws"]; math.Abs(f-(4+0.3*(1-4))) > 1e-9 {
		t.Fatalf("decaying average: %v", f)
	}
	// Another provider: its own estimate, the global one mixes.
	thrashBuild(r, "b", "oci", 30*gib, 20*gib, 10*gib, 10*gib)
	f := r.Factors()
	if f["oci"] != 1 || f["aws"] == f[""] {
		t.Fatalf("per provider: %v", f)
	}
}

// A thrash event opens an episode and a sample below capacity closes it;
// a stop closes it too and forgets the machine.
func TestRefetchEpisodeBoundaries(t *testing.T) {
	r := NewRefetch()
	vm := scheduler.VM{Capacity: 10 * gib, WorkingSet: 20 * gib, Thrashing: true}
	r.Observe(events.Event{Type: events.Thrash, Machine: "m", Provider: "aws", VM: &vm, Bytes: vm.WorkingSet})
	r.Observe(events.Event{Type: events.PageIn, Machine: "m", Bytes: 30 * gib})
	r.Observe(events.Event{Type: events.Reclaim, Machine: "m", Bytes: 30 * gib})
	ok := scheduler.VM{Capacity: 10 * gib, WorkingSet: 5 * gib}
	r.Observe(events.Event{Type: events.MachineSample, Machine: "m", VM: &ok})
	if f := r.Factors()["aws"]; f != 3 {
		t.Fatalf("closed by a sample: %v", f)
	}
	r.Observe(events.Event{Type: events.PageIn, Machine: "m", Bytes: 30 * gib}) // not thrashing any more
	r.Observe(events.Event{Type: events.Thrash, Machine: "m", VM: &vm})
	r.Observe(events.Event{Type: events.PageIn, Machine: "m", Bytes: 1000 * gib})
	r.Observe(events.Event{Type: events.Reclaim, Machine: "m", Bytes: 1000 * gib})
	r.Observe(events.Event{Type: events.MachineDestroy, Machine: "m"})
	if f := r.Factors()["aws"]; f != 3+0.3*(maxRefetch-3) || r.Samples("aws") != 2 {
		t.Fatalf("closed by destroy, clamped: %v", f)
	}
	if len(r.episodes) != 0 {
		t.Fatal("destroyed machine forgotten")
	}
}

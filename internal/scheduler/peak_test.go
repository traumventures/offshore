package scheduler

import (
	"testing"
	"time"
)

// Incident 2026-11-12: three clippy runs of new worktrees reserved the
// configured 3G each on a 15G x8s machine and peaked at 4.5-6.5G each;
// the host ran out of memory. The same command had peaked at 6G in a
// sibling worktree: the learned peak must count for admission.
func TestLearnedPeakFromSiblingWorktree(t *testing.T) {
	h := NewHistory("")
	for _, p := range []int64{5, 6, 5} {
		h.Observe("repoA/cargo/clippy?p=app-store", Usage{PeakMemory: p * gib, Wall: time.Minute, CPUSeconds: 60})
	}
	fallback := res(4, 3)
	e := h.Explain("repoB/cargo/clippy?p=app-store", fallback)
	if e.Need.Memory != 3*gib {
		t.Fatalf("estimate need %d", e.Need.Memory)
	}
	if e.LearnedPeak != 6*gib {
		t.Fatalf("learned peak %d, want 6G from the sibling", e.LearnedPeak)
	}
	// Enough own runs: the key's own peak statistic (p90).
	for _, p := range []int64{4, 4, 4} {
		h.Observe("repoB/cargo/clippy?p=app-store", Usage{PeakMemory: p * gib})
	}
	if e := h.Explain("repoB/cargo/clippy?p=app-store", fallback); e.LearnedPeak != 4*gib {
		t.Fatalf("own learned peak %d", e.LearnedPeak)
	}
	// Nothing known anywhere: no learned peak, the fallback stands.
	if e := h.Explain("repoC/cargo/build", fallback); e.LearnedPeak != 0 {
		t.Fatalf("unknown command: %d", e.LearnedPeak)
	}
}

func TestPeakReservation(t *testing.T) {
	c := Config{MemoryOvercommit: 1}
	if r := c.PeakReservation(res(4, 3), 4*gib, 0); r.Memory != 5*gib || r.CPU != 4 {
		t.Fatalf("raised to the peak: %+v", r)
	}
	if r := c.PeakReservation(res(4, 8), 4*gib, 0); r.Memory != 8*gib {
		t.Fatalf("a larger estimate stays: %+v", r)
	}
	if r := c.PeakReservation(res(4, 3), 6*gib, 5*gib); r.Memory != 5*gib {
		t.Fatalf("capped at the hard limit: %+v", r)
	}
	// With overcommit the reservations may exceed capacity by the
	// factor, the peaks never exceed capacity.
	oc := Config{MemoryOvercommit: 1.25}
	if r := oc.PeakReservation(res(4, 3), 4*gib, 0); r.Memory != 5*gib*5/4 {
		t.Fatalf("overcommit: %+v", r)
	}
}

// Sum of learned peaks <= capacity: the third 6G-peak session waits on a
// 15G machine running two.
func TestPeakAdmissionOnX8s(t *testing.T) {
	pol := Default{Cfg: Config{CPUOvercommit: 2, MemoryOvercommit: 1}}
	peak := func() Task {
		tk := task(2, 3)
		tk.Request = pol.Cfg.PeakReservation(tk.Request, 6*gib, 0)
		return tk
	}
	m := machine("x8s", res(8, 15), Resources{})
	for i := 0; i < 2; i++ {
		pl := pol.Place(Pool{Now: now, Machines: []Machine{m}}, peak())
		if pl.Kind != Assign {
			t.Fatalf("session %d: %+v", i, pl)
		}
		m.Reserved = m.Reserved.Add(pl.Grant.Reserve)
	}
	if pl := pol.Place(Pool{Now: now, Machines: []Machine{m}}, peak()); pl.Kind == Assign {
		t.Fatalf("third 6G peak admitted on 15G with %d reserved: %+v", m.Reserved.Memory, pl)
	}
	// The 3G estimates alone would have admitted it (the incident).
	if pl := pol.Place(Pool{Now: now, Machines: []Machine{machine("x8s", res(8, 15), res(4, 6))}}, task(2, 3)); pl.Kind != Assign {
		t.Fatalf("control: %+v", pl)
	}
}

// Production 2026-11-13: the first clippy of 8 crates in a new worktree
// reserved the 3G default on a 16G x8s and peaked at 14.5G; a clippy of
// two of those crates had peaked at 14.1G in the same repository. A run
// over a subset of the packages is a lower bound, and with the peak
// headroom the run needs more than an x8s.
func TestLearnedPeakFromPackageSubset(t *testing.T) {
	h := NewHistory("")
	h.Observe("bb83/cargo/clippy?p=app-api,app-sales", Usage{PeakMemory: 14 * gib})
	h.Observe("bb83/cargo/clippy?p=app-api,app-other", Usage{PeakMemory: 20 * gib}) // not a subset
	h.Observe("bb83/cargo/clippy?workspace", Usage{PeakMemory: 25 * gib})           // a superset
	h.Observe("bb83/cargo/build?p=app-api", Usage{PeakMemory: 22 * gib})            // another command
	key := "bb83/cargo/clippy?p=app-api,app-comms,app-sales,app-store"
	e := h.Explain(key, res(3, 3))
	if e.LearnedPeak != 14*gib {
		t.Fatalf("learned peak %.1fG", float64(e.LearnedPeak)/float64(gib))
	}
	pol := Default{Cfg: Config{CPUOvercommit: 2, MemoryOvercommit: 1}}
	tk := task(3, 3)
	tk.Request = pol.Cfg.PeakReservation(tk.Request, e.LearnedPeak, 0)
	if pl := pol.Place(Pool{Now: now, Machines: []Machine{machine("x8s", Resources{CPU: 8, Memory: 16106127360}, Resources{})}}, tk); pl.Kind == Assign {
		t.Fatalf("placed on an x8s: %+v", pl)
	}
	if pl := pol.Place(Pool{Now: now, Machines: []Machine{machine("x8", res(8, 30), Resources{})}}, tk); pl.Kind != Assign {
		t.Fatalf("x8: %+v", pl)
	}
}

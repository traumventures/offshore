package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// localSizes: a cheap size with a 20G local disk and one with 100G at
// twice the price, both a local disk over the store.
func localSizes() []Size {
	mk := func(name string, disk int64, price float64) Size {
		return Size{Provider: "aws", Name: name, Capacity: Resources{CPU: 8, Memory: 16 * gib, Disk: disk}, PricePerHour: price,
			Medium: medium.Local, Stack: StackOf(disk, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Store))}
	}
	return []Size{mk("small", 20*gib, 0.1), mk("big", 100*gib, 0.2)}
}

func wsTask(ws int64) Task {
	t := task(4, 8)
	t.Profile = Profile{Warm: 10 * time.Minute, Cold: 10 * time.Minute, Spread: 1, WorkingSet: ws}
	return t
}

func TestThrashTime(t *testing.T) {
	st := StackOf(20*gib, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Store))
	if d := ThrashTime(st, 20*gib, 20*gib); d != 0 {
		t.Fatalf("fits: %v", d)
	}
	if d := ThrashTime(st, 0, 100*gib); d != 0 {
		t.Fatalf("unknown capacity: %v", d)
	}
	small, large := ThrashTime(st, 20*gib, 25*gib), ThrashTime(st, 20*gib, 40*gib)
	if small <= 0 || large <= small {
		t.Fatalf("thrash grows with the excess: %v %v", small, large)
	}
	// Two refaults of 20G from the store: transfer at gp3's 125 MiB/s
	// (slower than S3) plus 30 ms per 8 MiB page.
	want := 2 * (time.Duration(float64(20*gib)/float64(125<<20)*float64(time.Second)) + 2560*30*time.Millisecond)
	if large != want {
		t.Fatalf("thrash time %v, want %v", large, want)
	}
}

// A working set larger than the small size's local disk makes new
// machines of the bigger size win unless only cost counts.
func TestPlanAvoidsSizesSmallerThanWorkingSet(t *testing.T) {
	for _, c := range []struct {
		r    float64
		ws   int64
		want string
	}{{0.5, 40 * gib, "big"}, {0, 40 * gib, "big"}, {1, 40 * gib, "small"}, {0.5, 10 * gib, "small"}} {
		pol := Default{Cfg: Config{MaxMachines: 3, CostOptimization: c.r, CPUOvercommit: 1, MemoryOvercommit: 1}}
		got := creates(pol.Plan(Pool{Now: now, Sizes: localSizes(), Queue: queue(wsTask(c.ws))}))
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("r=%v ws=%dG: got %v, want %s", c.r, c.ws>>30, got, c.want)
		}
	}
}

// A machine whose cache file system thrashes is penalised: the task goes
// to the other idle machine.
func TestPlaceAvoidsThrashingMachine(t *testing.T) {
	a := machine("a", res(8, 16), Resources{})
	b := machine("b", res(8, 16), Resources{})
	for _, m := range []*Machine{&a, &b} {
		m.Provider, m.Size, m.IdleSince, m.Stack = "aws", "small", now.Add(-time.Minute), localSizes()[0].Stack
	}
	a.VM = VM{Capacity: 20 * gib, WorkingSet: 30 * gib, Thrashing: true}
	tk := wsTask(0)
	pol := Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}}
	f := Pool{Now: now, Sizes: localSizes(), Machines: []Machine{a, b}}
	for _, id := range []string{"a", "b"} { // independent of the rendezvous order
		f.Machines[0].ID, f.Machines[1].ID = id, map[string]string{"a": "b", "b": "a"}[id]
		pl := pol.Place(f, tk)
		if pl.Kind != Assign || pl.Machine != f.Machines[1].ID {
			t.Fatalf("want the machine that does not thrash, got %+v", pl)
		}
	}
}

// The only machine thrashes for a task's working set: balanced grows its
// disk first, cost-optimised accepts the thrashing (the extra disk until
// the machine expires costs more than the thrashing build).
func TestPlaceResizesDiskForWorkingSet(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Provider, m.Size, m.Stack, m.DiskTotal, m.CanResize = "aws", "small", localSizes()[0].Stack, 20*gib, true
	m.ExpiresAt = now.Add(6 * time.Hour)
	f := Pool{Now: now, Sizes: localSizes(), Machines: []Machine{m}}
	tk := wsTask(40 * gib)
	fast := Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	if fast.Kind != Assign || fast.Slot.Kind != SlotResize || fast.Slot.Bytes < 20*gib+(50-20)*gib || !strings.Contains(fast.Reason, "working set") {
		t.Fatalf("want a resize for the working set, got %+v", fast)
	}
	cheap := Default{Cfg: Config{CostOptimization: 1, CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	if cheap.Kind != Assign || cheap.Slot.Kind != SlotNone || !strings.Contains(cheap.Reason, "thrashing") {
		t.Fatalf("want to thrash cheaply, got %+v", cheap)
	}
	// After the resize the working set fits: no penalty, no resize.
	m.DiskTotal, m.VM.Capacity = 60*gib, 60*gib
	f.Machines = []Machine{m}
	if pl := (Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}}).Place(f, tk); pl.Slot.Kind != SlotNone || strings.Contains(pl.Reason, "thrash") {
		t.Fatalf("fits now: %+v", pl)
	}
}

// Moving a slot from a machine with a cache file system uploads only its
// dirty pages.
func TestSaveTimeUploadsDirtyPages(t *testing.T) {
	src := Machine{Stack: localSizes()[0].Stack}
	full := saveTime(src, 10*gib, 200<<20)
	src.VM = VM{Capacity: 20 * gib, Dirty: gib}
	if d := saveTime(src, 10*gib, 200<<20); d >= full/5 {
		t.Fatalf("dirty only: %v vs full %v", d, full)
	}
}

func TestProfileLearnsWorkingSet(t *testing.T) {
	h := NewHistory("")
	for _, ws := range []int64{10, 12, 11, 30, 13} {
		h.Observe("k", Usage{Wall: time.Minute, WorkingSetBytes: ws * gib})
	}
	h.Observe("k", Usage{Wall: time.Minute}) // unknown: ignored
	if got := h.Profile("k").WorkingSet; got != 30*gib {
		t.Fatalf("working set p80 %dG", got>>30)
	}
}

func TestWithTop(t *testing.T) {
	st := StackOf(0, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Store))
	nv := WithTop(st, medium.Default("aws-ec2", medium.LocalNVMe), 237e9)
	if nv.Capacity != 237e9 || nv.Top().Throughput != medium.Default("aws-ec2", medium.LocalNVMe).Throughput || len(nv.Levels) != 2 || st.Top().Throughput == nv.Top().Throughput {
		t.Fatalf("%+v", nv)
	}
	if SlotMedium(StackOf(0, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Volume), medium.Default("aws-ec2", medium.Store))).Medium != medium.Volume {
		t.Fatal("slots persist on the volume")
	}
}

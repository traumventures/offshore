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
	if d := ThrashTime(st, 20*gib, 20*gib, 0); d != 0 {
		t.Fatalf("fits: %v", d)
	}
	if d := ThrashTime(st, 0, 100*gib, 0); d != 0 {
		t.Fatalf("unknown capacity: %v", d)
	}
	small, large := ThrashTime(st, 20*gib, 25*gib, 0), ThrashTime(st, 20*gib, 40*gib, 0)
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
		pol := Default{Cfg: Config{CostOptimization: c.r, CPUOvercommit: 1, MemoryOvercommit: 1}}
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

func TestRefetchOf(t *testing.T) {
	if r := (Pool{}).RefetchOf("aws"); r != DefaultRefetch {
		t.Fatalf("no calibration: %v", r)
	}
	f := Pool{Refetch: map[string]float64{"aws": 5, "": 3}}
	if f.RefetchOf("aws") != 5 || f.RefetchOf("oci") != 3 {
		t.Fatalf("per provider, then global: %v %v", f.RefetchOf("aws"), f.RefetchOf("oci"))
	}
	st := localSizes()[0].Stack
	if a, b := ThrashTime(st, 20*gib, 40*gib, 4), ThrashTime(st, 20*gib, 40*gib, 2); a != 2*b {
		t.Fatalf("thrash time scales with the factor: %v vs %v", a, b)
	}
	if RefetchBytes(20*gib, 40*gib, 0) != 40*gib || RefetchBytes(20*gib, 10*gib, 3) != 0 {
		t.Fatal("refetch bytes")
	}
}

// Cost-optimised accepts thrashing at the default factor, but a calibrated
// factor of 10 makes thrashing cost more machine time than the resize.
func TestCalibratedRefetchFlipsResize(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Provider, m.Size, m.Stack, m.DiskTotal, m.CanResize = "aws", "small", localSizes()[0].Stack, 20*gib, true
	m.ExpiresAt = now.Add(6 * time.Hour)
	f := Pool{Now: now, Sizes: localSizes(), Machines: []Machine{m}}
	pol := Default{Cfg: Config{CostOptimization: 1, CPUOvercommit: 1, MemoryOvercommit: 1}}
	if pl := pol.Place(f, wsTask(40*gib)); pl.Slot.Kind != SlotNone || !strings.Contains(pl.Reason, "thrashing") {
		t.Fatalf("default factor: want to thrash, got %+v", pl)
	}
	f.Refetch = map[string]float64{"aws": 10}
	if pl := pol.Place(f, wsTask(40*gib)); pl.Slot.Kind != SlotResize || strings.Contains(pl.Reason, "thrashing") {
		t.Fatalf("calibrated factor 10: want a resize, got %+v", pl)
	}
}

// A restore whose working set does not fit the local tier grows the disk
// in the same step (one placement, the restore kept) instead of
// restoring and thrashing or growing and running cold.
func TestRestoreAndResizeJointly(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Provider, m.Size, m.Stack, m.DiskTotal, m.CanResize = "aws", "small", localSizes()[0].Stack, 20*gib, true
	m.ExpiresAt, m.IdleSince = now.Add(6*time.Hour), now.Add(-time.Minute)
	tk := wsTask(40 * gib)
	tk.RepoID = "r"
	tk.Profile.Cold = time.Hour // a cold build costs more than any restore
	snap := Snapshot{Repo: "r", Provider: "aws", Store: "aws", Bytes: 15 * gib, At: now.Add(-time.Hour)}
	f := Pool{Now: now, Sizes: localSizes(), Machines: []Machine{m}, Snapshots: map[string][]Snapshot{"r": {snap}}}
	pl := Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	if pl.Kind != Assign || pl.Slot.Kind != SlotRestore || pl.Slot.Grow < 50*gib || strings.Contains(pl.Reason, "thrashing") ||
		pl.Slot.Steps() != "resize+restore" {
		t.Fatalf("want restore with a resize for the working set, got %+v (%s)", pl, pl.Slot.Steps())
	}
	// A full disk: the restore keeps its kind and grows the disk in the
	// same step (before, the resize replaced the restore and the build ran
	// cold); it never prunes another repository's warm slot.
	m.SlotBytes, m.DiskUsed = map[string]int64{"other": 12 * gib}, 14*gib
	tk.Profile.WorkingSet = 5 * gib
	f.Machines = []Machine{m}
	for _, r := range []float64{0, 0.5, 1} {
		pl := Default{Cfg: Config{CostOptimization: r, CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
		if pl.Kind != Assign || pl.Slot.Kind != SlotRestore || pl.Slot.Grow < 20*gib+15*gib-14*gib {
			t.Fatalf("r=%v: want the restore with the disk grown, got %+v", r, pl)
		}
	}
	// Without a resize the restore does not fit: cold.
	m.CanResize = false
	f.Machines = []Machine{m}
	if pl := (Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}}).Place(f, tk); pl.Kind != Assign || pl.Slot.Kind == SlotRestore {
		t.Fatalf("want a cold build, got %+v", pl)
	}
}

// Growing a volume below a fixed local disk does not grow the local tier:
// no resize is offered against thrashing.
func TestNoResizeForWorkingSetBelowFixedLocalTier(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Provider, m.Size, m.DiskTotal, m.CanResize = "aws", "v", 100*gib, true
	m.Stack = StackOf(20*gib, medium.Default("aws-ec2", medium.LocalNVMe), medium.Default("aws-ec2", medium.Volume), medium.Default("aws-ec2", medium.Store))
	f := Pool{Now: now, Machines: []Machine{m}}
	for _, rc := range (Default{}).runChoices(f, m, wsTask(40*gib)) {
		if rc.slot.Kind == SlotResize {
			t.Fatalf("resize offered: %+v", rc)
		}
	}
}

// thrashRuns are warm runs of 10 minutes base time with a 40G working set
// on the small size (20G local): each refaults refetch x 20G, which the
// worker reports and the orchestrator turns into thrash seconds.
func thrashRuns(h *History, key string, n int, refetch float64) (wall, thrash time.Duration) {
	st := localSizes()[0].Stack
	refault := int64(refetch * float64(20*gib))
	thrash = RefaultTime(st, refault)
	wall = 10*time.Minute + thrash
	for i := 0; i < n; i++ {
		h.Observe(key, Usage{Wall: wall, Warm: true, WorkingSetBytes: 40 * gib, LocalCapacity: 20 * gib,
			RefaultBytes: refault, ThrashSeconds: thrash.Seconds(), Provider: "aws"})
	}
	return wall, thrash
}

// The learned run times exclude the observed thrashing and the model adds
// the thrashing of the candidate machine once: on the same machine the
// predicted run is the observed one, on a machine whose local tier holds
// the working set the thrashing is gone (before: base + thrash learned,
// plus the penalty again).
func TestThrashNotCountedTwice(t *testing.T) {
	h := NewHistory("")
	wall, thrash := thrashRuns(h, "r", 5, 3)
	p := h.Profile("r")
	if p.Warm != 10*time.Minute || p.Thrash != thrash || p.Refetch != 3 {
		t.Fatalf("profile: warm %v thrash %v refetch %v", p.Warm, p.Thrash, p.Refetch)
	}
	tk := task(4, 8)
	tk.Profile = p
	place := func(disk int64) Placement {
		m := machine("m", res(8, 16), Resources{})
		m.Provider, m.Size, m.Stack, m.WarmRepos = "aws", "small", StackOf(disk, localSizes()[0].Stack.Levels...), []string{"r"}
		m.VM.Capacity = disk
		// The provider calibration is off (factor 1): the key's own wins.
		f := Pool{Now: now, Sizes: localSizes(), Machines: []Machine{m}, Refetch: map[string]float64{"aws": 1}}
		return Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	}
	same := place(20 * gib)
	if same.Kind != Assign || !strings.Contains(same.Reason, "thrashing") {
		t.Fatalf("same machine: %+v", same)
	}
	if d := same.Run - wall; d < -time.Second || d > time.Second {
		t.Fatalf("same machine: predicted %v, observed %v", same.Run, wall)
	}
	big := place(100 * gib)
	if big.Run != 10*time.Minute || strings.Contains(big.Reason, "thrashing") {
		t.Fatalf("bigger disk: predicted %v (%s), want the base 10m", big.Run, big.Reason)
	}
}

// The refetch factor is learned per key: two keys on the same provider
// keep their own factors; a key with fewer than MinRefetchRuns thrashing
// runs uses the provider's calibration.
func TestRefetchPerKey(t *testing.T) {
	h := NewHistory("")
	thrashRuns(h, "light", 3, 0.5)
	thrashRuns(h, "heavy", 3, 8)
	thrashRuns(h, "new", 1, 8)
	f := Pool{Refetch: map[string]float64{"aws": 2}}
	for key, want := range map[string]float64{"light": 0.5, "heavy": 8, "new": 2} {
		tk := Task{Key: key, Profile: h.Profile(key)}
		if got := refetchOf(f, "aws", tk); got != want {
			t.Errorf("%s: refetch %v, want %v", key, got, want)
		}
	}
	if got := refetchOf(Pool{Refetch: map[string]float64{"": 3}}, "oci", Task{}); got != 3 {
		t.Errorf("global fallback: %v", got)
	}
	if got := refetchOf(Pool{}, "oci", Task{}); got != DefaultRefetch {
		t.Errorf("default: %v", got)
	}
}

package scheduler

import (
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

func volSize() Size {
	return Size{Provider: "aws", Name: "v", Capacity: res(8, 16), PricePerHour: 0.4, Medium: medium.Volume,
		Stack:  StackOf(0, medium.Default("aws-ec2", medium.Volume), medium.Default("aws-ec2", medium.Store)),
		Volume: VolumePolicy{Min: 100 * gib, Step: 50 * gib, KeepDetached: true, TTL: 24 * time.Hour}}
}

// A queued task whose warm slot is on a detached volume: the new machine
// attaches that volume (warm for the attach time) instead of a fresh one.
func TestPlanAttachesWarmDetachedVolume(t *testing.T) {
	tk := profiled(2*time.Minute, 20*time.Minute)
	tk.RepoID = "r"
	vol := Volume{ID: "vol-1", Provider: "aws", Zone: "eu-central-1b", Bytes: 150 * gib, SlotBytes: map[string]int64{"r": 30 * gib}, ExpiresAt: now.Add(time.Hour)}
	f := Pool{Now: now, Sizes: []Size{volSize()}, Queue: []Task{tk}, Volumes: []Volume{vol}}
	for _, r := range []float64{0, 0.5, 1} {
		acts := createActs(Default{Cfg: Config{CostOptimization: r, CPUOvercommit: 1, MemoryOvercommit: 1}}.Plan(f))
		if len(acts) != 1 || acts[0].Volume != "vol-1" {
			t.Fatalf("r=%v: want a create on the warm volume, got %+v", r, acts)
		}
	}
	// Expired, other provider or another repository: a fresh volume.
	for _, v := range []Volume{
		{ID: "vol-2", Provider: "aws", SlotBytes: map[string]int64{"r": gib}, ExpiresAt: now.Add(-time.Second)},
		{ID: "vol-3", Provider: "oci", SlotBytes: map[string]int64{"r": gib}},
		{ID: "vol-4", Provider: "aws", SlotBytes: map[string]int64{"other": gib}},
	} {
		f.Volumes = []Volume{v}
		acts := createActs(Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}.Plan(f))
		if len(acts) != 1 || acts[0].Volume != "" {
			t.Fatalf("%s: want a fresh volume, got %+v", v.ID, acts)
		}
	}
	// Local sizes never attach volumes.
	ls := volSize()
	ls.Medium = medium.Local
	f.Sizes, f.Volumes = []Size{ls}, []Volume{vol}
	if acts := createActs(Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}.Plan(f)); len(acts) != 1 || acts[0].Volume != "" {
		t.Fatalf("local size: %+v", acts)
	}
}

// The resize cooldown (EBS: 6 h) blocks a resize; the task waits instead.
func TestResizeCooldown(t *testing.T) {
	m := machine("m", res(8, 16), Resources{})
	m.Provider, m.Size, m.DiskTotal, m.CanResize = "aws", "v", 100*gib, true
	m.Disk = &DiskLedger{Total: 100 * gib, Free: 12 * gib, Pinned: 88 * gib, Reserve: 10 * gib}
	m.Medium, m.Stack = medium.Volume, volSize().Stack
	tk := profiled(time.Minute, 3*time.Minute)
	tk.Profile.SlotBytes = 20 * gib
	pol := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}
	// No capacity to create (provider down): the disk grows.
	f := Pool{Now: now, Sizes: []Size{volSize()}, Machines: []Machine{m}, Providers: map[string]ProviderState{"aws": {Down: now.Add(24 * time.Hour)}}}
	pl := pol.Place(f, tk)
	if pl.Kind != Assign || pl.Slot.Grow < m.DiskTotal+50*gib {
		t.Fatalf("want a resize of at least one step, got %+v", pl)
	}
	f.Machines[0].ResizableAt = now.Add(time.Hour)
	if pl := pol.Place(f, tk); pl.Kind != Wait {
		t.Fatalf("within the cooldown: want wait, got %+v", pl)
	}
}

// A slow hot medium (OCI balanced volume, ~48 MB/s) bounds a restore.
func TestRestoreBoundedByHotMedium(t *testing.T) {
	fast := Machine{Stack: StackOf(0, medium.Default("aws-ec2", medium.LocalNVMe))}
	slow := Machine{Stack: StackOf(0, medium.Default("oci", medium.Volume))}
	b := int64(4800) << 20
	if d := restoreTime(fast, Task{}, b, 200<<20); d != 24*time.Second+RestoreMount {
		t.Fatalf("fast medium: %v", d)
	}
	// 100 s transfer plus 600 pages of 1 ms latency.
	if d := restoreTime(slow, Task{}, b, 200<<20); d != 100600*time.Millisecond+RestoreMount {
		t.Fatalf("slow medium: %v", d)
	}
	// Lazy restore: a build with a known working set waits only for it.
	lazy := Task{Profile: Profile{WorkingSet: b / 10}}
	if d := restoreTime(fast, lazy, b, 200<<20); d != 2400*time.Millisecond+RestoreMount {
		t.Fatalf("lazy restore: %v", d)
	}
	// The read set (what runs read of the slot they start on) beats the
	// working set, which also counts what they write.
	lazy.Profile.ReadSet = b / 100
	if d := restoreTime(fast, lazy, b, 200<<20); d != 240*time.Millisecond+RestoreMount {
		t.Fatalf("read set: %v", d)
	}
}

func TestStoragePrice(t *testing.T) {
	s := volSize()
	want := 0.4 + 100*1.073741824*0.08/730
	if got := sizePrice(s); got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("size price %v, want %v", got, want)
	}
	m := Machine{Provider: "aws", Size: "v", Medium: medium.Volume, Stack: s.Stack, DiskTotal: 200 * gib}
	if got := priceOf(Pool{Sizes: []Size{s}}, m); got <= sizePrice(s) {
		t.Fatalf("a 200G volume costs more than a 100G one: %v", got)
	}
	s.Medium = medium.Local
	if sizePrice(s) != 0.4 {
		t.Fatal("local medium is priced into the size")
	}
}

func createActs(acts []Action) []Action {
	var out []Action
	for _, a := range acts {
		if a.Kind == Create {
			out = append(out, a)
		}
	}
	return out
}

package sim

import (
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/scheduler"
)

// localSizes: a cheap size with a 20G local disk and one with 100G at
// twice the price (local disk over the store).
func localSizes() []scheduler.Size {
	mk := func(name string, disk int64, price float64) scheduler.Size {
		return scheduler.Size{Provider: "aws", Name: name, Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib, Disk: disk}, PricePerHour: price,
			Medium: medium.Local, Stack: scheduler.StackOf(disk, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Store))}
	}
	return []scheduler.Size{mk("small", 20*gib, 0.1), mk("big", 100*gib, 0.2)}
}

// spaced returns n builds of a repository two hours apart (machines
// expire in between, so every build places a new machine).
func spaced(n int, j Job) Trace {
	var js []Job
	for i := 0; i < n; i++ {
		j.At = float64(i) * 7200
		js = append(js, j)
	}
	return Trace{Start: DefaultStart, End: float64(n) * 7200, Jobs: js}
}

// A working set of 40G thrashes on the small size's 20G disk. Once the
// estimator learned it, balanced and time-optimised scheduling create
// the bigger size; cost-optimised keeps the cheaper small size and
// thrashes (cheaper, slower).
func TestThrashingPicksBiggerSize(t *testing.T) {
	tr := spaced(6, Job{Repo: "r", Env: "e", Duration: 600, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib), WorkingSet: Bytes(40 * gib)})
	run := func(r float64) Report {
		o := opts(scheduler.Config{CostOptimization: r})
		o.Sizes, o.MaxLifetime, o.Estimator = localSizes(), time.Hour, scheduler.NewHistory("")
		return Run(tr, o)
	}
	for _, r := range []float64{0, 0.5} {
		rep := run(r)
		if rep.Completed != 6 || rep.Created["big"] < 5 || rep.Thrashed > 1 {
			t.Errorf("r=%v: want the big size after learning: created %v, thrashed %d", r, rep.Created, rep.Thrashed)
		}
	}
	cheap, fast := run(1), run(0.5)
	if cheap.Created["big"] != 0 || cheap.Thrashed != 6 || cheap.ThrashTime <= 0 {
		t.Errorf("r=1: want the small size, thrashing: created %v, thrashed %d", cheap.Created, cheap.Thrashed)
	}
	if cheap.Cost >= fast.Cost || cheap.TurnP50 <= fast.TurnP50 {
		t.Errorf("thrashing is cheaper and slower: cost %.3f vs %.3f, turnaround %v vs %v", cheap.Cost, fast.Cost, cheap.TurnP50, fast.TurnP50)
	}
}

// Jobs without a working set model no cache file system: no thrashing,
// no page-ins, the cheap size (the behaviour before tier stacks).
func TestNoWorkingSetNoThrash(t *testing.T) {
	tr := spaced(3, Job{Repo: "r", Env: "e", Duration: 600, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib)})
	o := opts(scheduler.Config{})
	o.Sizes, o.MaxLifetime = localSizes(), time.Hour
	rep := Run(tr, o)
	if rep.Created["small"] != 3 || rep.Thrashed != 0 || rep.PageInBytes != 0 {
		t.Fatalf("%+v", rep)
	}
}

// A machine that keeps running thrashes; with resizable disks the
// balanced policy grows the disk to the working set once.
func TestThrashingResizesDisk(t *testing.T) {
	var js []Job
	for i := 0; i < 6; i++ {
		js = append(js, Job{At: float64(i * 900), Repo: "r", Env: "e", Duration: 600, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib), WorkingSet: Bytes(40 * gib)})
	}
	tr := Trace{Start: DefaultStart, End: 3 * 3600, Jobs: js}
	o := opts(scheduler.Config{CostOptimization: 0.5})
	o.Sizes, o.Estimator, o.Resize, o.MaxLifetime = localSizes()[:1], scheduler.NewHistory(""), true, 4*time.Hour
	rep := Run(tr, o)
	if rep.Completed != 6 || rep.Resizes != 1 || rep.Thrashed != 1 {
		t.Fatalf("want one resize after the first thrashing build: resizes %d, thrashed %d", rep.Resizes, rep.Thrashed)
	}
}

// A restored slot is mounted lazily: only the working set pages in
// before the build proceeds, so restoring a large slot beats a cold
// build; restoring all of it would not.
func TestLazyRestore(t *testing.T) {
	j := Job{Repo: "r", Env: "e", Duration: 300, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib), SlotBytes: Bytes(60 * gib), SlotGrowth: Bytes(gib)}
	run := func(ws int64) Report {
		jj := j
		jj.WorkingSet = Bytes(ws)
		o := opts(scheduler.Config{})
		o.Sizes, o.MaxLifetime, o.ColdPenalty, o.Estimator = localSizes()[1:], time.Hour, 0.5, scheduler.NewHistory("")
		return Run(spaced(2, jj), o)
	}
	lazy, full := run(5*gib), run(0)
	if lazy.Restores != 1 || full.Restores != 0 || lazy.PageInBytes != 5*gib {
		t.Fatalf("restores %d (paged in %d) vs %d", lazy.Restores, lazy.PageInBytes, full.Restores)
	}
	if lazy.Slowdown >= full.Slowdown {
		t.Fatalf("the lazy restore is faster than a cold build: slowdown %.2f vs %.2f", lazy.Slowdown, full.Slowdown)
	}
}

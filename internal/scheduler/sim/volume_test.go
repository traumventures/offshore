package sim

import (
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/scheduler"
)

func volumeSizes(keep bool) []scheduler.Size {
	return []scheduler.Size{{Provider: "p", Name: "v", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 1,
		Medium: medium.Volume, Stack: scheduler.StackOf(0, medium.Default("aws-ec2", medium.Volume), medium.Default("aws-ec2", medium.Store)),
		Volume: scheduler.VolumePolicy{Min: 100 * gib, Step: 50 * gib, KeepDetached: keep, TTL: 24 * time.Hour}}}
}

// A machine's lifetime ends between two builds of a repository: with
// keep_detached the next machine attaches the warm volume.
func TestDetachedVolumeReused(t *testing.T) {
	j := Job{Repo: "r", Env: "e", Duration: 300, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib), SlotBytes: Bytes(20 * gib), SlotGrowth: Bytes(gib)}
	j2 := j
	j2.At = 3 * 3600
	tr := Trace{Start: DefaultStart, End: 4 * 3600, Jobs: []Job{j, j2}}
	run := func(keep bool) Report {
		o := opts(scheduler.Config{})
		o.Sizes, o.MaxLifetime = volumeSizes(keep), 30*time.Minute
		return Run(tr, o)
	}
	kept, dropped := run(true), run(false)
	if kept.Completed != 2 || kept.VolumeAttaches != 1 || kept.VolumesCreated != 1 || kept.WarmHits != 1 {
		t.Fatalf("keep_detached: %+v", kept)
	}
	if dropped.VolumeAttaches != 0 || dropped.VolumesCreated != 2 || dropped.WarmHits != 0 {
		t.Fatalf("deleted with the machine: %+v", dropped)
	}
	if kept.VolumeCost <= dropped.VolumeCost || kept.VolumeCost <= 0 {
		t.Fatalf("a kept volume costs storage while detached: %v vs %v", kept.VolumeCost, dropped.VolumeCost)
	}
}

// Volume resizes respect the 6 h cooldown.
func TestVolumeResizeCooldown(t *testing.T) {
	var js []Job
	for i := 0; i < 6; i++ {
		js = append(js, Job{At: float64(i * 600), Repo: "r", Env: "e", Duration: 300, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib),
			SlotBytes: Bytes(80 * gib), SlotGrowth: Bytes(40 * gib)})
	}
	o := opts(scheduler.Config{})
	o.Sizes, o.Estimator = volumeSizes(false), scheduler.NewHistory("")
	r := Run(Trace{Start: DefaultStart, End: 4 * 3600, Jobs: js}, o)
	if r.Completed != 6 || r.Resizes > r.Machines || r.ENOSPC > 0 {
		t.Fatalf("want at most one resize per machine within 6 h: %+v", r)
	}
}

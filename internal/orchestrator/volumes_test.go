package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/clock"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

func volumeSize(keep bool) scheduler.Size {
	return scheduler.Size{Provider: "fake", Name: "v4", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib, Disk: 100 * gib},
		Medium: medium.Volume, Stack: scheduler.StackOf(0, medium.Default("aws-ec2", medium.Volume), medium.Default("aws-ec2", medium.Store)),
		Volume: scheduler.VolumePolicy{Min: 100 * gib, Max: 300 * gib, Step: 50 * gib, KeepDetached: keep, TTL: 24 * time.Hour}}
}

func waitVolume(t *testing.T, o *Orchestrator, ok func([]VolumeState) bool) []VolumeState {
	t.Helper()
	for i := 0; i < 200; i++ {
		if vs := o.Volumes(); ok(vs) {
			return vs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("volumes: %+v", o.Volumes())
	return nil
}

func countEvents(evs []events.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestVolumeKeptAndReused(t *testing.T) {
	f := newFake()
	f.volCaps = provider.VolumeCaps{ResizeCooldown: 6 * time.Hour, Bounded: true}
	sz := volumeSize(true)
	o := newTestOrch(t, f, func(c *Config) { c.Sizes = []scheduler.Size{sz} })
	clk := clock.NewFake(time.Now())
	o.Clock = clk
	ctx := context.Background()
	if err := o.CreateMachine(ctx, "m1", "env1env1env1env1", "img", sz); err != nil {
		t.Fatal(err)
	}
	vs := o.Volumes()
	if len(vs) != 1 || vs[0].Machine != "m1" || vs[0].Bytes != 100*gib || vs[0].Zone != "z1" || f.specs[0].Volume != vs[0].ID || f.specs[0].CacheTTL != 24*time.Hour {
		t.Fatalf("volumes %+v spec %+v", vs, f.specs[0])
	}
	// The scheduler sees the machine's volume.
	o.mu.Lock()
	o.machines["m1"].Status.Disk = nil
	pool := o.poolLocked(o.now())
	o.mu.Unlock()
	if len(pool.Machines) != 1 || pool.Machines[0].Volume != vs[0].ID || pool.Machines[0].Medium != medium.Volume || !pool.Machines[0].CanResize {
		t.Fatalf("pool machine %+v", pool.Machines)
	}
	// Resize: at least one step; a second within the cooldown is refused.
	if err := o.resizeVolume(ctx, "m1", 110*gib); err != nil {
		t.Fatal(err)
	}
	if v := o.Volumes()[0]; v.Bytes != 150*gib || f.resized != 1 {
		t.Fatalf("resized %+v", v)
	}
	if err := o.resizeVolume(ctx, "m1", 400*gib); err == nil || f.resized != 1 {
		t.Fatal("second resize within the cooldown")
	}
	clk.Advance(7 * time.Hour)
	if err := o.resizeVolume(ctx, "m1", 400*gib); err != nil || o.Volumes()[0].Bytes != 300*gib {
		t.Fatalf("capped at max: %v %+v", err, o.Volumes())
	}
	if err := o.resizeVolume(ctx, "m1", 400*gib); err == nil {
		t.Fatal("resize beyond max")
	}
	// Destroying the machine keeps the volume detached until its ttl.
	o.destroyMachine(ctx, "m1", "test")
	vs = waitVolume(t, o, func(vs []VolumeState) bool { return len(vs) == 1 && vs[0].Machine == "" })
	if !vs[0].ExpiresAt.Equal(clk.Now().Add(24*time.Hour)) || f.volumes[vs[0].ID].ExpiresAt.IsZero() {
		t.Fatalf("detached %+v provider %+v", vs[0], f.volumes[vs[0].ID])
	}
	o.mu.Lock()
	pool = o.poolLocked(o.now())
	o.mu.Unlock()
	if len(pool.Volumes) != 1 || pool.Volumes[0].ID != vs[0].ID {
		t.Fatalf("pool volumes %+v", pool.Volumes)
	}
	// The next machine of the size reuses it.
	if err := o.CreateMachine(ctx, "m2", "env1env1env1env1", "img", sz); err != nil {
		t.Fatal(err)
	}
	if v := o.Volumes(); len(v) != 1 || v[0].Machine != "m2" || f.volCreated != 1 || f.specs[1].Volume != v[0].ID {
		t.Fatalf("reuse %+v created %d", v, f.volCreated)
	}
	o.destroyMachine(ctx, "m2", "test")
	waitVolume(t, o, func(vs []VolumeState) bool { return len(vs) == 1 && vs[0].Machine == "" })
	// Expired: the reaper deletes it.
	clk.Advance(25 * time.Hour)
	if err := o.reapVolumes(ctx); err != nil {
		t.Fatal(err)
	}
	if len(o.Volumes()) != 0 || len(f.volumes) != 0 {
		t.Fatalf("expired volume kept: %+v %+v", o.Volumes(), f.volumes)
	}
	evs := readEvents(t, o)
	for typ, n := range map[string]int{events.VolumeCreate: 1, events.VolumeAttach: 2, events.VolumeDetach: 2, events.VolumeResize: 2, events.VolumeDelete: 1} {
		if got := countEvents(evs, typ); got != n {
			t.Errorf("%s: %d events, want %d", typ, got, n)
		}
	}
	var cost float64
	for _, e := range evs {
		cost += e.Cost
	}
	if cost <= 0 {
		t.Error("volume events carry no storage cost")
	}
}

func TestVolumeDeletedWithMachineByDefault(t *testing.T) {
	f := newFake()
	sz := volumeSize(false)
	o := newTestOrch(t, f, func(c *Config) { c.Sizes = []scheduler.Size{sz} })
	ctx := context.Background()
	if err := o.CreateMachine(ctx, "m1", "env1env1env1env1", "img", sz); err != nil {
		t.Fatal(err)
	}
	o.destroyMachine(ctx, "m1", "test")
	waitVolume(t, o, func(vs []VolumeState) bool { return len(vs) == 0 })
	if len(f.volumes) != 0 {
		t.Fatalf("provider kept %+v", f.volumes)
	}
}

func TestVolumeTarget(t *testing.T) {
	p := scheduler.VolumePolicy{Step: 50, Max: 300}
	for _, c := range []struct{ cur, need, want int64 }{{100, 110, 150}, {100, 220, 220}, {280, 400, 300}} {
		if got := volumeTarget(p, c.cur, c.need); got != c.want {
			t.Errorf("%d -> %d: %d, want %d", c.cur, c.need, got, c.want)
		}
	}
}

type nvmeFake struct{ *fakeProvider }

func (nvmeFake) InstanceStorage(_ context.Context, typ string) (int64, error) {
	if typ == "m6id.xlarge" {
		return 237e9, nil
	}
	return 0, nil
}

func TestLocalDisksFromInstanceStore(t *testing.T) {
	f := newFake()
	cfg := testConfig("fake")
	cfg.Sizes = []scheduler.Size{
		{Provider: "fake", Name: "m6id.xlarge", Medium: medium.Local, Capacity: scheduler.Resources{CPU: 4, Memory: 16 * gib}},
		{Provider: "fake", Name: "c7a.xlarge", Medium: medium.Local, Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}},
	}
	cfg.Profile = func(prov, med string) medium.Profile { return medium.Default("aws-ec2", med) }
	o, err := New(cfg, map[string]provider.Provider{"fake": nvmeFake{f}}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.resolveLocalDisks(context.Background())
	sz := o.Config().Sizes
	if sz[0].Capacity.Disk != 237e9 || sz[0].Stack.Top().Throughput != medium.Default("aws-ec2", medium.LocalNVMe).Throughput {
		t.Fatalf("instance store size %+v", sz[0])
	}
	if sz[1].Capacity.Disk != 0 {
		t.Fatalf("size without instance store %+v", sz[1])
	}
}

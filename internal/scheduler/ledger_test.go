package scheduler

import (
	"strings"
	"testing"
	"time"
)

// Production 2026-11-12: every machine's disk was used beyond its budget
// by data outside the slots and a warm slot's growth fitted nowhere; the
// tasks waited for their queue timeout. With the ledger a machine that
// cannot make room is no option, so the task gets a new machine instead
// of running into a full disk.
func TestNoRoomTakesNewCapacity(t *testing.T) {
	m := machine("m", res(8, 30), Resources{})
	m.WarmRepos = []string{"r"}
	m.SlotBytes = map[string]int64{"r": 30 * gib}
	m.DiskTotal = 200 * gib
	m.Disk = &DiskLedger{Total: 200 * gib, Free: 5 * gib, Pinned: 195 * gib, Reserve: 20 * gib}
	tk := task(0, 0)
	tk.Profile.SlotGrowth = 30 * gib
	sizes := []Size{{Provider: "p", Name: "m", Capacity: Resources{CPU: 8, Memory: 30 * gib, Disk: 200 * gib}, PricePerHour: 0.4}}
	pl := (Default{}).Place(Pool{Now: now, Sizes: sizes, Machines: []Machine{m}}, tk)
	if pl.Kind == Assign && pl.Machine == "m" {
		t.Fatalf("task on a machine without room: %+v", pl)
	}
	// Room from a demotable slot: the warm slot runs after a reclaim.
	m.Disk.Pinned = 95 * gib
	m.Disk.Reclaim = []Reclaimable{{Class: ReclaimSlot, Repo: "other", Bytes: 100 * gib, Cost: 20}}
	pl = (Default{}).Place(Pool{Now: now, Sizes: sizes, Machines: []Machine{m}}, tk)
	if pl.Kind != Assign || pl.Machine != "m" || pl.Slot.Reclaim != 30*gib || !strings.Contains(pl.Reason, "reclaim") {
		t.Fatalf("reclaimable disk: %+v", pl)
	}
}

// A disk whose pinned data and reserve exceed 70 % grows in the
// background, once per cooldown.
func TestGrowDisks(t *testing.T) {
	m := machine("m", res(8, 30), Resources{})
	m.DiskTotal, m.CanResize = 200*gib, true
	m.Disk = &DiskLedger{Total: 200 * gib, Free: 40 * gib, Pinned: 130 * gib, Reserve: 20 * gib}
	acts := (Default{}).Plan(Pool{Now: now, Machines: []Machine{m}})
	var grow *Action
	for i := range acts {
		if acts[i].Kind == Grow {
			grow = &acts[i]
		}
	}
	if grow == nil || grow.Machine != "m" || grow.Bytes != 300*gib {
		t.Fatalf("want a grow to 300G, got %+v", acts)
	}
	m.ResizableAt = now.Add(time.Hour)
	for _, a := range (Default{}).Plan(Pool{Now: now, Machines: []Machine{m}}) {
		if a.Kind == Grow {
			t.Fatalf("grow within the cooldown: %+v", a)
		}
	}
	m.ResizableAt = now
	m.Disk.Pinned = 100 * gib
	for _, a := range (Default{}).Plan(Pool{Now: now, Machines: []Machine{m}}) {
		if a.Kind == Grow {
			t.Fatalf("grow below 70 %%: %+v", a)
		}
	}
}

// FreshDisk is what a new machine reports before its first session.
func TestFreshDisk(t *testing.T) {
	l := FreshDisk(200*gib, map[string]int64{"r": 30 * gib})
	if l.Free != 200*gib-18*gib-30*gib || l.Reserve != 20*gib || l.Room() != l.Free-20*gib {
		t.Fatalf("fresh disk: %+v", l)
	}
	if FreshDisk(0, nil) != nil {
		t.Fatal("unknown disk")
	}
}

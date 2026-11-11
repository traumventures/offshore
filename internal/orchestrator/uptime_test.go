package orchestrator

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/provider"
)

// A restarted orchestrator recovers the uptime of today's machines from
// the event log, so that cost so far today never goes down: stopped,
// destroyed and vanished machines keep their uptime.
func TestUptimeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	log := events.Open(dir, 0)
	for _, ev := range []events.Event{
		{Type: events.MachineCreate, Machine: "m1", Provider: "fake", Size: "m4", Time: at(3 * time.Hour)},
		{Type: events.MachineStop, Machine: "m1", Time: at(2 * time.Hour)},
		{Type: events.MachineCreate, Machine: "m2", Provider: "fake", Size: "m4", Time: at(90 * time.Minute)},
		{Type: events.MachineReady, Machine: "m2", Time: at(89 * time.Minute)},
		{Type: events.MachineDestroy, Machine: "m2", Time: at(60 * time.Minute)},
		{Type: events.MachineCreate, Machine: "m3", Provider: "fake", Size: "m4", Time: at(30 * time.Minute)},
		{Type: events.MachineSample, Machine: "m3", Time: at(10 * time.Minute)},
		{Type: events.MachineCreate, Machine: "m4", Provider: "fake", Size: "m4", Time: at(20 * time.Minute)},
		{Type: events.MachineReady, Machine: "m4", Time: at(19 * time.Minute)},
	} {
		log.Append(ev)
	}
	// A failed-create storm: create requests (some closed by
	// create_failed) of machines that never existed.
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("storm%d", i)
		log.Append(events.Event{Type: events.MachineCreate, Machine: id, Provider: "fake", Size: "m4", Time: at(15 * time.Minute)})
		if i%2 == 0 {
			log.Append(events.Event{Type: events.MachineDestroy, Machine: id, Reason: "create_failed", Time: at(15 * time.Minute)})
		}
	}
	log.Close()
	f := newFake()
	f.machines["m1"] = &provider.Machine{ID: "m1", State: provider.Stopped}
	f.machines["m4"] = &provider.Machine{ID: "m4", State: provider.Running}
	o := newTestOrchDir(t, f, dir)
	if err := o.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]time.Duration{}
	snap := o.Snapshot()
	if len(snap) != 4 {
		t.Fatalf("%d machines in the snapshot, want 4 (storm creates must not appear)", len(snap))
	}
	for _, m := range snap {
		got[m.ID] = m.UpTime(at(24*time.Hour), time.Now()).Round(time.Minute)
	}
	want := map[string]time.Duration{"m1": time.Hour, "m2": 30 * time.Minute, "m3": 20 * time.Minute, "m4": 20 * time.Minute}
	for id, d := range want {
		if got[id] != d {
			t.Errorf("%s up %v, want %v (all %v)", id, got[id], d, got)
		}
	}
}

// A machine whose size left the pool configuration keeps its price: from
// its LabelPrice, else from its events, else the configuration.
func TestPriceSurvivesSizeRemoval(t *testing.T) {
	dir := t.TempDir()
	log := events.Open(dir, 0)
	log.Append(events.Event{Type: events.MachineReady, Machine: "old", Provider: "fake", Size: "x4", Price: 0.25, Time: time.Now().Add(-time.Hour)})
	log.Close()
	f := newFake()
	f.machines["tagged"] = &provider.Machine{ID: "tagged", State: provider.Running,
		Labels: map[string]string{provider.LabelSize: "x4", provider.LabelPrice: "0.5"}}
	f.machines["old"] = &provider.Machine{ID: "old", State: provider.Running, Labels: map[string]string{provider.LabelSize: "x4"}}
	o := newTestOrchDir(t, f, dir)
	if err := o.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, m := range o.Snapshot() {
		got[m.ID] = m.PricePerHour
	}
	if got["tagged"] != 0.5 || got["old"] != 0.25 {
		t.Fatalf("prices %v", got)
	}
	// New machines record the configured price on the provider.
	o2 := newTestOrch(t, newFake(), func(c *Config) { c.Sizes[0].PricePerHour = 0.75 })
	if _, err := o2.Acquire(context.Background(), req("r"), io.Discard); err != nil {
		t.Fatal(err)
	}
	fp := o2.provs["fake"].(*fakeProvider)
	if len(fp.specs) != 1 || fp.specs[0].PricePerHour != 0.75 {
		t.Fatalf("spec price %+v", fp.specs)
	}
}

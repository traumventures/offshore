package orchestrator

import (
	"context"
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
		{Type: events.MachineDestroy, Machine: "m2", Time: at(60 * time.Minute)},
		{Type: events.MachineCreate, Machine: "m3", Provider: "fake", Size: "m4", Time: at(30 * time.Minute)},
		{Type: events.MachineSample, Machine: "m3", Time: at(10 * time.Minute)},
		{Type: events.MachineCreate, Machine: "m4", Provider: "fake", Size: "m4", Time: at(20 * time.Minute)},
	} {
		log.Append(ev)
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
	for _, m := range o.Snapshot() {
		got[m.ID] = m.UpTime(at(24*time.Hour), time.Now()).Round(time.Minute)
	}
	want := map[string]time.Duration{"m1": time.Hour, "m2": 30 * time.Minute, "m3": 20 * time.Minute, "m4": 20 * time.Minute}
	for id, d := range want {
		if got[id] != d {
			t.Errorf("%s up %v, want %v (all %v)", id, got[id], d, got)
		}
	}
}

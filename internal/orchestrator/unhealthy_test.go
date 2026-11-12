package orchestrator

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/machine"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

func TestNotReadyFor(t *testing.T) {
	now := time.Unix(10000, 0)
	up := now.Add(-20 * time.Minute)
	m := &Machine{Machine: provider.Machine{State: provider.Running}, Uptime: []Span{{Start: up}}, seenAt: up}
	// Never ready in this uptime period: booting since it began.
	if d := m.NotReadyFor(now); d != 20*time.Minute || m.EverReady() {
		t.Fatalf("booting: %v %v", d, m.EverReady())
	}
	// Ready before, not any more: since it was last ready.
	m.LastReady = now.Add(-3 * time.Minute)
	if d := m.NotReadyFor(now); d != 3*time.Minute || !m.EverReady() {
		t.Fatalf("unreachable: %v", d)
	}
	// Ready in an earlier uptime period only (stopped, started again):
	// booting since the start.
	m.LastReady = up.Add(-time.Hour)
	if d := m.NotReadyFor(now); d != 20*time.Minute || m.EverReady() {
		t.Fatalf("restarted: %v %v", d, m.EverReady())
	}
	// Seen only recently (control plane restart): not before that.
	m.seenAt = now.Add(-time.Minute)
	if d := m.NotReadyFor(now); d != time.Minute {
		t.Fatalf("after a restart: %v", d)
	}
	m.Status = &proto.Status{Ready: true}
	if d := m.NotReadyFor(now); d != 0 {
		t.Fatalf("ready: %v", d)
	}
	m.Status, m.State = nil, provider.Stopped
	if d := m.NotReadyFor(now); d != 0 {
		t.Fatalf("stopped: %v", d)
	}
}

// TestReplaceUnhealthy: a running machine whose worker has not been ready
// for UnhealthyAfter is destroyed, unless no machine of its provider is
// ready (the control plane's network is the likelier cause).
func TestReplaceUnhealthy(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	ctx := context.Background()
	full := req("r1")
	full.Need = scheduler.Resources{CPU: 4, Memory: 4 * gib}
	l1, err := o.Acquire(ctx, full, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	full.RepoID = "r2"
	l2, err := o.Acquire(ctx, full, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l1.MachineID == l2.MachineID {
		t.Fatal("expected two machines")
	}
	o.Release(l1.ID)
	o.Release(l2.ID)
	wedge := func(ids ...string) {
		f.mu.Lock()
		for _, id := range ids {
			f.status[id] = nil
		}
		f.mu.Unlock()
		o.mu.Lock()
		for _, id := range ids {
			m := o.machines[id]
			m.Status, m.LastReady, m.seenAt = nil, o.now().Add(-UnhealthyAfter-time.Minute), o.now().Add(-time.Hour)
			m.Uptime = []Span{{Start: o.now().Add(-time.Hour)}}
		}
		o.mu.Unlock()
	}
	// Both unreachable: nothing is replaced.
	saved := map[string]*proto.Status{}
	f.mu.Lock()
	for id, st := range f.status {
		saved[id] = st
	}
	f.mu.Unlock()
	wedge(l1.MachineID, l2.MachineID)
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if d := byType(readEvents(t, o), events.MachineDestroy); len(d) != 0 {
		t.Fatalf("destroyed while no machine answers: %+v", d)
	}
	// The first answers again, the second stays wedged: replaced.
	f.mu.Lock()
	f.status[l1.MachineID] = saved[l1.MachineID]
	f.mu.Unlock()
	wedge(l2.MachineID)
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	d := byType(readEvents(t, o), events.MachineDestroy)
	if len(d) != 1 || d[0].Machine != l2.MachineID || !strings.HasPrefix(d[0].Reason, "unhealthy") {
		t.Fatalf("machine_destroy %+v", d)
	}
}

func TestBootPhases(t *testing.T) {
	at := time.Unix(1000, 0)
	c := creation{at: at, provided: at.Add(7 * time.Second)}
	requested := time.Unix(5000, 0) // wall clock of the request (pool clock offset)
	got := bootPhases(c, requested, []machine.BootPhase{{Name: "kernel", At: 5_006_000}, {Name: "image", At: 5_040_000, Bytes: 9}}, 41_000)
	want := []events.BootPhase{{Phase: "provider", Ms: 7000}, {Phase: "kernel", Ms: 6000}, {Phase: "image", Ms: 40000, Bytes: 9}, {Phase: "observed", Ms: 41000}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%+v", got)
		}
	}
}

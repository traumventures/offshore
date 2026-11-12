package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

type rebootFake struct {
	*fakeProvider
	mu      sync.Mutex
	reboots []string
}

func (r *rebootFake) Reboot(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reboots = append(r.reboots, id)
	return nil
}

func (r *rebootFake) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reboots)
}

// Incident 2026-11-12: the only machine of the pool ran out of memory and
// stopped answering; requests queued behind it ("booting 27m") and it was
// never replaced (replacement needed another ready machine of the
// provider). An unreachable machine is no capacity, and while requests
// wait it is rebooted, then replaced.
func TestUnreachableSingleMachineRebootedThenReplaced(t *testing.T) {
	f := newFake()
	rf := &rebootFake{fakeProvider: f}
	o, err := New(testConfig("fake"), map[string]provider.Provider{"fake": rf}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.ProbeFunc = f.probe
	ctx := context.Background()
	l, err := o.Acquire(ctx, req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o.Release(l.ID)
	id := l.MachineID
	f.mu.Lock()
	f.status[id] = nil // the worker does not answer
	f.mu.Unlock()
	wedge := func(lastReady time.Duration) {
		o.mu.Lock()
		m := o.machines[id]
		m.Status, m.LastReady, m.seenAt = nil, o.now().Add(-lastReady), o.now().Add(-time.Hour)
		m.Uptime = []Span{{Start: o.now().Add(-time.Hour)}}
		o.mu.Unlock()
	}
	gone := func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		for _, sm := range o.poolLocked(o.now()).Machines {
			if sm.ID == id {
				return sm.State == scheduler.Gone
			}
		}
		return true
	}
	// Briefly unanswered: still capacity on its way, nothing happens.
	wedge(time.Minute)
	if gone() {
		t.Fatal("a minute without a probe already counts as gone")
	}
	// Unreachable for 3m without queued requests: no capacity, but it is
	// left alone (the lifecycle and unhealthy rules apply).
	wedge(3 * time.Minute)
	if !gone() {
		t.Fatal("an unreachable machine still counts as capacity")
	}
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if rf.count() != 0 {
		t.Fatal("rebooted without queued requests")
	}
	// A request waits for its environment: reboot first.
	o.mu.Lock()
	o.waiting["q1"] = QueuedTask{ID: "q1", Env: req("r2").EnvHash}
	o.mu.Unlock()
	wedge(3 * time.Minute)
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if rf.count() != 1 {
		t.Fatalf("reboots %d, want 1", rf.count())
	}
	if d := byType(readEvents(t, o), events.MachineDestroy); len(d) != 0 {
		t.Fatalf("destroyed before the reboot grace: %+v", d)
	}
	// Still within the grace: no second reboot, no destroy.
	wedge(4 * time.Minute)
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if rf.count() != 1 || len(byType(readEvents(t, o), events.MachineDestroy)) != 0 {
		t.Fatal("acted within the reboot grace")
	}
	// Still unreachable RebootGrace after the reboot: replaced.
	o.mu.Lock()
	o.machines[id].rebootedAt = o.now().Add(-RebootGrace - time.Second)
	o.mu.Unlock()
	wedge(6 * time.Minute)
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	d := byType(readEvents(t, o), events.MachineDestroy)
	if len(d) != 1 || d[0].Machine != id || !strings.Contains(d[0].Reason, "after a reboot") {
		t.Fatalf("machine_destroy %+v", d)
	}
}

// A machine that answers again after its reboot is reachable; a later
// outage reboots it again.
func TestUnreachableRecoveredAfterReboot(t *testing.T) {
	m := &Machine{Machine: provider.Machine{State: provider.Running}}
	now := time.Unix(10000, 0)
	m.Uptime, m.seenAt = []Span{{Start: now.Add(-time.Hour)}}, now.Add(-time.Hour)
	m.LastReady = now.Add(-3 * time.Minute)
	if !m.Unreachable(now) || m.UnreachableFor(now) != 3*time.Minute {
		t.Fatalf("unreachable for %v", m.UnreachableFor(now))
	}
	m.rebootedAt = now.Add(-2 * time.Minute)
	if !m.rebootedAt.After(m.LastReady) {
		t.Fatal("reboot after last ready")
	}
	m.LastReady = now.Add(-time.Second)
	if m.Unreachable(now) || m.rebootedAt.After(m.LastReady) {
		t.Fatal("ready again after the reboot")
	}
	// Booting (never ready in this uptime period) is not unreachable.
	b := &Machine{Machine: provider.Machine{State: provider.Running}, Uptime: []Span{{Start: now.Add(-time.Hour)}}, seenAt: now.Add(-time.Hour)}
	if b.Unreachable(now) {
		t.Fatal("booting machine unreachable")
	}
}

// After a control plane restart a machine that stopped answering shows
// as unreachable (its last sample), not as booting.
func TestReplayRestoresLastReady(t *testing.T) {
	now := time.Now()
	_, seen := replayUptime([]events.Event{
		{Type: events.MachineCreate, Machine: "m1", Time: now.Add(-time.Hour)},
		{Type: events.MachineReady, Machine: "m1", Time: now.Add(-59 * time.Minute)},
		{Type: events.MachineSample, Machine: "m1", Time: now.Add(-20 * time.Minute)},
	})
	s := seen["m1"]
	if s == nil || !s.LastReady.Equal(now.Add(-20*time.Minute)) {
		t.Fatalf("%+v", s)
	}
	o := &Orchestrator{seedUptime: seen}
	m := &Machine{}
	o.mergeUptimeLocked(m, "m1", provider.Running)
	m.State = provider.Running
	if !m.EverReady() {
		t.Fatal("not ever ready after the merge")
	}
}

// top and status read machines from the control plane as JSON: the
// readiness history must survive it.
func TestUnreachableSurvivesJSON(t *testing.T) {
	now := time.Now()
	m := Machine{Machine: provider.Machine{ID: "m", State: provider.Running}, LastReady: now.Add(-5 * time.Minute),
		Uptime: []Span{{Start: now.Add(-time.Hour)}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got Machine
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.EverReady() || !got.LastReady.Equal(m.LastReady) {
		t.Fatalf("%s", b)
	}
}

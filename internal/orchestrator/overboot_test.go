package orchestrator

import (
	"fmt"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

// Production 2026-11-12 13:23-13:25: three queued sessions, eight
// creates. Every waiting request plans on every tick; machines being
// created (o.creating) must count against the queued demand so that the
// overdue requests do not get a new machine per tick while theirs boot.
func TestRegressionQueuedDemandCountsCreatingMachines(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) {
		c.QueueTimeout = 10 * time.Minute
		c.Policy.CostOptimization = 0.2
	})
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range 3 {
		id := fmt.Sprintf("t%d", i)
		o.queue[id] = scheduler.Task{ID: id, EnvHash: "env1env1env1env1", RepoID: "r", Request: scheduler.Resources{CPU: 2, Memory: 2 * gib},
			Since: o.now().Add(-5*time.Minute + time.Duration(i)*time.Second)}
	}
	creates := 0
	for tick := range 10 {
		acts := o.planLocked()
		o.markLocked(acts)
		for _, a := range acts {
			if a.Kind == scheduler.Create || a.Kind == scheduler.Start {
				creates++
				t.Logf("tick %d: %s %s: %s", tick, a.Kind, a.Size.Name, a.Reason)
			}
		}
	}
	// m4 (4 GiB) takes two of the 2 GiB requests: 2 machines, never more
	// than one per request.
	if creates < 1 || creates > 2 || len(o.creating) != creates {
		t.Fatalf("3 overdue requests over 10 ticks: %d creates (%d creating), want 1..2", creates, len(o.creating))
	}
}

package orchestrator

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/scheduler"
)

// TestLabelsInEventsAndQueue: request labels are cleaned and recorded in
// task_placed and task_end, and waiting requests list them in Queue.
func TestLabelsInEventsAndQueue(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) { c.Policy.MaxMachines = 1; c.QueueTimeout = 3 * time.Second })
	ctx := context.Background()
	lbl := map[string]string{"agent": "codex", "worktree": "/w/a", "Bad Key": "x", "branch": "main"}
	var first Lease
	for i := 0; i < 2; i++ {
		r := req("r1")
		r.Labels = lbl
		l, err := o.Acquire(ctx, r, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		f.startSession(l.MachineID, l.ID, 2, 2*gib)
		first = l
	}
	// A third request waits for capacity.
	done := make(chan error, 1)
	go func() {
		r := req("r1")
		r.Labels = map[string]string{"agent": "claude-code", "worktree": "/w/b"}
		_, err := o.Acquire(ctx, r, io.Discard)
		done <- err
	}()
	var q []QueuedTask
	for i := 0; i < 100 && len(q) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		q = o.Queue()
	}
	if len(q) != 1 || q[0].Labels["agent"] != "claude-code" || q[0].Labels["worktree"] != "/w/b" || q[0].RepoID != "r1" {
		t.Fatalf("queue %+v", q)
	}
	<-done
	o.Finish(ctx, first.ID, "k", &scheduler.Usage{Wall: time.Second, CPUSeconds: 1})
	evs := readEvents(t, o)
	placed := byType(evs, events.TaskPlaced)
	if len(placed) != 3 || placed[0].Labels["agent"] != "codex" || placed[0].Labels["branch"] != "main" {
		t.Fatalf("task_placed labels %+v", placed)
	}
	if _, bad := placed[0].Labels["Bad Key"]; bad {
		t.Fatalf("invalid label key kept: %v", placed[0].Labels)
	}
	if placed[2].Labels["agent"] != "claude-code" {
		t.Fatalf("timed out request labels %+v", placed[2])
	}
	end := byType(evs, events.TaskEnd)
	if len(end) != 1 || end[0].Labels["worktree"] != "/w/a" {
		t.Fatalf("task_end labels %+v", end)
	}
	if len(o.Queue()) != 0 {
		t.Fatal("queue not emptied")
	}
}

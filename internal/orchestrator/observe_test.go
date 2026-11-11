package orchestrator

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/tracing"
	"github.com/traumventures/offshore/internal/tracing/tracingtest"
)

func readEvents(t *testing.T, o *Orchestrator) []events.Event {
	t.Helper()
	evs, err := events.Read(o.Events().Dir(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func byType(evs []events.Event, typ string) []events.Event {
	var out []events.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestAcquireSpansAndEvents: an acquire continues the caller's trace, the
// machine creation it triggers is a linked root span, and the event log
// records placement, machine lifecycle and the task's usage.
func TestAcquireSpansAndEvents(t *testing.T) {
	rec := tracingtest.Record(t)
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) { c.Sizes[0].PricePerHour = 0.5 })
	ctx, root := tracing.Start(context.Background(), "test.root")
	r := req("r1")
	r.Key = "r1/cargo/build"
	l, err := o.Acquire(ctx, r, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	root.End()
	o.Finish(ctx, l.ID, r.Key, &scheduler.Usage{PeakMemory: 1 << 30, CPUSeconds: 12, Wall: 10 * time.Second, Warm: true,
		Cache: map[string]int64{"sccache_hits": 2}})
	o.Release(l.ID)
	acq := tracingtest.Find(rec, "pool.acquire")
	if acq == nil || acq.Parent().SpanID() != root.SpanContext().SpanID() || acq.SpanContext().TraceID() != root.SpanContext().TraceID() {
		t.Fatalf("pool.acquire must be a child of the caller:\n%s", tracingtest.Tree(rec, root.SpanContext().TraceID()))
	}
	attrs := map[string]string{}
	for _, kv := range acq.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["offshore.placement"] != "assign" || attrs["offshore.machine"] != l.MachineID || attrs["offshore.size"] != "m4" || attrs["offshore.price_per_hour"] != "0.5" {
		t.Fatalf("acquire attributes %v", attrs)
	}
	var create = tracingtest.Find(rec, "pool.create")
	for i := 0; create == nil && i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		create = tracingtest.Find(rec, "pool.create")
	}
	if create == nil || create.SpanContext().TraceID() == root.SpanContext().TraceID() || len(create.Links()) != 1 ||
		create.Links()[0].SpanContext.TraceID() != root.SpanContext().TraceID() {
		t.Fatalf("pool.create must be a new root linked to the request: %+v", create)
	}
	evs := readEvents(t, o)
	placed := byType(evs, events.TaskPlaced)
	if len(placed) != 1 || placed[0].Placement != "assign" || placed[0].Machine != l.MachineID || placed[0].Reserve == nil ||
		placed[0].Need.Memory != 2*gib || placed[0].TraceID != root.SpanContext().TraceID().String() || placed[0].Arrival.IsZero() {
		t.Fatalf("task_placed %+v", placed)
	}
	end := byType(evs, events.TaskEnd)
	if len(end) != 1 || end[0].Task != l.ID || end[0].Usage.PeakMemory != 1<<30 || end[0].Usage.Cache["sccache_hits"] != 2 ||
		end[0].Repo != "r1" || end[0].Price != 0.5 || end[0].Reserve == nil || end[0].Capacity == nil || end[0].Capacity.CPU != 4 {
		t.Fatalf("task_end %+v", end)
	}
	cr := byType(evs, events.MachineCreate)
	if len(cr) != 1 || cr[0].Machine != l.MachineID || cr[0].Size != "m4" || cr[0].Price != 0.5 || cr[0].Capacity == nil {
		t.Fatalf("machine_create %+v", cr)
	}
	if rd := byType(evs, events.MachineReady); len(rd) != 1 || rd[0].Machine != l.MachineID {
		t.Fatalf("machine_ready %+v", rd)
	}
	// A finished task is reported once.
	o.Finish(ctx, l.ID, r.Key, nil)
	if n := len(byType(readEvents(t, o), events.TaskEnd)); n != 1 {
		t.Fatalf("task_end %d times", n)
	}
}

func TestReconcileActionEvents(t *testing.T) {
	rec := tracingtest.Record(t)
	f := newFake()
	o := newTestOrch(t, f, nil)
	l, err := o.Acquire(context.Background(), req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o.Release(l.ID)
	f.mu.Lock()
	f.machines[l.MachineID].ExpiresAt = time.Now().Add(-time.Second)
	f.mu.Unlock()
	if err := o.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := byType(readEvents(t, o), events.MachineDestroy)
	if len(d) != 1 || d[0].Machine != l.MachineID || d[0].Reason != "expired" {
		t.Fatalf("machine_destroy %+v", d)
	}
	rs := tracingtest.Find(rec, "orchestrator.reconcile")
	ds := tracingtest.Find(rec, "pool.destroy")
	if rs == nil || ds == nil || ds.Parent().SpanID() != rs.SpanContext().SpanID() {
		t.Fatalf("pool.destroy must be a child of orchestrator.reconcile")
	}
}

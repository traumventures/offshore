package orchestrator

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/proto"
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

// TestVMEvents: samples log the cache file system activity since the
// previous sample as page_fault, page_in and reclaim events.
func TestVMEvents(t *testing.T) {
	o := newTestOrch(t, newFake(), nil)
	m := &Machine{Status: &proto.Status{Ready: true, VM: &proto.VMStats{Faults: 10, PageInBytes: 100}}}
	o.mu.Lock()
	m.StatusAt = time.Now()
	o.sampleLocked("m1", m)
	m.Status = &proto.Status{Ready: true, VM: &proto.VMStats{Faults: 15, PageInBytes: 700, ReclaimBytes: 50}}
	m.StatusAt = m.StatusAt.Add(events.SampleInterval)
	o.sampleLocked("m1", m)
	o.mu.Unlock()
	evs := readEvents(t, o)
	if f := byType(evs, events.PageFault); len(f) != 1 || f[0].Pages != 5 {
		t.Fatalf("page_fault %+v", f)
	}
	if p := byType(evs, events.PageIn); len(p) != 1 || p[0].Bytes != 600 {
		t.Fatalf("page_in %+v", p)
	}
	if r := byType(evs, events.Reclaim); len(r) != 1 || r[0].Bytes != 50 {
		t.Fatalf("reclaim %+v", r)
	}
	if s := byType(evs, events.MachineSample); len(s) != 2 || s[1].VM == nil {
		t.Fatalf("samples %+v", s)
	}
}

// A session's refaulted bytes become thrash seconds with its size's tier
// stack (and the size's local capacity) before the estimator learns from
// the usage; reported thrash seconds are kept.
func TestFinishDerivesThrashSeconds(t *testing.T) {
	st := scheduler.StackOf(20*gib, medium.Default("aws-ec2", medium.Local), medium.Default("aws-ec2", medium.Store))
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) { c.Sizes[0].Stack = st })
	ctx := context.Background()
	l, err := o.Acquire(ctx, req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	u := &scheduler.Usage{Wall: 20 * time.Minute, WorkingSetBytes: 40 * gib, RefaultBytes: 60 * gib}
	o.Finish(ctx, l.ID, "r1/cargo/build", u)
	o.Release(l.ID)
	want := scheduler.RefaultTime(st, 60*gib).Seconds()
	if u.ThrashSeconds != want || u.LocalCapacity != 20*gib {
		t.Fatalf("thrash %v (want %v), capacity %d", u.ThrashSeconds, want, u.LocalCapacity)
	}
	end := byType(readEvents(t, o), events.TaskEnd)
	if len(end) != 1 || end[0].Usage.ThrashSeconds != want || end[0].Usage.RefaultBytes != 60*gib {
		t.Fatalf("task_end %+v", end)
	}
	reported := scheduler.Usage{RefaultBytes: gib, ThrashSeconds: 7, LocalCapacity: 30 * gib}
	o.thrashUsage("fake", o.cfg.Sizes[0].Name, &reported)
	if reported.ThrashSeconds != 7 || reported.LocalCapacity != 30*gib {
		t.Fatalf("reported values must be kept: %+v", reported)
	}
}

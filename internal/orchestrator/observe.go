package orchestrator

import (
	"context"
	"github.com/traumventures/cloudsourced/internal/labels"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/scheduler"
	"github.com/traumventures/cloudsourced/internal/tracing"
)

// observer records the analysis event log and holds what Finish needs to
// describe a finished task (under its own lock).
type observer struct {
	log *events.Log

	mu    sync.Mutex
	tasks map[string]taskInfo // lease id -> placement
}

// taskInfo is a placed task, kept until it finishes (or for a day).
type taskInfo struct {
	ev events.Event // the task_placed event
}

func newObserver(stateDir string, retention time.Duration) *observer {
	return &observer{log: events.Open(stateDir, retention), tasks: map[string]taskInfo{}}
}

// Events returns the analysis event log.
func (o *Orchestrator) Events() *events.Log { return o.obs.log }

// SetEventRetention changes how long event files are kept.
func (o *Orchestrator) SetEventRetention(d time.Duration) { o.obs.log.SetRetention(d) }

// machineEvent describes machine id from the orchestrator's state.
func (o *Orchestrator) machineEvent(typ, id string) events.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.machineEventLocked(typ, id)
}

func (o *Orchestrator) machineEventLocked(typ, id string) events.Event {
	ev := events.Event{Type: typ, Machine: id}
	if m, ok := o.machines[id]; ok {
		size := m.Labels[provider.LabelSize]
		ev.Provider, ev.Size, ev.Env = m.Provider, size, m.EnvHash
		ev.Price = o.priceLocked(m.Provider, size)
		if m.CPU > 0 || m.Memory > 0 {
			ev.Capacity = &scheduler.Resources{CPU: m.CPU, Memory: m.Memory}
		}
	} else if c, ok := o.creating[id]; ok {
		ev.Provider, ev.Size, ev.Env, ev.Price = c.prov, c.size.Name, c.env, c.size.PricePerHour
		if c.size.Capacity.CPU > 0 {
			cp := c.size.Capacity
			ev.Capacity = &cp
		}
	}
	return ev
}

// machineSpan starts the span of a pool action. Creation and start are
// background work: new roots linked to the triggering request.
func machineSpan(ctx context.Context, action string, ev events.Event) (context.Context, trace.Span) {
	attrs := trace.WithAttributes(
		attribute.String("cloudsourced.machine", ev.Machine),
		attribute.String("cloudsourced.provider", ev.Provider),
		attribute.String("cloudsourced.size", ev.Size),
		attribute.Float64("cloudsourced.price_per_hour", ev.Price),
	)
	if action == "create" || action == "start" {
		return tracing.StartLinked(ctx, "pool."+action, attrs)
	}
	return tracing.Start(ctx, "pool."+action, attrs)
}

// traced runs a pool action with its span and event. Creation and start
// are recorded when requested (capacity, and cost, begin), followed by
// machine_ready from waitReady; stop and destroy when done, with the
// provider latency.
func (o *Orchestrator) traced(ctx context.Context, a scheduler.Action, fn func(ctx context.Context)) {
	typ := map[scheduler.ActionKind]string{
		scheduler.Create: events.MachineCreate, scheduler.Start: events.MachineStart,
		scheduler.Stop: events.MachineStop, scheduler.Destroy: events.MachineDestroy,
	}[a.Kind]
	ev := o.machineEvent(typ, a.Machine)
	ev.Reason = a.Reason
	if a.Kind == scheduler.Create {
		ev.Env, ev.Provider, ev.Size, ev.Price = a.EnvHash, a.Size.Provider, a.Size.Name, a.Size.PricePerHour
		cp := a.Size.Capacity
		ev.Capacity = &cp
	}
	ctx, span := machineSpan(ctx, a.Kind.String(), ev)
	span.SetAttributes(attribute.String("cloudsourced.reason", a.Reason))
	ev.TraceID = tracing.TraceID(ctx)
	start := time.Now()
	if a.Kind == scheduler.Create || a.Kind == scheduler.Start {
		ev.Time = start
		o.obs.log.Append(ev)
	}
	fn(ctx)
	ms := time.Since(start).Milliseconds()
	span.SetAttributes(attribute.Int64("cloudsourced.action_ms", ms))
	if a.Kind == scheduler.Stop || a.Kind == scheduler.Destroy {
		ev.Time, ev.LatencyMs = time.Time{}, ms
		o.obs.log.Append(ev)
	}
	span.End()
}

// readyEvent records that a machine's server became ready, with the time
// since its creation or start was requested.
func (o *Orchestrator) readyEvent(ctx context.Context, id string) {
	o.mu.Lock()
	ev := o.machineEventLocked(events.MachineReady, id)
	if c, ok := o.creating[id]; ok {
		ev.LatencyMs = o.now().Sub(c.at).Milliseconds()
	}
	o.mu.Unlock()
	ev.TraceID = tracing.TraceID(ctx)
	o.obs.log.Append(ev)
	trace.SpanFromContext(ctx).AddEvent("machine ready", trace.WithAttributes(attribute.Int64("cloudsourced.ready_ms", ev.LatencyMs)))
}

// markEventsLocked records drain decisions (no provider call).
func (o *Orchestrator) markEventsLocked(a scheduler.Action) {
	typ := ""
	switch a.Kind {
	case scheduler.Drain:
		typ = events.MachineDrain
	case scheduler.Undrain:
		typ = events.MachineUndrain
	default:
		return
	}
	ev := o.machineEventLocked(typ, a.Machine)
	ev.Reason = a.Reason
	o.obs.log.Append(ev)
}

// stateEventLocked records a state change observed from the provider.
func (o *Orchestrator) stateEventLocked(id string, state provider.State) {
	ev := o.machineEventLocked(events.MachineState, id)
	ev.State = string(state)
	o.obs.log.Append(ev)
}

// startAcquire starts the span of a capacity request.
func startAcquire(ctx context.Context, req Request) (context.Context, trace.Span) {
	return tracing.Start(ctx, "pool.acquire", trace.WithAttributes(
		attribute.String("cloudsourced.repo_id", req.RepoID),
		attribute.String("cloudsourced.env_hash", req.EnvHash),
		attribute.Float64("cloudsourced.need.cpu", req.Need.CPU),
		attribute.Int64("cloudsourced.need.memory", req.Need.Memory),
	))
}

// endAcquire records the placement of a request (event and span).
func (o *Orchestrator) endAcquire(ctx context.Context, span trace.Span, req Request, arrival time.Time, lease Lease, result string, err error) {
	wait := time.Since(arrival)
	kind := result
	if kind == "assigned" {
		kind = "assign"
	} else if kind == "rejected" {
		kind = "reject"
	}
	ev := events.Event{
		Type: events.TaskPlaced, Task: lease.ID, Repo: req.RepoID, Env: req.EnvHash, Key: req.Key,
		Arrival: arrival.UTC(), WaitMs: wait.Milliseconds(), Placement: kind, Reason: lease.Reason,
		Machine: lease.MachineID, Provider: lease.Provider, Size: lease.Size, Price: lease.PricePerHour,
		TraceID: tracing.TraceID(ctx), Labels: labels.Clean(req.Labels),
	}
	need, limit := req.Need, req.Limit
	ev.Need = &need
	if limit.CPU > 0 || limit.Memory > 0 {
		ev.Limit = &limit
	}
	if err != nil {
		ev.Error = err.Error()
	} else {
		res := lease.Grant.Reserve
		ev.Reserve = &res
		o.mu.Lock()
		if m, ok := o.machines[lease.MachineID]; ok {
			ev.Capacity = &scheduler.Resources{CPU: m.CPU, Memory: m.Memory}
		}
		o.mu.Unlock()
	}
	o.obs.log.Append(ev)
	if err == nil {
		o.obs.mu.Lock()
		now := time.Now()
		for id, t := range o.obs.tasks {
			if now.Sub(t.ev.Time) > 24*time.Hour {
				delete(o.obs.tasks, id)
			}
		}
		ev.Time = now
		o.obs.tasks[lease.ID] = taskInfo{ev: ev}
		o.obs.mu.Unlock()
	}
	span.SetAttributes(
		attribute.String("cloudsourced.placement", kind),
		attribute.String("cloudsourced.placement_reason", ev.Reason),
		attribute.Int64("cloudsourced.queue_wait_ms", ev.WaitMs),
		attribute.String("cloudsourced.machine", lease.MachineID),
		attribute.String("cloudsourced.provider", lease.Provider),
		attribute.String("cloudsourced.size", lease.Size),
		attribute.Float64("cloudsourced.price_per_hour", lease.PricePerHour),
		attribute.Float64("cloudsourced.reserve.cpu", lease.Grant.Reserve.CPU),
		attribute.Int64("cloudsourced.reserve.memory", lease.Grant.Reserve.Memory),
	)
	tracing.End(span, err)
}

// Finish records the end of a task with the usage its session reported.
// Tasks without usage (never started) are only released.
func (o *Orchestrator) Finish(ctx context.Context, leaseID, key string, u *scheduler.Usage) {
	o.obs.mu.Lock()
	t, ok := o.obs.tasks[leaseID]
	delete(o.obs.tasks, leaseID)
	o.obs.mu.Unlock()
	if u == nil {
		return
	}
	ev := t.ev
	if !ok {
		ev = events.Event{Task: leaseID, Key: key}
	}
	ev.Type, ev.Time, ev.Error = events.TaskEnd, time.Time{}, ""
	if key != "" {
		ev.Key = key
	}
	usage := *u
	ev.Usage = &usage
	if tid := tracing.TraceID(ctx); tid != "" {
		ev.TraceID = tid
	}
	o.obs.log.Append(ev)
}

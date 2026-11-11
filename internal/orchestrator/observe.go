package orchestrator

import (
	"context"
	"github.com/traumventures/offshore/internal/labels"
	"math"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/tracing"
)

// observer records the analysis event log and holds what Finish needs to
// describe a finished task (under its own lock).
type observer struct {
	log *events.Log
	// refetch calibrates the scheduler's refetch factor of thrashing
	// builds from the cache file system events (Pool.Refetch).
	refetch *events.Refetch

	mu    sync.Mutex
	tasks map[string]taskInfo // lease id -> placement
	// series are the recent samples of running sessions (lease id).
	series map[string]*sessionSeries
}

// taskInfo is a placed task, kept until it finishes (or for a day).
type taskInfo struct {
	ev events.Event // the task_placed event
}

func newObserver(stateDir string, retention time.Duration) *observer {
	o := &observer{log: events.Open(stateDir, retention), tasks: map[string]taskInfo{}, series: map[string]*sessionSeries{}, refetch: events.NewRefetch()}
	// Calibrate from the last day of events, then from every new one.
	now := time.Now()
	if evs, err := events.Read(stateDir, now.Add(-refetchHistory), now); err == nil {
		for _, ev := range evs {
			o.refetch.Observe(ev)
		}
	}
	o.log.Tap(o.refetch.Observe)
	return o
}

// refetchHistory is how far back the refetch calibration replays the
// event log at start.
const refetchHistory = 24 * time.Hour

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
		ev.Price = o.machinePriceLocked(m)
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
		attribute.String("offshore.machine", ev.Machine),
		attribute.String("offshore.provider", ev.Provider),
		attribute.String("offshore.size", ev.Size),
		attribute.Float64("offshore.price_per_hour", ev.Price),
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
	ev.Reason, ev.Decision = a.Reason, a.Decision
	if a.Kind == scheduler.Create {
		ev.Env, ev.Provider, ev.Size, ev.Price = a.EnvHash, a.Size.Provider, a.Size.Name, a.Size.PricePerHour
		cp := a.Size.Capacity
		ev.Capacity = &cp
	}
	ctx, span := machineSpan(ctx, a.Kind.String(), ev)
	span.SetAttributes(attribute.String("offshore.reason", a.Reason))
	ev.TraceID = tracing.TraceID(ctx)
	start := time.Now()
	if a.Kind == scheduler.Create || a.Kind == scheduler.Start {
		ev.Time = start
		o.obs.log.Append(ev)
	}
	fn(ctx)
	ms := time.Since(start).Milliseconds()
	span.SetAttributes(attribute.Int64("offshore.action_ms", ms))
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
	trace.SpanFromContext(ctx).AddEvent("machine ready", trace.WithAttributes(attribute.Int64("offshore.ready_ms", ev.LatencyMs)))
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
		attribute.String("offshore.repo_id", req.RepoID),
		attribute.String("offshore.env_hash", req.EnvHash),
		attribute.Float64("offshore.need.cpu", req.Need.CPU),
		attribute.Int64("offshore.need.memory", req.Need.Memory),
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
		Decision: lease.Decision, Estimate: req.Estimate, Predicted: lease.Predicted,
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
		attribute.String("offshore.placement", kind),
		attribute.String("offshore.placement_reason", ev.Reason),
		attribute.Int64("offshore.queue_wait_ms", ev.WaitMs),
		attribute.String("offshore.machine", lease.MachineID),
		attribute.String("offshore.provider", lease.Provider),
		attribute.String("offshore.size", lease.Size),
		attribute.Float64("offshore.price_per_hour", lease.PricePerHour),
		attribute.Float64("offshore.reserve.cpu", lease.Grant.Reserve.CPU),
		attribute.Int64("offshore.reserve.memory", lease.Grant.Reserve.Memory),
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
	ev.Decision = nil // logged on task_placed
	if ev.Predicted != nil {
		pr := *ev.Predicted
		pr.Compare(*u)
		ev.Predicted = &pr
	}
	if key != "" {
		ev.Key = key
	}
	usage := *u
	if usage.Provider == "" {
		usage.Provider = ev.Provider
	}
	o.thrashUsage(ev.Provider, ev.Size, &usage)
	*u = usage // the estimator learns cold durations per provider
	ev.Usage = &usage
	if tid := tracing.TraceID(ctx); tid != "" {
		ev.TraceID = tid
	}
	o.obs.log.Append(ev)
}

// thrashUsage derives the session's thrash time from its refaulted bytes
// with the tier stack of its size (unless the worker reported it), so the
// estimator learns base run times without the thrashing, and fills in the
// local capacity from the size when the worker did not report it.
func (o *Orchestrator) thrashUsage(prov, size string, u *scheduler.Usage) {
	if u.RefaultBytes <= 0 || (u.ThrashSeconds > 0 && u.LocalCapacity > 0) {
		return
	}
	o.mu.Lock()
	sizes := o.measuredSizes()
	o.mu.Unlock()
	for _, s := range sizes {
		if s.Provider != prov || s.Name != size {
			continue
		}
		if u.ThrashSeconds <= 0 {
			u.ThrashSeconds = scheduler.RefaultTime(s.Stack, u.RefaultBytes).Seconds()
		}
		if u.LocalCapacity <= 0 {
			u.LocalCapacity = s.Stack.Capacity
		}
		return
	}
	if u.ThrashSeconds <= 0 { // unknown size: the default bandwidth
		u.ThrashSeconds = scheduler.RefaultTime(medium.Stack{}, u.RefaultBytes).Seconds()
	}
}

// predicted is what a placement expects of the run: the chosen option's
// run time and the estimate's cores and peak memory (the need when the
// request carries no estimate).
func predicted(pl scheduler.Placement, req Request) *events.Predicted {
	p := &events.Predicted{RunSeconds: pl.Run.Seconds(), ExpectedSeconds: pl.Expected.Seconds(),
		Cores: req.Need.CPU, PeakMemory: req.Need.Memory}
	if e := req.Estimate; e != nil {
		if e.Cores > 0 {
			p.Cores = e.Cores
		}
		if e.PeakMemory > 0 {
			p.PeakMemory = e.PeakMemory
		}
	}
	if p.RunSeconds == 0 && p.ExpectedSeconds == 0 && p.Cores == 0 && p.PeakMemory == 0 {
		return nil
	}
	return p
}

// waitingLocked describes what a waiting task waits for: the busy or
// starting machine its placement chose to wait for, else the machines of
// its environment on their way (ETA: the earliest expected ready time).
func (o *Orchestrator) waitingLocked(env string, pl scheduler.Placement) events.Waiting {
	if c, ok := pl.Decision.ChosenOption(); ok && c.Kind == scheduler.OptWaitMachine {
		return events.Waiting{For: c.Kind, Machine: c.Machine, Provider: c.Provider, Size: c.Size, ETASeconds: c.WaitSeconds}
	}
	w := events.Waiting{For: "none"}
	now := o.now()
	eta := time.Duration(-1)
	for _, c := range o.creating {
		if c.env != env {
			continue
		}
		w.For = scheduler.OptWaitCapacity
		w.Creating++
		h := o.health[c.prov]
		lat := h.boot
		if c.started {
			lat = h.start
			if lat <= 0 {
				lat = scheduler.DefaultStart
			}
		} else if lat <= 0 {
			lat = scheduler.DefaultBoot
		}
		left := max(c.at.Add(lat).Sub(now), 0)
		if eta < 0 || left < eta {
			eta, w.Provider, w.Size = left, c.prov, c.size.Name
		}
	}
	if eta > 0 {
		w.ETASeconds = math.Round(eta.Seconds()*10) / 10
	}
	return w
}

// waitingEvent records that a queued task still waits (task_waiting).
func (o *Orchestrator) waitingEvent(ctx context.Context, t scheduler.Task, req Request, arrival time.Time, reason string, w events.Waiting) {
	need := t.Request
	o.obs.log.Append(events.Event{Type: events.TaskWaiting, Task: t.ID, Repo: t.RepoID, Env: t.EnvHash, Key: t.Key,
		Arrival: arrival.UTC(), WaitMs: o.now().Sub(arrival).Milliseconds(), Need: &need, Reason: reason, Waiting: &w,
		Labels: labels.Clean(req.Labels), TraceID: tracing.TraceID(ctx)})
}

// waitingInterval is how often a queued task emits task_waiting (a var
// for tests).
var waitingInterval = events.WaitingInterval

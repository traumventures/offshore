package orchestrator

import (
	"context"
	"time"

	"sort"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/metrics"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// orchMetrics are the orchestrator's own metrics; pool state (machines,
// queue, cost) and the workers' metrics are collected at gather
// time.
type orchMetrics struct {
	reg        *metrics.Registry
	acquire    metrics.HistogramVec
	placements metrics.CounterVec
	actions    metrics.CounterVec
	provErrs   metrics.CounterVec
	reconcile  metrics.HistogramVec
}

func newOrchMetrics(o *Orchestrator) *orchMetrics {
	r := metrics.NewRegistry()
	m := &orchMetrics{
		reg:        r,
		acquire:    r.Histogram("offshore_acquire_wait_seconds", "Time from a capacity request to its lease (or failure).", metrics.DurationBuckets, "result"),
		placements: r.Counter("offshore_placements_total", "Placement decisions by kind (assign, queue, reject, timeout).", "kind"),
		actions:    r.Counter("offshore_pool_actions_total", "Pool actions decided by policy and invariants (create, start, stop, destroy, drain, undrain).", "action"),
		provErrs:   r.Counter("offshore_provider_errors_total", "Failed provider API calls.", "provider", "op"),
		reconcile:  r.Histogram("offshore_reconcile_duration_seconds", "Duration of maintenance passes.", []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}, "result"),
	}
	r.Collect(o.collect)
	r.Collect(o.collectEvents)
	return m
}

// Metrics returns the orchestrator's registry (other pool components add
// their metrics to it).
func (o *Orchestrator) Metrics() *metrics.Registry { return o.metrics.reg }

// provName returns the instance name of a provider (the label machines
// carry), falling back to its backend name.
func (o *Orchestrator) provName(p provider.Provider) string {
	for name, q := range o.provs {
		if q == p {
			return name
		}
	}
	return p.Name()
}

// provErr counts a failed provider call and returns err.
func (o *Orchestrator) provErr(prov, op string, err error) error {
	if err != nil {
		o.metrics.provErrs.Inc(prov, op)
	}
	return err
}

func actionName(k scheduler.ActionKind) string {
	switch k {
	case scheduler.Create:
		return "create"
	case scheduler.Start:
		return "start"
	case scheduler.Stop:
		return "stop"
	case scheduler.Destroy:
		return "destroy"
	case scheduler.Drain:
		return "drain"
	case scheduler.Undrain:
		return "undrain"
	}
	return "other"
}

// collect derives pool gauges from the current state and republishes the
// last probed metrics of every running machine.
func (o *Orchestrator) collect() []metrics.Family {
	o.mu.Lock()
	defer o.mu.Unlock()
	type key struct{ prov, size, state string }
	counts := map[key]int{}
	cost := map[string]float64{}
	var machineFams [][]metrics.Family
	for id, m := range o.machines {
		size := m.Labels[provider.LabelSize]
		state := string(m.State)
		if state == "" {
			state = "unknown"
		}
		if m.State == provider.Running && (m.Status == nil || !m.Status.Ready) {
			state = "provisioning"
		}
		if m.Draining || (m.Status != nil && m.Status.Draining) {
			state = "draining"
		}
		counts[key{m.Provider, size, state}]++
		if m.State == provider.Running || m.State == provider.Starting {
			cost[m.Provider] += o.machinePriceLocked(m)
		}
		if m.Status != nil && len(m.Status.Metrics) > 0 {
			machineFams = append(machineFams, metrics.WithLabels(m.Status.Metrics,
				metrics.Label{Name: "machine", Value: id}, metrics.Label{Name: "provider", Value: m.Provider}, metrics.Label{Name: "size", Value: size}))
		}
	}
	for id, c := range o.creating {
		if _, known := o.machines[id]; known {
			continue
		}
		counts[key{c.prov, c.size.Name, "creating"}]++
	}
	var ms []metrics.Sample
	for k, n := range counts {
		ms = append(ms, metrics.Sample{Labels: metrics.L("provider", k.prov, "size", k.size, "state", k.state), Value: float64(n)})
	}
	var cs []metrics.Sample
	for p := range o.provs {
		cs = append(cs, metrics.Sample{Labels: metrics.L("provider", p), Value: cost[p]})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Labels[0].Value < cs[j].Labels[0].Value })
	out := []metrics.Family{
		metrics.GaugeFamily("offshore_machines", "Machines by provider, size and state (creating, provisioning, running, draining, stopped, ...).", ms...),
		metrics.GaugeFamily("offshore_queue_length", "Sessions waiting for capacity.", metrics.Sample{Value: float64(len(o.queue))}),
		metrics.GaugeFamily("offshore_cost_per_hour", "Hourly price of machines that are up, by provider (pool size prices).", cs...),
		metrics.GaugeFamily("offshore_environments_recent", "Environments used in the last 24 hours (warm pool candidates).", metrics.Sample{Value: float64(len(o.recentEnvs))}),
	}
	return metrics.Merge(append([][]metrics.Family{out}, machineFams...)...)
}

// RefreshIfStale refreshes the pool state when it is older than maxAge.
func (o *Orchestrator) RefreshIfStale(ctx context.Context, maxAge time.Duration) error {
	o.mu.Lock()
	fresh := time.Since(o.refreshed) < maxAge
	o.mu.Unlock()
	if fresh {
		return nil
	}
	return o.Refresh(ctx)
}

// collectEvents reports the health of the analysis event log: events
// dropped by failed writes (disk full, size cap) and unparsable lines
// skipped by readers.
func (o *Orchestrator) collectEvents() []metrics.Family {
	var dropped int64
	if o.obs != nil {
		dropped = o.obs.log.Dropped()
	}
	return []metrics.Family{
		{Name: "offshore_events_dropped_total", Help: "Analysis events dropped (write failure, disk full, size cap).", Type: metrics.Counter,
			Samples: []metrics.Sample{{Value: float64(dropped)}}},
		{Name: "offshore_events_skipped_lines_total", Help: "Unparsable event log lines skipped by readers.", Type: metrics.Counter,
			Samples: []metrics.Sample{{Value: float64(events.SkippedLines())}}},
	}
}

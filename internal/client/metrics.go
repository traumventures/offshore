package client

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/traumventures/cloudsourced/internal/fleet"
	"github.com/traumventures/cloudsourced/internal/metrics"
	"github.com/traumventures/cloudsourced/internal/runlog"
)

// daemonMetrics are the local daemon's own metrics.
type daemonMetrics struct {
	reg      *metrics.Registry
	requests metrics.CounterVec
	prepare  metrics.HistogramVec
	runs     metrics.CounterVec
}

func newDaemonMetrics(d *Daemon) *daemonMetrics {
	r := metrics.NewRegistry()
	m := &daemonMetrics{
		reg:      r,
		requests: r.Counter("cloudsourced_daemon_requests_total", "Daemon RPC requests by operation and result.", "op", "result"),
		prepare:  r.Histogram("cloudsourced_daemon_prepare_seconds", "Time to prepare a remote run (environment, sync, placement, attach).", metrics.DurationBuckets, "result"),
		runs:     r.Counter("cloudsourced_daemon_runs_total", "Proxied runs by tool and mode (remote, local, failed).", "tool", "mode"),
	}
	r.Collect(func() []metrics.Family {
		d.mu.Lock()
		roots, conns := len(d.roots), len(d.conns)
		d.mu.Unlock()
		return []metrics.Family{
			metrics.GaugeFamily("cloudsourced_daemon_roots", "Watched source roots.", metrics.Sample{Value: float64(roots)}),
			metrics.GaugeFamily("cloudsourced_daemon_machine_connections", "Open connections to machines.", metrics.Sample{Value: float64(conns)}),
		}
	})
	return m
}

var knownOps = map[string]bool{"ping": true, "prepare": true, "release": true, "record": true, "sync": true, "status": true,
	"reconcile": true, "down": true, "shutdown": true, "metrics": true}

func (m *daemonMetrics) request(op string, start time.Time, err error) {
	if !knownOps[op] {
		op = "other"
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.requests.Inc(op, result)
	if op == "prepare" {
		m.prepare.Observe(time.Since(start).Seconds(), result)
	}
}

func (m *daemonMetrics) run(r *runlog.Run) {
	if r == nil || r.Tool == "" || len(r.Tool) > 64 {
		return
	}
	m.runs.Inc(r.Tool, r.Mode)
}

// Metrics is the result of the "metrics" op.
type Metrics struct {
	Text string `json:"text"`
}

// metricsText renders the daemon's metrics and those of its fleet: the
// embedded orchestrator (with machine metrics) or, in remote mode, the
// control plane's.
func (d *Daemon) metricsText(ctx context.Context) (*Metrics, error) {
	var b bytes.Buffer
	own := d.metrics.reg.Gather()
	if d.local != nil {
		if err := metrics.WriteText(&b, metrics.Merge(own, d.local.Metrics(ctx))); err != nil {
			return nil, err
		}
		return &Metrics{Text: b.String()}, nil
	}
	if err := metrics.WriteText(&b, own); err != nil {
		return nil, err
	}
	if src, ok := d.fleet.(fleet.MetricsSource); ok {
		var cp bytes.Buffer
		if err := src.WriteMetrics(ctx, &cp); err != nil {
			fmt.Fprintf(&b, "# control plane metrics unavailable: %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		} else {
			b.Write(cp.Bytes())
		}
	}
	return &Metrics{Text: b.String()}, nil
}

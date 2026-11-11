package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/metrics"
	"github.com/traumventures/offshore/internal/scheduler"
)

func TestMetricsAggregateMachines(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) { c.Sizes[0].PricePerHour = 0.5 })
	ctx := context.Background()
	l, err := o.Acquire(ctx, req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// The machine's server reports its own metrics in the status stream.
	srv := metrics.NewRegistry()
	srv.Gauge("offshore_worker_sessions_running", "Sessions running now.").Set(1)
	srv.Counter("offshore_worker_session_exits_total", "Exits.", "code").Inc("0")
	f.mu.Lock()
	f.status[l.MachineID].Metrics = srv.Gather()
	f.mu.Unlock()
	if err := o.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = o.Acquire(ctx, Request{EnvHash: "env1env1env1env1", Need: scheduler.Resources{CPU: 64, Memory: 1 << 40}}, io.Discard)
	if !errors.Is(err, ErrUnschedulable) || !strings.HasPrefix(err.Error(), "cannot schedule: ") {
		t.Fatalf("want rejection, got %v", err)
	}
	var b bytes.Buffer
	metrics.WriteText(&b, o.Metrics().Gather())
	text := b.String()
	for _, want := range []string{
		`offshore_machines{provider="fake",size="m4",state="running"} 1`,
		`offshore_queue_length 0`,
		`offshore_cost_per_hour{provider="fake"} 0.5`,
		`offshore_placements_total{kind="assign"} 1`,
		`offshore_placements_total{kind="queue"} 1`,
		`offshore_placements_total{kind="reject"} 1`,
		`offshore_pool_actions_total{action="create"} 1`,
		`offshore_acquire_wait_seconds_count{result="assigned"} 1`,
		`offshore_acquire_wait_seconds_count{result="rejected"} 1`,
		`offshore_worker_sessions_running{machine="` + l.MachineID + `",provider="fake",size="m4"} 1`,
		`offshore_worker_session_exits_total{machine="` + l.MachineID + `",provider="fake",size="m4",code="0"} 1`,
		"# TYPE offshore_worker_session_exits_total counter",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	metrics.WriteText(&b, o.Metrics().Gather())
	if !strings.Contains(b.String(), `offshore_reconcile_duration_seconds_count{result="ok"} 1`) {
		t.Errorf("reconcile not counted:\n%s", b.String())
	}
}

func TestMetricsProviderErrors(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	o.provErr("fake", "create", errors.New("boom"))
	o.provErr("fake", "create", nil)
	if name := o.provName(f); name != "fake" {
		t.Fatalf("provName %q", name)
	}
	var b bytes.Buffer
	metrics.WriteText(&b, o.Metrics().Gather())
	if !strings.Contains(b.String(), `offshore_provider_errors_total{provider="fake",op="create"} 1`) {
		t.Fatal(b.String())
	}
}

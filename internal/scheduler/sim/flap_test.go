package sim

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

// prodSizes is the production pool of the flapping case (AWS x4/x8/x16).
var prodSizes = []scheduler.Size{
	{Provider: "aws", Name: "x4", Capacity: scheduler.Resources{CPU: 4, Memory: 7 * gib}, PricePerHour: 0.21},
	{Provider: "aws", Name: "x8", Capacity: scheduler.Resources{CPU: 8, Memory: 15 * gib}, PricePerHour: 0.42},
	{Provider: "aws", Name: "x16", Capacity: scheduler.Resources{CPU: 16, Memory: 30 * gib}, PricePerHour: 0.84},
}

func prodRun(t *testing.T, variant string) Report {
	t.Helper()
	tr, err := Load(filepath.Join("testdata", "prod-flap.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Run(tr, Options{Name: variant, Policy: scheduler.Default{Cfg: scheduler.Config{CPUOvercommit: 2, MemoryOvercommit: 1, ExpiryMargin: 15 * time.Minute, CostOptimization: 0.5,
		QueueTimeout: 10 * time.Minute}}, Sizes: prodSizes, Estimator: scheduler.NewHistory(""),
		Boot: map[string]time.Duration{"": 80 * time.Second}, Start: map[string]time.Duration{"": 70 * time.Second}})
}

// TestRegressionDrainThenCreate replays the production evening of
// 2026-11-10 (sim export of the control plane's events): the pre-cost
// model policy drained a machine for consolidation and created or started
// one within a minute. The scheduler must not flap.
func TestRegressionDrainThenCreate(t *testing.T) {
	cur := prodRun(t, "v2")
	t.Logf("flaps %d cost %.2f p95 %v", cur.Flaps, cur.Cost, cur.WaitP95)
	if cur.Flaps != 0 || cur.Completed != cur.Jobs {
		t.Fatalf("%d flaps, %d/%d done", cur.Flaps, cur.Completed, cur.Jobs)
	}
}

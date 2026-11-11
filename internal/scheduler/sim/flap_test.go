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
	return Run(tr, Options{Name: variant, Policy: scheduler.Default{Cfg: scheduler.Config{Variant: variant, CPUOvercommit: 2, MemoryOvercommit: 1,
		MaxMachines: 4, IdleStop: 20 * time.Minute, ExpiryMargin: 15 * time.Minute, ConsolidateBelow: 0.5, CostOptimization: 0.5,
		QueueTimeout: 10 * time.Minute}}, Sizes: prodSizes, Estimator: scheduler.NewHistory(""),
		Boot: map[string]time.Duration{"": 80 * time.Second}, Start: map[string]time.Duration{"": 70 * time.Second}})
}

// TestRegressionDrainThenCreate replays the production evening of
// 2026-11-10 (sim export of the control plane's events): the pre-cost
// model policy drained a machine for consolidation and created or started
// one within a minute. The default policy must not flap.
func TestRegressionDrainThenCreate(t *testing.T) {
	old, cur := prodRun(t, scheduler.VariantBinpack), prodRun(t, scheduler.VariantDefault)
	t.Logf("binpack: flaps %d cost %.2f p95 %v; default: flaps %d cost %.2f p95 %v",
		old.Flaps, old.Cost, old.WaitP95, cur.Flaps, cur.Cost, cur.WaitP95)
	if old.Flaps == 0 {
		t.Fatalf("the scenario no longer reproduces the flapping of the old policy")
	}
	if cur.Flaps != 0 || cur.Completed != cur.Jobs {
		t.Fatalf("default policy: %d flaps, %d/%d done", cur.Flaps, cur.Completed, cur.Jobs)
	}
}

// cost_optimization = 1 is about as cheap as the binpack policy on every
// sample trace (c7a sizes and defaults as in testdata/pool.toml: static
// estimator, queue_timeout 2m, boot 60s, start 20s), and
// the default 0.5 still does not flap on the production trace.
func TestCheapestBeatsBinpack(t *testing.T) {
	sizes := []scheduler.Size{
		{Provider: "aws", Name: "c7a.large", Capacity: scheduler.Resources{CPU: 2, Memory: 4 * gib}, PricePerHour: 0.1027},
		{Provider: "aws", Name: "c7a.xlarge", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 0.2053},
		{Provider: "aws", Name: "c7a.2xlarge", Capacity: scheduler.Resources{CPU: 8, Memory: 16 * gib}, PricePerHour: 0.4106},
		{Provider: "aws", Name: "c7a.4xlarge", Capacity: scheduler.Resources{CPU: 16, Memory: 32 * gib}, PricePerHour: 0.8211},
	}
	for _, name := range []string{"team-week.json", "small-week.json", "prod-flap.json"} {
		tr, err := Load(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		run := func(variant string, r float64) Report {
			return Run(tr, Options{Policy: scheduler.Default{Cfg: scheduler.Config{Variant: variant, CPUOvercommit: 2, MemoryOvercommit: 1,
				MaxMachines: 16, IdleStop: 20 * time.Minute, ExpiryMargin: 15 * time.Minute, ConsolidateBelow: 0.5,
				CostOptimization: r, QueueTimeout: 2 * time.Minute}}, Sizes: sizes, Estimator: scheduler.Static{},
				Boot: map[string]time.Duration{"": time.Minute}, Start: map[string]time.Duration{"": 20 * time.Second}})
		}
		bin, cheap, mid := run(scheduler.VariantBinpack, 0), run(scheduler.VariantDefault, 1), run(scheduler.VariantDefault, 0.5)
		t.Logf("%s: binpack $%.2f (%d flaps), r=1 $%.2f (%d flaps), r=0.5 $%.2f (%d flaps)", name, bin.Cost, bin.Flaps, cheap.Cost, cheap.Flaps, mid.Cost, mid.Flaps)
		if cheap.Jobs-cheap.Completed > cheap.OOMs { // only OOM-killed jobs (after retries) may fail
			t.Errorf("%s: r=1 completed %d/%d (%d OOMs)", name, cheap.Completed, cheap.Jobs, cheap.OOMs)
		}
		// Binpack runs single tasks on machines with fewer cores than they
		// request (overcommit), which the default policy never does: up to
		// 10% more may be the price of that invariant.
		if cheap.Cost > 1.1*bin.Cost {
			t.Errorf("%s: cost_optimization=1 costs %.2f, binpack %.2f", name, cheap.Cost, bin.Cost)
		}
		if cheap.Cost > mid.Cost {
			t.Errorf("%s: r=1 (%.2f) more expensive than r=0.5 (%.2f)", name, cheap.Cost, mid.Cost)
		}
		if name == "prod-flap.json" && mid.Flaps != 0 {
			t.Errorf("%s: %d flaps at the default cost_optimization", name, mid.Flaps)
		}
	}
}

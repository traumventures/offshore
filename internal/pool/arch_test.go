package pool

import (
	"context"
	"testing"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// Images are built for the architecture of the size a machine is created
// with (pool.size arch), not for the provider's default platform.
func TestSizeArchSelectsImagePlatform(t *testing.T) {
	l := &Local{cfg: &config.PoolConfig{Pool: config.Pool{Sizes: []config.Size{
		{Provider: "aws", Name: "x4", Arch: "amd64"},
		{Provider: "aws", Name: "g4", Arch: "arm64"},
	}}}}
	if a := l.sizeArch("aws", "g4"); a != "arm64" {
		t.Errorf("g4: %q", a)
	}
	if a := l.sizeArch("aws", "x4"); a != "amd64" {
		t.Errorf("x4: %q", a)
	}
	if a := l.sizeArch("other", "g4"); a != "" {
		t.Errorf("unknown size: %q", a)
	}
}

type fixedEstimator struct{ r scheduler.Resources }

func (f fixedEstimator) Estimate(string, scheduler.Resources) scheduler.Resources { return f.r }
func (fixedEstimator) Observe(string, scheduler.Usage)                            {}
func (fixedEstimator) Profile(string) scheduler.Profile                           { return scheduler.Profile{} }

// A learned need above every size is capped at the largest admissible
// size instead of making the key unschedulable.
func TestEstimateCappedAtLargestSize(t *testing.T) {
	l := &Local{cfg: &config.PoolConfig{Pool: config.Pool{
		Sizes:     []config.Size{{Name: "s", CPU: 4, Memory: "7G"}, {Name: "l", CPU: 8, Memory: "15G"}},
		Scheduler: config.Scheduler{CPUOvercommit: 2, MemoryOvercommit: 1},
	}}, est: fixedEstimator{scheduler.Resources{CPU: 1.9, Memory: 16182154240}}}
	got, _ := l.Estimate(context.Background(), "k", scheduler.Resources{CPU: 5, Memory: 4 << 30})
	if got.Memory != 15<<30 || got.CPU != 1.9 {
		t.Fatalf("%+v", got)
	}
}

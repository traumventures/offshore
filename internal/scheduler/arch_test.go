package scheduler

import (
	"testing"
	"time"
)

func TestPlanCreatesOnlyMatchingArch(t *testing.T) {
	sizes := []Size{
		{Provider: "aws", Name: "c7a", Capacity: res(4, 8), PricePerHour: 0.2, Arch: "amd64"},
		{Provider: "aws", Name: "c7g", Capacity: res(4, 8), PricePerHour: 0.15, Arch: "arm64"},
	}
	q := []Task{task(2, 4)}
	for _, v := range []string{""} {
		pol := Default{Cfg: Config{}}
		// The cheaper arm64 size is never used for an amd64 environment
		// (unknown environments are amd64).
		if got := creates(pol.Plan(Pool{Now: now, Sizes: sizes, Queue: q})); len(got) != 1 || got[0] != "c7a" {
			t.Errorf("%s: amd64 env: got %v", v, got)
		}
		arm := map[string]string{"e": "arm64"}
		if got := creates(pol.Plan(Pool{Now: now, Sizes: sizes, Queue: q, EnvArch: arm})); len(got) != 1 || got[0] != "c7g" {
			t.Errorf("%s: arm64 env: got %v", v, got)
		}
	}
	// No size of the environment's architecture: the task is rejected
	// instead of waiting forever.
	pl := Default{Cfg: Config{}}.Place(Pool{Now: now, Sizes: sizes[:1], EnvArch: map[string]string{"e": "arm64"}}, task(2, 4))
	if pl.Kind != Reject {
		t.Errorf("want reject without an arm64 size, got %+v", pl)
	}
	// The warm pool creates machines of the environment's architecture.
	acts := Default{Cfg: Config{MinMachines: 1}}.Plan(Pool{Now: now, Sizes: sizes,
		Recent: map[string]time.Time{"e": now}, EnvArch: map[string]string{"e": "arm64"}})
	if got := creates(acts); len(got) != 1 || got[0] != "c7g" {
		t.Errorf("warm pool: got %v", got)
	}
}

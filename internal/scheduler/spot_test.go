package scheduler

import (
	"testing"
	"time"
)

func TestPlanPrefersSpot(t *testing.T) {
	onDemand := Size{Provider: "aws", Name: "od", Capacity: res(4, 8), PricePerHour: 0.2}
	spot := Size{Provider: "aws", Name: "spot", Capacity: res(4, 8), PricePerHour: 0.08, Spot: true, OnDemandPrice: 0.2}
	q := []Task{task(2, 4)}
	for _, v := range []string{VariantDefault, VariantClassic} {
		pol := Default{Cfg: Config{MaxMachines: 3, Variant: v}}
		// The spot discount makes the spot size cheaper.
		if got := creates(pol.Plan(Pool{Now: now, Sizes: []Size{onDemand, spot}, Queue: q})); len(got) != 1 || got[0] != "spot" {
			t.Errorf("%s: want the cheaper spot size, got %v", v, got)
		}
		// At equal (or unknown) price, spot wins the tie.
		unpriced := []Size{{Provider: "aws", Name: "od", Capacity: res(4, 8)}, {Provider: "aws", Name: "spot", Capacity: res(4, 8), Spot: true}}
		if got := creates(pol.Plan(Pool{Now: now, Sizes: unpriced, Queue: q})); len(got) != 1 || got[0] != "spot" {
			t.Errorf("%s: want spot at equal price, got %v", v, got)
		}
		// A dearer spot size (a discount too small to matter) loses.
		dear := spot
		dear.PricePerHour = 0.25
		if got := creates(pol.Plan(Pool{Now: now, Sizes: []Size{onDemand, dear}, Queue: q})); len(got) != 1 || got[0] != "od" {
			t.Errorf("%s: want the cheaper on-demand size, got %v", v, got)
		}
	}
}

func TestPlanCreatesOnlyMatchingArch(t *testing.T) {
	sizes := []Size{
		{Provider: "aws", Name: "c7a", Capacity: res(4, 8), PricePerHour: 0.2, Arch: "amd64"},
		{Provider: "aws", Name: "c7g", Capacity: res(4, 8), PricePerHour: 0.15, Arch: "arm64"},
	}
	q := []Task{task(2, 4)}
	for _, v := range []string{VariantDefault, VariantClassic} {
		pol := Default{Cfg: Config{MaxMachines: 3, Variant: v}}
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
	pl := Default{Cfg: Config{MaxMachines: 3}}.Place(Pool{Now: now, Sizes: sizes[:1], EnvArch: map[string]string{"e": "arm64"}}, task(2, 4))
	if pl.Kind != Reject {
		t.Errorf("want reject without an arm64 size, got %+v", pl)
	}
	// The warm pool creates machines of the environment's architecture.
	acts := Default{Cfg: Config{MaxMachines: 3, MinMachines: 1}}.Plan(Pool{Now: now, Sizes: sizes,
		Recent: map[string]time.Time{"e": now}, EnvArch: map[string]string{"e": "arm64"}})
	if got := creates(acts); len(got) != 1 || got[0] != "c7g" {
		t.Errorf("warm pool: got %v", got)
	}
}

func TestInterruptedMachineIsNotUndrained(t *testing.T) {
	// Queued work must create capacity elsewhere instead of undraining an
	// interrupted machine.
	m := busy("spot", res(4, 8), res(1, 2), 1)
	m.Draining, m.Interrupted = true, true
	acts := Default{Cfg: Config{MaxMachines: 3}}.Plan(Pool{Now: now, Sizes: aws, Machines: []Machine{m}, Queue: []Task{task(1, 1)}})
	got := kinds(acts)
	if len(got[Undrain]) != 0 || len(got[Create]) != 1 {
		t.Fatalf("want a new machine, got %+v", acts)
	}
}

func TestEmptyInterruptedMachineStops(t *testing.T) {
	m := machine("spot", res(4, 8), Resources{})
	m.Draining, m.Interrupted, m.IdleSince = true, true, now
	acts := Default{Cfg: Config{MaxMachines: 3, MinMachines: 1, IdleStop: time.Hour}}.Plan(Pool{Now: now, Sizes: aws, Machines: []Machine{m},
		Recent: map[string]time.Time{"e": now}})
	got := kinds(acts)
	if len(got[Stop]) != 1 || got[Stop][0] != "spot" || len(got[Create]) != 1 {
		t.Fatalf("want the interrupted machine stopped and replaced in the warm pool, got %+v", acts)
	}
}

func TestAtRiskMachineComesLast(t *testing.T) {
	risky := machine("risky", res(8, 16), res(4, 8))
	risky.AtRisk = true
	safe := machine("safe", res(8, 16), Resources{})
	pl := Default{}.Place(Pool{Now: now, Machines: []Machine{risky, safe}}, task(1, 1))
	if pl.Machine != "safe" {
		t.Fatalf("want the machine without rebalance recommendation, got %+v", pl)
	}
	// Alone it still serves.
	if pl := (Default{}).Place(Pool{Now: now, Machines: []Machine{risky}}, task(1, 1)); pl.Kind != Assign {
		t.Fatalf("at-risk machine must still accept work, got %+v", pl)
	}
}

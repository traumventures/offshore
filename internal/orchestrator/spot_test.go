package orchestrator

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// A spot machine with an interruption notice gets no new sessions; the
// request goes to a new machine.
func TestInterruptedMachineGetsNoSessions(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	ctx := context.Background()
	l1, err := o.Acquire(ctx, req("r"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o.Release(l1.ID)
	f.mu.Lock()
	f.status[l1.MachineID].Draining = true
	f.status[l1.MachineID].InterruptAt = time.Now().Add(2 * time.Minute).Unix()
	f.mu.Unlock()
	if err := o.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	l2, err := o.Acquire(ctx, req("r"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l2.MachineID == l1.MachineID {
		t.Fatal("session placed on an interrupted machine")
	}
	o.mu.Lock()
	fl := o.poolLocked(time.Now())
	o.mu.Unlock()
	for _, m := range fl.Machines {
		if m.ID == l1.MachineID && (!m.Interrupted || !m.Draining) {
			t.Fatalf("interrupted machine seen as %+v", m)
		}
	}
}

// The architecture of a request selects the size of new machines, and
// the machine spec carries the size's spot settings.
func TestArchAndSpotReachTheProvider(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) {
		c.Sizes = []scheduler.Size{
			{Provider: "fake", Name: "x86", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0.2, Arch: "amd64"},
			{Provider: "fake", Name: "arm", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0.06, Arch: "arm64",
				Spot: true, OnDemandPrice: 0.15, SpotFallback: true, InstanceTypes: []string{"c7g.xlarge", "m7g.xlarge"}},
		}
	})
	ctx := context.Background()
	r := req("r")
	r.Arch = "aarch64"
	l, err := o.Acquire(ctx, r, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	spec := f.specs[len(f.specs)-1]
	f.mu.Unlock()
	if spec.Size != "arm" || spec.Arch != "arm64" || !spec.Spot || !spec.SpotFallback || len(spec.InstanceTypes) != 2 {
		t.Fatalf("spec %+v", spec)
	}
	if l.PricePerHour != 0.06 {
		t.Errorf("lease price %v", l.PricePerHour)
	}
	// An amd64 request in another environment never gets the arm64 size.
	r2 := req("r2")
	r2.EnvHash = "env2env2env2env2"
	if _, err := o.Acquire(ctx, r2, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	spec = f.specs[len(f.specs)-1]
	f.mu.Unlock()
	if spec.Size != "x86" || spec.Arch != "amd64" || spec.Spot {
		t.Fatalf("amd64 spec %+v", spec)
	}
	// A spot machine that fell back to on-demand is priced on-demand.
	o.mu.Lock()
	if p := o.priceLocked("fake", "arm", provider.MarketOnDemand); p != 0.15 {
		t.Errorf("fallback price %v", p)
	}
	if p := o.priceLocked("fake", "arm", provider.MarketSpot); p != 0.06 {
		t.Errorf("spot price %v", p)
	}
	o.mu.Unlock()
}

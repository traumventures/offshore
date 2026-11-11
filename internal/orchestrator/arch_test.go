package orchestrator

import (
	"context"
	"io"
	"testing"

	"github.com/traumventures/offshore/internal/scheduler"
)

// The architecture of a request selects the size of new machines, and
// the machine spec carries the size's instance types.
func TestArchAndInstanceTypesReachTheProvider(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) {
		c.Sizes = []scheduler.Size{
			{Provider: "fake", Name: "x86", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0.2, Arch: "amd64"},
			{Provider: "fake", Name: "arm", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0.06, Arch: "arm64",
				InstanceTypes: []string{"c7g.xlarge", "m7g.xlarge"}},
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
	if spec.Size != "arm" || spec.Arch != "arm64" || len(spec.InstanceTypes) != 2 {
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
	if spec.Size != "x86" || spec.Arch != "amd64" {
		t.Fatalf("amd64 spec %+v", spec)
	}
}

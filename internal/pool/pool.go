// Package pool is the boundary between the local daemon and the control
// plane. A Pool hands out leases on machines, builds environment images and
// learns resource usage. Local runs the orchestrator in process (embedded
// mode or the control plane itself); Remote talks to a shared control plane
// over HTTPS. Build traffic never passes through the pool: clients connect
// to the leased machine directly.
package pool

import (
	"context"
	"io"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// Pool provides machine capacity. progress receives messages for the user.
type Pool interface {
	// EnsureEnv makes an environment image available on every provider.
	EnsureEnv(ctx context.Context, env Env, progress io.Writer) error
	// Acquire reserves capacity for one session.
	Acquire(ctx context.Context, req orchestrator.Request, progress io.Writer) (orchestrator.Lease, error)
	// Release ends a lease and reports what the session consumed.
	Release(ctx context.Context, leaseID, usageKey string, usage *scheduler.Usage) error
	// Unreachable reports that a leased machine could not be reached.
	Unreachable(ctx context.Context, leaseID, machineID string) error
	// Estimate returns the expected need of a task, learned from earlier runs.
	Estimate(ctx context.Context, key string, fallback scheduler.Resources) (scheduler.Resources, error)
	// Status returns all machines with their last known server status.
	Status(ctx context.Context) ([]orchestrator.Machine, error)
	// Reconcile runs one maintenance pass (expiry, idle stop, warm pool).
	Reconcile(ctx context.Context) error
	// DestroyAll removes every machine and cache of the pool.
	DestroyAll(ctx context.Context) error
}

// Env is an environment image to make available. Context writes the build
// context to a temporary directory, which the caller removes.
type Env struct {
	Hash    string
	Tag     string
	Context func() (provider.ImageSpec, string, error)
}

// Config derives the orchestrator configuration from the pool
// configuration. Repository settings are not part of it: they travel in
// each orchestrator.Request.
func Config(u *config.PoolConfig) orchestrator.Config {
	idle, lifetime, queue := u.Pool.Durations()
	var provision []string
	for _, p := range u.Pool.Provision {
		provision = append(provision, p.Run)
	}
	var sizes []scheduler.Size
	for _, sz := range u.Pool.Sizes {
		mem, _ := config.ParseBytes(sz.Memory)
		var disk int64
		if sz.Disk != "" {
			disk, _ = config.ParseBytes(sz.Disk)
		}
		s := scheduler.Size{Provider: sz.Provider, Name: sz.Name,
			Capacity: scheduler.Resources{CPU: sz.CPU, Memory: mem, Disk: disk}, PricePerHour: sz.PricePerHour,
			Arch: config.MachineArch(sz.Arch), InstanceTypes: sz.InstanceTypes}
		sizes = append(sizes, s)
	}
	f := u.Pool
	c := orchestrator.Config{
		Owner: f.ID, Sizes: sizes, MaxLifetime: lifetime, QueueTimeout: queue,
		Provision: provision, Swap: f.Swap,
		Policy: scheduler.Config{
			CPUOvercommit: f.CPUOvercommit, MemoryOvercommit: f.MemoryOvercommit,
			MaxCPUPressure: f.MaxCPUPressure, MaxMemoryPressure: f.MaxMemoryPressure,
			SoftLimit: f.SoftLimit, Swap: f.Swap, LimitJobs: f.LimitJobs,
			MinMachines: f.MinMachines, MaxMachines: f.MaxMachines, IdleStop: idle,
			ProviderMax: map[string]int{}, Variant: f.Policy, ConsolidateTarget: f.ConsolidateTarget,
		},
	}
	if d := u.Telemetry.EventsRetentionDays; d > 0 {
		c.EventRetention = time.Duration(d) * 24 * time.Hour
	}
	if f.ConsolidateBelow != nil {
		c.Policy.ConsolidateBelow = *f.ConsolidateBelow
	}
	if f.CostOptimization != nil {
		c.Policy.CostOptimization = *f.CostOptimization
	}
	c.Policy.QueueTimeout = queue
	c.Policy.DiskPricePerGBHour = f.DiskPricePerGBMonth / 730
	for _, e := range f.Egress {
		var bw int64
		if e.Bandwidth != "" {
			bw, _ = config.ParseBytes(e.Bandwidth)
		}
		c.Policy.Egress = append(c.Policy.Egress, scheduler.Egress{From: e.From, To: e.To, PricePerGB: e.PricePerGB, Bandwidth: bw})
	}
	for _, w := range f.WarmPool {
		days, from, to, loc, err := w.Parse()
		if err != nil {
			continue // rejected by validation
		}
		c.Policy.WarmPool = append(c.Policy.WarmPool, scheduler.WarmWindow{Days: days, From: from, To: to, Machines: w.Machines, Location: loc})
	}
	for name, opts := range u.Providers {
		switch v := opts["max_machines"].(type) {
		case int64:
			c.Policy.ProviderMax[name] = int(v)
		case float64:
			c.Policy.ProviderMax[name] = int(v)
		}
	}
	return c
}

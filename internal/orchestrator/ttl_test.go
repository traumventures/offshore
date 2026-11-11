package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// Two concurrent requests of repositories with different cache TTLs must
// each get their own TTLs: the TTLs travel in the request, not in shared
// pool configuration.
func TestConcurrentRequestsKeepTheirTTLs(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) { c.MaxLifetime = 6 * time.Hour })
	short := req("short")
	short.EnvHash, short.BuildTTL, short.DepsTTL = "envAenvAenvAenvA", time.Hour, 2*time.Hour
	long := req("long")
	long.EnvHash, long.BuildTTL, long.DepsTTL = "envBenvBenvBenvB", 48*time.Hour, 3*24*time.Hour
	var wg sync.WaitGroup
	leases := make([]Lease, 2)
	errs := make([]error, 2)
	for i, r := range []Request{short, long} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leases[i], errs[i] = o.Acquire(context.Background(), r, io.Discard)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if leases[0].BuildTTL != time.Hour {
		t.Errorf("short repo build TTL = %v, want 1h", leases[0].BuildTTL)
	}
	if leases[1].BuildTTL != 6*time.Hour {
		t.Errorf("long repo build TTL = %v, want 6h (capped by max_lifetime)", leases[1].BuildTTL)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) != 2 {
		t.Fatalf("created %d machines, want 2", len(f.specs))
	}
	for _, s := range f.specs {
		want := map[string]time.Duration{short.EnvHash: 2 * time.Hour, long.EnvHash: 3 * 24 * time.Hour}[s.EnvHash]
		if s.DepsTTL != want {
			t.Errorf("machine for %s: deps TTL %v, want %v", s.EnvHash, s.DepsTTL, want)
		}
		// The machine-wide build TTL is the upper bound only; sessions send
		// their own.
		if got := s.Env["OFFSHORE_BUILD_TTL"]; got != (6 * time.Hour).String() {
			t.Errorf("machine build TTL %q, want max_lifetime", got)
		}
	}
	if o.cfg.MaxLifetime != 6*time.Hour || len(o.queued) != 0 {
		t.Errorf("requests leaked into pool state: %+v queued=%v", o.cfg, o.queued)
	}
}

// Requests from older clients carry no TTLs and get the defaults.
func TestRequestTTLDefaults(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	l, err := o.Acquire(context.Background(), req("r"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l.BuildTTL != DefaultBuildTTL || f.specs[0].DepsTTL != DefaultDepsTTL {
		t.Fatalf("lease TTL %v, deps TTL %v", l.BuildTTL, f.specs[0].DepsTTL)
	}
	// A machine created later for the environment (warm pool) uses the
	// parameters of its last request, also after a restart.
	r := req("r")
	r.DepsTTL = time.Hour
	o.Acquire(context.Background(), r, io.Discard)
	o2 := newTestOrchDir(t, f, o.stateDir)
	o2.mu.Lock()
	defer o2.mu.Unlock()
	if p := o2.paramsLocked(r.EnvHash); p.DepsTTL != time.Hour {
		t.Fatalf("persisted deps TTL %v", p.DepsTTL)
	}
}

// The size's disk budget reaches the worker; the worker's reported budget
// reaches the scheduler's machine view.
func TestDiskBudgetToWorker(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) {
		for i := range c.Sizes {
			c.Sizes[i].DiskBudget = "80%"
		}
	})
	if _, err := o.Acquire(context.Background(), req("r"), io.Discard); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.specs[0].Env["OFFSHORE_DISK_BUDGET"]
	f.mu.Unlock()
	if got != "80%" {
		t.Fatalf("OFFSHORE_DISK_BUDGET = %q, want 80%%", got)
	}
}

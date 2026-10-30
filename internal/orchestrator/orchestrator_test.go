package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/provider"
)

// fakeProvider keeps machines in memory; servers are simulated by fakeStatus.
type fakeProvider struct {
	mu        sync.Mutex
	machines  map[string]*provider.Machine
	status    map[string]*proto.Status
	volumes   map[string]provider.Volume
	created   int
	destroyed []string
	stopped   []string
}

func newFake() *fakeProvider {
	return &fakeProvider{machines: map[string]*provider.Machine{}, status: map[string]*proto.Status{}, volumes: map[string]provider.Volume{}}
}

func (f *fakeProvider) Name() string        { return "fake" }
func (f *fakeProvider) Caps() provider.Caps { return provider.Caps{} }
func (f *fakeProvider) EnsureImage(context.Context, provider.ImageSpec, io.Writer) error {
	return nil
}

func (f *fakeProvider) Create(_ context.Context, s provider.MachineSpec) (provider.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	m := &provider.Machine{ID: s.ID, State: provider.Running, Addr: "addr-" + s.ID, EnvHash: s.EnvHash, CPU: s.CPU, Memory: s.Memory,
		CreatedAt: time.Now(), ExpiresAt: s.ExpiresAt, Fingerprint: s.Finger}
	f.machines[s.ID] = m
	f.status[s.ID] = &proto.Status{Machine: s.ID, Ready: true, CPU: s.CPU, Memory: s.Memory, IdleSince: time.Now().Unix()}
	f.volumes["cs-cache-"+s.ID] = provider.Volume{Name: "cs-cache-" + s.ID, Kind: "cache"}
	return *m, nil
}

func (f *fakeProvider) Start(_ context.Context, id string) (provider.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.machines[id]
	m.State = provider.Running
	f.status[id].IdleSince = time.Now().Unix()
	return *m, nil
}

func (f *fakeProvider) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.machines[id].State = provider.Stopped
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeProvider) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.machines, id)
	delete(f.volumes, "cs-cache-"+id)
	f.destroyed = append(f.destroyed, id)
	return nil
}

func (f *fakeProvider) List(context.Context, string) ([]provider.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []provider.Machine
	for _, m := range f.machines {
		out = append(out, *m)
	}
	return out, nil
}

func (f *fakeProvider) Volumes(context.Context, string) ([]provider.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []provider.Volume
	for _, v := range f.volumes {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeProvider) DeleteVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
	return nil
}

func (f *fakeProvider) probe(_ context.Context, addr, _, _ string) (proto.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.machines {
		if m.Addr == addr && m.State == provider.Running {
			return *f.status[id], nil
		}
	}
	return proto.Status{}, fmt.Errorf("unreachable")
}

// startSession simulates a running session on a machine.
func (f *fakeProvider) startSession(id string, lease string, cpu float64, mem int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status[id]
	st.Sessions = append(st.Sessions, proto.Session{ID: lease, Lease: lease, CPU: cpu, Memory: mem})
	st.UsedCPU += cpu
	st.UsedMemory += mem
	st.IdleSince = 0
}

const gib = int64(1) << 30

func newTestOrch(t *testing.T, f *fakeProvider, mod func(*Config)) *Orchestrator {
	cfg := Config{
		Owner: "test", MachineCPU: 4, MachineMemory: 4 * gib, MinMachines: 0, MaxMachines: 3,
		IdleStop: time.Minute, MaxLifetime: 24 * time.Hour, QueueTimeout: 3 * time.Second, CPUOvercommit: 2,
	}
	if mod != nil {
		mod(&cfg)
	}
	o, err := New(cfg, f, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.ProbeFunc = f.probe
	return o
}

func req(repo string) Request {
	return Request{EnvHash: "env1env1env1env1", Image: "img", RepoID: repo, CPU: 2, Memory: 2 * gib}
}

func TestAcquireCreatesMachine(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	l, err := o.Acquire(context.Background(), req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.created != 1 || l.MachineID == "" || l.Token == "" {
		t.Fatalf("created=%d lease=%+v", f.created, l)
	}
}

func TestScaleOutWhenFull(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	ctx := context.Background()
	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		l, err := o.Acquire(ctx, req("r1"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		f.startSession(l.MachineID, l.ID, 2, 2*gib)
		seen[l.MachineID]++
	}
	// 4G machines fit two 2G sessions: three sessions need two machines.
	if f.created != 2 || len(seen) != 2 {
		t.Fatalf("created=%d placements=%v", f.created, seen)
	}
}

func TestQueueTimeoutAtMaxMachines(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) { c.MaxMachines = 1; c.QueueTimeout = 500 * time.Millisecond })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		l, err := o.Acquire(ctx, req("r1"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		f.startSession(l.MachineID, l.ID, 2, 2*gib)
	}
	if _, err := o.Acquire(ctx, req("r1"), io.Discard); err != ErrQueueTimeout {
		t.Fatalf("want ErrQueueTimeout, got %v", err)
	}
	if f.created != 1 {
		t.Fatalf("created %d machines, max is 1", f.created)
	}
}

func TestWarmCacheAffinity(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	ctx := context.Background()
	// Two machines exist; only the one the repo does NOT prefer by score
	// holds its warm cache.
	for _, id := range []string{"m1", "m2"} {
		f.Create(ctx, provider.MachineSpec{ID: id, EnvHash: "env1env1env1env1", CPU: 4, Memory: 4 * gib, ExpiresAt: time.Now().Add(time.Hour)})
		o.machines[id] = &Machine{Token: "t", pending: map[string]pendingLease{}}
	}
	pref := "m1"
	if score("repo", "m2") > score("repo", "m1") {
		pref = "m2"
	}
	other := map[string]string{"m1": "m2", "m2": "m1"}[pref]
	f.status[other].WarmRepos = []string{"repo"}
	l, err := o.Acquire(ctx, req("repo"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l.MachineID != other {
		t.Fatalf("placed on %s, want warm machine %s", l.MachineID, other)
	}
	// Without warm caches rendezvous hashing decides, stably.
	f.status[other].WarmRepos = nil
	o.Release(l.ID)
	o.Refresh(ctx)
	for i := 0; i < 3; i++ {
		l, err := o.Acquire(ctx, req("repo"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if l.MachineID != pref {
			t.Fatalf("placed on %s, want preferred %s", l.MachineID, pref)
		}
		o.Release(l.ID)
	}
}

func TestReconcileIdleExpiryAndWarmPool(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, func(c *Config) { c.MinMachines = 1; c.IdleStop = time.Second })
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		l, err := o.Acquire(ctx, req("r1"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		f.startSession(l.MachineID, l.ID, 2, 2*gib)
		ids = append(ids, l.MachineID)
	}
	// All sessions end; machines become idle long ago.
	f.mu.Lock()
	for _, st := range f.status {
		st.Sessions, st.UsedCPU, st.UsedMemory = nil, 0, 0
		st.IdleSince = time.Now().Add(-time.Hour).Unix()
	}
	f.mu.Unlock()
	o.mu.Lock()
	for _, m := range o.machines {
		m.pending = map[string]pendingLease{}
	}
	o.mu.Unlock()
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, m := range f.machines {
		if m.State == provider.Running {
			running++
		}
	}
	if running != 1 || len(f.stopped) != 1 {
		t.Fatalf("warm pool of 1 expected: running=%d stopped=%v", running, f.stopped)
	}
	// Expire everything: machines are destroyed together with their volumes.
	f.mu.Lock()
	for _, m := range f.machines {
		m.ExpiresAt = time.Now().Add(-time.Second)
	}
	f.volumes["orphan"] = provider.Volume{Name: "orphan", Kind: "cache"}
	f.volumes["deps-old"] = provider.Volume{Name: "deps-old", Kind: "deps", ExpiresAt: time.Now().Add(-time.Second)}
	f.volumes["deps-new"] = provider.Volume{Name: "deps-new", Kind: "deps", ExpiresAt: time.Now().Add(time.Hour)}
	f.mu.Unlock()
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// Warm pool refill may create a fresh machine; none of the old ones survive.
	for _, id := range ids {
		if _, ok := f.machines[id]; ok {
			t.Fatalf("expired machine %s survived", id)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes["orphan"]; ok {
		t.Fatal("orphaned cache volume survived")
	}
	if _, ok := f.volumes["deps-old"]; ok {
		t.Fatal("expired deps volume survived")
	}
	if _, ok := f.volumes["deps-new"]; !ok {
		t.Fatal("valid deps volume was deleted")
	}
}

func TestExpiringMachineGetsNoSessions(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	ctx := context.Background()
	f.Create(ctx, provider.MachineSpec{ID: "old", EnvHash: "env1env1env1env1", CPU: 4, Memory: 4 * gib, ExpiresAt: time.Now().Add(time.Minute)})
	o.machines["old"] = &Machine{Token: "t", pending: map[string]pendingLease{}}
	l, err := o.Acquire(ctx, req("r"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l.MachineID == "old" {
		t.Fatal("session placed on a machine about to expire")
	}
}

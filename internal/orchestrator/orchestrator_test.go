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
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// fakeProvider keeps machines in memory; servers are simulated by fakeStatus.
type fakeProvider struct {
	name      string
	mu        sync.Mutex
	machines  map[string]*provider.Machine
	status    map[string]*proto.Status
	volumes   map[string]provider.Volume
	created   int
	destroyed []string
	stopped   []string
}

func newFake() *fakeProvider { return newNamedFake("fake") }

func newNamedFake(name string) *fakeProvider {
	return &fakeProvider{name: name, machines: map[string]*provider.Machine{}, status: map[string]*proto.Status{}, volumes: map[string]provider.Volume{}}
}

func (f *fakeProvider) Name() string        { return f.name }
func (f *fakeProvider) Caps() provider.Caps { return provider.Caps{} }
func (f *fakeProvider) EnsureImage(context.Context, provider.ImageSpec, io.Writer) error {
	return nil
}

func (f *fakeProvider) Create(_ context.Context, s provider.MachineSpec) (provider.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	m := &provider.Machine{ID: s.ID, State: provider.Running, Addr: f.name + "-" + s.ID, EnvHash: s.EnvHash, CPU: s.CPU, Memory: s.Memory,
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

func testConfig(prov string) Config {
	return Config{
		Owner: "test", MaxLifetime: 24 * time.Hour, QueueTimeout: 3 * time.Second,
		Sizes:  []scheduler.Size{{Provider: prov, Name: "m4", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}}},
		Policy: scheduler.Config{CPUOvercommit: 2, MemoryOvercommit: 1, MinMachines: 0, MaxMachines: 3, IdleStop: time.Minute},
	}
}

func newTestOrch(t *testing.T, f *fakeProvider, mod func(*Config)) *Orchestrator {
	cfg := testConfig("fake")
	if mod != nil {
		mod(&cfg)
	}
	o, err := New(cfg, map[string]provider.Provider{"fake": f}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.ProbeFunc = f.probe
	return o
}

func req(repo string) Request {
	return Request{EnvHash: "env1env1env1env1", Image: "img", RepoID: repo, Need: scheduler.Resources{CPU: 2, Memory: 2 * gib}}
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
	o := newTestOrch(t, f, func(c *Config) { c.Policy.MaxMachines = 1; c.QueueTimeout = 500 * time.Millisecond })
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
		o.machines[id] = &Machine{Provider: "fake", Token: "t", pending: map[string]pendingLease{}}
	}
	pref := "m1"
	if scheduler.Score("repo", "m2") > scheduler.Score("repo", "m1") {
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
	o := newTestOrch(t, f, func(c *Config) { c.Policy.MinMachines = 1; c.Policy.IdleStop = time.Second })
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
	o.machines["old"] = &Machine{Provider: "fake", Token: "t", pending: map[string]pendingLease{}}
	l, err := o.Acquire(ctx, req("r"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l.MachineID == "old" {
		t.Fatal("session placed on a machine about to expire")
	}
}

// multiProbe routes probes to the fake that owns the address.
func multiProbe(fs ...*fakeProvider) func(context.Context, string, string, string) (proto.Status, error) {
	return func(ctx context.Context, addr, fp, tok string) (proto.Status, error) {
		for _, f := range fs {
			if st, err := f.probe(ctx, addr, fp, tok); err == nil {
				return st, nil
			}
		}
		return proto.Status{}, fmt.Errorf("unreachable")
	}
}

// One orchestrator spans several providers: the cheapest size is used
// first, a more expensive provider only once the first one is full.
func TestMultiProviderBurst(t *testing.T) {
	local, cloud := newNamedFake("local"), newNamedFake("aws")
	cfg := testConfig("local")
	cfg.Sizes = []scheduler.Size{
		{Provider: "aws", Name: "c7a.xlarge", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0.2},
		{Provider: "local", Name: "box", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0},
	}
	cfg.Policy.MaxMachines = 3
	cfg.Policy.ProviderMax = map[string]int{"local": 1}
	o, err := New(cfg, map[string]provider.Provider{"local": local, "aws": cloud}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.ProbeFunc = multiProbe(local, cloud)
	ctx := context.Background()
	placed := map[string]int{}
	for i := 0; i < 3; i++ {
		l, err := o.Acquire(ctx, req("r1"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []*fakeProvider{local, cloud} {
			if _, ok := f.machines[l.MachineID]; ok {
				f.startSession(l.MachineID, l.ID, 2, 2*gib)
				placed[f.name]++
			}
		}
	}
	if placed["local"] != 2 || placed["aws"] != 1 || local.created != 1 || cloud.created != 1 {
		t.Fatalf("placements=%v created local=%d aws=%d", placed, local.created, cloud.created)
	}
	if err := o.DestroyAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(local.machines)+len(cloud.machines) != 0 {
		t.Fatal("DestroyAll left machines")
	}
}

// slowStart is a provider whose machines take a while to start, so that
// refreshes and reconciles observe them still stopped mid-start.
type slowStart struct {
	*fakeProvider
	delay  time.Duration
	starts int
	smu    sync.Mutex
}

func (s *slowStart) Start(ctx context.Context, id string) (provider.Machine, error) {
	s.smu.Lock()
	s.starts++
	s.smu.Unlock()
	time.Sleep(s.delay)
	return s.fakeProvider.Start(ctx, id)
}

func (s *slowStart) Caps() provider.Caps {
	return provider.Caps{PollInterval: 20 * time.Millisecond, PollMax: 80 * time.Millisecond}
}

func TestWarmRestartStartsOnce(t *testing.T) {
	f := &slowStart{fakeProvider: newFake(), delay: 2500 * time.Millisecond}
	cfg := testConfig("fake")
	cfg.Policy.MaxMachines = 2
	o, err := New(cfg, map[string]provider.Provider{"fake": f}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.ProbeFunc = f.probe
	ctx := context.Background()
	l, err := o.Acquire(ctx, req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o.Release(l.ID)
	f.Stop(ctx, l.MachineID)
	if err := o.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// While the start is in progress, the waiting session refreshes every
	// second and reconciles run concurrently; none may start it again.
	stopRec := make(chan struct{})
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		for {
			select {
			case <-stopRec:
				return
			case <-time.After(300 * time.Millisecond):
				o.Reconcile(ctx)
			}
		}
	}()
	l2, err := o.Acquire(ctx, req("r1"), io.Discard)
	close(stopRec)
	<-recDone
	if err != nil {
		t.Fatal(err)
	}
	if l2.MachineID != l.MachineID {
		t.Fatalf("placed on %s, want the restarted machine %s", l2.MachineID, l.MachineID)
	}
	if f.starts != 1 || f.created != 1 {
		t.Fatalf("starts=%d created=%d, want one start and no new machine", f.starts, f.created)
	}
}

func TestPollBackoff(t *testing.T) {
	first, max := pollIntervals(provider.Caps{})
	if first != 500*time.Millisecond || max != first {
		t.Fatalf("default poll %v/%v", first, max)
	}
	first, max = pollIntervals(provider.Caps{PollInterval: 2 * time.Second, PollMax: 10 * time.Second})
	var seq []time.Duration
	for d := first; len(seq) < 6; d = nextPoll(d, max) {
		seq = append(seq, d)
	}
	want := []time.Duration{2 * time.Second, 3 * time.Second, 4500 * time.Millisecond, 6750 * time.Millisecond, 10 * time.Second, 10 * time.Second}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("poll sequence %v, want %v", seq, want)
		}
	}
}

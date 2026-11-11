package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// noCapacity fails every create with a capacity error.
type noCapacity struct {
	*fakeProvider
	tries int
}

func (n *noCapacity) Create(context.Context, provider.MachineSpec) (provider.Machine, error) {
	n.mu.Lock()
	n.tries++
	n.mu.Unlock()
	return provider.Machine{}, fmt.Errorf("aws-ec2: %w for size m4 (c7a: InsufficientInstanceCapacity)", provider.ErrCapacity)
}

// A provider without capacity is cooled down and the next plan creates
// on the other provider; the cooldown doubles on further errors.
func TestCapacityErrorCoolsDownProvider(t *testing.T) {
	a := &noCapacity{fakeProvider: newNamedFake("a")}
	b := newNamedFake("b")
	cfg := testConfig("a")
	cfg.Sizes = []scheduler.Size{
		{Provider: "a", Name: "m4", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0.1},
		{Provider: "b", Name: "m4", Capacity: scheduler.Resources{CPU: 4, Memory: 4 * gib}, PricePerHour: 0.3},
	}
	o, err := New(cfg, map[string]provider.Provider{"a": a, "b": b}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.ProbeFunc = func(ctx context.Context, addr, fp, token string) (proto.Status, error) {
		return b.probe(ctx, addr, fp, token)
	}
	l, err := o.Acquire(context.Background(), req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l.Provider != "b" || a.tries != 1 {
		t.Fatalf("lease on %s after %d tries on a, want b after 1", l.Provider, a.tries)
	}
	o.mu.Lock()
	h := o.health["a"]
	o.providerFailedLocked("a", fmt.Errorf("x: %w", provider.ErrCapacity))
	h2 := o.health["a"]
	o.mu.Unlock()
	if h.cooldown != baseCooldown || h2.cooldown != 2*baseCooldown || !h2.down.After(time.Now()) {
		t.Fatalf("cooldowns %v then %v", h.cooldown, h2.cooldown)
	}
	o.mu.Lock()
	o.providerReadyLocked("a", time.Minute, false)
	h3 := o.health["a"]
	o.mu.Unlock()
	if h3.cooldown != 0 || !h3.down.IsZero() || h3.boot != time.Minute {
		t.Fatalf("after ready: %+v", h3)
	}
}

func TestSnapshotsPersistAndDedupe(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchDir(t, newFake(), dir)
	at := time.Unix(1_800_000_000, 0)
	o.mu.Lock()
	for i := 0; i < 6; i++ {
		o.addSnapshotLocked(scheduler.Snapshot{Repo: "r", Store: "fake", Bytes: 1, At: at.Add(time.Duration(i) * time.Minute)})
	}
	o.addSnapshotLocked(scheduler.Snapshot{Repo: "r", Store: "fake", Bytes: 1, At: at.Add(5 * time.Minute)})
	o.mu.Unlock()
	o2 := newTestOrchDir(t, newFake(), dir)
	ss := o2.snapshots["r"]
	if len(ss) != snapshotsKeep || !ss[0].At.Equal(at.Add(5*time.Minute)) {
		t.Fatalf("got %+v", ss)
	}
}

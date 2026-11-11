package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
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

func TestHeadsPersistAndDedupe(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchDir(t, newFake(), dir)
	at := time.Unix(1_800_000_000, 0)
	o.mu.Lock()
	for i := 0; i < 6; i++ {
		o.addHeadLocked(scheduler.Snapshot{Repo: "r", Store: "fake", Bytes: 1, At: at.Add(time.Duration(i) * time.Minute), Layer: fmt.Sprint(i)}, nil, time.Time{})
	}
	if o.addHeadLocked(scheduler.Snapshot{Repo: "r", Store: "fake", Bytes: 1, At: at.Add(5 * time.Minute), Layer: "5"}, nil, time.Time{}) {
		t.Fatal("duplicate head added")
	}
	o.mu.Unlock()
	o2 := newTestOrchDir(t, newFake(), dir)
	ss := o2.heads["r"].Heads
	if len(ss) != headsKeep || !ss[0].At.Equal(at.Add(5*time.Minute)) || len(o2.heads["r"].Chains) != headsKeep {
		t.Fatalf("got %+v", o2.heads["r"])
	}
}

// A failing environment image must not make waiting requests create a
// machine per placement loop: one create, then a backoff per env and
// provider that doubles up to the cap.
func TestFailedImageBacksOffCreates(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	var mu sync.Mutex
	prepares := 0
	o.PrepareImage = func(context.Context, string, provider.Provider, string, string) (string, error) {
		mu.Lock()
		prepares++
		mu.Unlock()
		return "", errors.New("environment image on fake: docker login: no space left on device")
	}
	var wg sync.WaitGroup
	var out bytes.Buffer
	var outMu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			var b bytes.Buffer
			if _, err := o.Acquire(ctx, req(fmt.Sprint("r", i)), &b); err == nil {
				t.Error("lease without a machine")
			}
			outMu.Lock()
			out.Write(b.Bytes())
			outMu.Unlock()
		}(i)
	}
	wg.Wait()
	mu.Lock()
	n := prepares
	mu.Unlock()
	// One plan may create up to MaxMachines (3) machines at once; each
	// fails once, then the backoff holds.
	if n < 1 || n > 3 || f.created != 0 {
		t.Fatalf("%d image prepares, %d creates, want 1..3 and 0", n, f.created)
	}
	if !strings.Contains(out.String(), "no space left on device (retrying in ") {
		t.Fatalf("waiting requests not told about the failure:\n%s", out.String())
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	key := scheduler.CreateKey(req("r").EnvHash, "fake")
	if b := o.createFail[key]; b.delay < baseCreateBackoff || !b.until.After(o.now()) {
		t.Fatalf("backoff %+v", b)
	}
	if acts := o.planLocked(); len(acts) != 0 {
		t.Fatalf("planned %v during backoff", acts)
	}
	for i := 0; i < 10; i++ {
		o.createFailedLocked(req("r").EnvHash, "fake", errors.New("x"))
	}
	if b := o.createFail[key]; b.delay != maxCreateBackoff {
		t.Fatalf("backoff not capped: %v", b.delay)
	}
}

// The scheduler skips sizes of a provider blocked for the environment.
func TestCreateBlockedSizes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := scheduler.Pool{Now: now, Sizes: []scheduler.Size{{Provider: "a", Name: "s"}, {Provider: "b", Name: "s"}},
		CreateBlocked: map[string]time.Time{scheduler.CreateKey("e", "a"): now.Add(time.Second)}}
	if s := f.SizesFor("e"); len(s) != 1 || s[0].Provider != "b" {
		t.Fatalf("sizes %v", s)
	}
	if s := f.SizesFor("other"); len(s) != 2 {
		t.Fatalf("other env blocked: %v", s)
	}
	f.Now = now.Add(time.Second)
	if !f.CanCreate("e", "a") {
		t.Fatal("still blocked after the backoff")
	}
}

// slowDestroy blocks destroys until released.
type slowDestroy struct {
	*fakeProvider
	release chan struct{}
	calls   int
}

func (s *slowDestroy) Destroy(ctx context.Context, id string) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	<-s.release
	return s.fakeProvider.Destroy(ctx, id)
}

// Reconcile returns promptly while a slow destroy runs, and a second
// reconcile does not repeat it.
func TestReconcileDoesNotBlockOnSlowDestroy(t *testing.T) {
	old := reconcileWait
	reconcileWait = 50 * time.Millisecond
	defer func() { reconcileWait = old }()
	s := &slowDestroy{fakeProvider: newFake(), release: make(chan struct{})}
	s.machines["m1"] = &provider.Machine{ID: "m1", State: provider.Failed, EnvHash: "e"}
	o, err := New(testConfig("fake"), map[string]provider.Provider{"fake": s}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		start := time.Now()
		if err := o.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("reconcile took %v", d)
		}
	}
	s.mu.Lock()
	calls := s.calls
	s.mu.Unlock()
	close(s.release)
	if calls != 1 {
		t.Fatalf("%d destroys, want 1", calls)
	}
}

package orchestrator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/store"
)

// Layer GC deletes layers no kept head reaches and expired heads' layers,
// and keeps the chains of the newest heads.
func TestGCLayers(t *testing.T) {
	ctx := context.Background()
	o := newTestOrch(t, newFake(), nil)
	o.Stores = &StoreSet{Pool: "p", Override: &store.Config{Name: "storage", Dir: t.TempDir()}}
	st := o.repoStore(ctx, o.Stores, "fake", "r")
	if st == nil {
		t.Fatal("no store")
	}
	now := time.Now()
	id := func(name string, ago time.Duration) string { return fmt.Sprintf("%d-%s", now.Add(-ago).Unix(), name) }
	a, b, c := id("a", 3*time.Hour), id("b", 2*time.Hour), id("c", time.Hour)
	for _, l := range []string{a, b, c} {
		if err := anyfs.SaveLayer(ctx, st, &anyfs.Layer{ID: l}); err != nil {
			t.Fatal(err)
		}
	}
	head := func(layer string, at time.Time, chain []string, created time.Time) {
		o.mu.Lock()
		o.addHeadLocked(scheduler.Snapshot{Repo: "r", Provider: "fake", Store: "storage", At: at, Layer: layer}, chain, created)
		o.mu.Unlock()
	}
	head(b, now.Add(-2*time.Hour), []string{a, b}, now.Add(-3*time.Hour))
	// Four newer heads on a compacted base c push b out.
	for i := 0; i < headsKeep; i++ {
		d := id(fmt.Sprintf("d%d", i), 0)
		head(d, now.Add(time.Duration(i)*time.Second), []string{c, d}, now.Add(-time.Hour))
	}
	if n := o.GCLayers(ctx, true); n != 2 {
		t.Fatalf("deleted %d layers, want a and b", n)
	}
	if _, err := anyfs.LoadLayer(ctx, st, a); err == nil {
		t.Fatal("unreachable layer kept")
	}
	if _, err := anyfs.LoadLayer(ctx, st, c); err != nil {
		t.Fatalf("reachable base deleted: %v", err)
	}
	// The data of the remaining heads expires: everything goes.
	o.mu.Lock()
	for l := range o.heads["r"].Created {
		o.heads["r"].Created[l] = now.Add(-25 * time.Hour)
	}
	o.mu.Unlock()
	o.GCLayers(ctx, true)
	if _, err := anyfs.LoadLayer(ctx, st, c); err == nil {
		t.Fatal("expired layer kept")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.heads) != 0 {
		t.Fatalf("expired heads kept: %+v", o.heads)
	}
}

func TestOutputsStoreNewestHeadWithGrant(t *testing.T) {
	ctx := context.Background()
	o := newTestOrch(t, newFake(), nil)
	o.Stores = &StoreSet{Pool: "p", Override: &store.Config{Name: "storage", Dir: t.TempDir()}}
	if _, err := o.OutputsStore(ctx, "r"); err != ErrNoHead {
		t.Fatalf("without heads: %v", err)
	}
	now := time.Now()
	o.mu.Lock()
	o.addHeadLocked(scheduler.Snapshot{Repo: "r", Provider: "fake", Store: "storage", At: now.Add(-time.Hour), Layer: "1-old"}, nil, time.Time{})
	o.addHeadLocked(scheduler.Snapshot{Repo: "r", Provider: "fake", Store: "storage", At: now, Layer: "2-new"}, []string{"1-old", "2-new"}, time.Time{})
	o.mu.Unlock()
	out, err := o.OutputsStore(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if out.Head.Layer != "2-new" || len(out.Head.Chain) != 2 {
		t.Fatalf("head %+v", out.Head)
	}
	if !out.Grant.Allows(store.RepoScope("r")) || out.Grant.Allows(store.RepoScope("other")) {
		t.Fatalf("grant scopes %v", out.Grant.Scopes)
	}
}

// The pool's retention reaches grants and bounds the layer GC's TTL.
func TestStoreRetention(t *testing.T) {
	ctx := context.Background()
	o := newTestOrch(t, newFake(), nil)
	if o.layerTTL() != 24*time.Hour {
		t.Fatalf("default layer ttl %v", o.layerTTL())
	}
	o.Stores = &StoreSet{Pool: "p", Retention: 72 * time.Hour, Override: &store.Config{Name: "storage", Dir: t.TempDir()}}
	g, err := o.Stores.Granter(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	gr, err := g.Grant(ctx, []string{store.RepoScope("r")}, time.Hour)
	if err != nil || gr.Retention() != 72*time.Hour {
		t.Fatalf("grant %+v %v", gr, err)
	}
	if o.layerTTL() != 72*time.Hour {
		t.Fatalf("layer ttl %v", o.layerTTL())
	}
	now := time.Now()
	o.mu.Lock()
	o.addHeadLocked(scheduler.Snapshot{Repo: "r", Provider: "fake", Store: "storage", At: now, Layer: "1-a"}, []string{"1-a"}, now.Add(-48*time.Hour))
	o.mu.Unlock()
	o.GCLayers(ctx, true)
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.heads["r"].Heads) != 1 {
		t.Fatal("head within the retention dropped")
	}
}

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

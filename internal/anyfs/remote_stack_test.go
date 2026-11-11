package anyfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRemoteTreeStackLocalPeerStore: contents fetched from the peer
// become local pages (a reclaimed materialized file faults in from them
// without the peer); when the peer disappears, listings and contents
// come from the slot's committed layers in the store.
func TestRemoteTreeStackLocalPeerStore(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 3*PageSize+100; i++ { // several distinct pages
		fmt.Fprintf(&sb, "%015d\n", i)
	}
	big := sb.String()
	files := map[string]string{
		"target/debug/app":         big,
		"target/debug/small":       "small",
		"target/debug/deps/x.rlib": "rlib-x",
		"target/debug/deps/y.rlib": "rlib-y",
	}
	dir := t.TempDir()
	src := newFake()
	for rel, c := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
		mt := time.Unix(1700000000, int64(len(rel)))
		os.Chtimes(p, mt, mt)
		src.put(rel, c, mt.UnixNano())
	}
	st := dirStore(t)
	committed, _, err := Scan(dir, ScanOptions{Sink: func(k Key, b []byte) error { return st.Put(ctx, k, b, Dirty) }})
	if err != nil {
		t.Fatal(err)
	}
	// y.rlib was rebuilt after the commit: the store has an older version.
	src.put("target/debug/deps/y.rlib", "rlib-y2", 1)

	tr := newTestTree(t)
	var resolved atomic.Int64
	tr.SetBacking(func(context.Context) (*Tree, Tier, error) {
		resolved.Add(1)
		return committed, st, nil
	})
	tr.Update(src)

	if got, err := readContent(t, tr, "target/debug/app"); err != nil || got != big {
		t.Fatalf("app from peer: %d bytes, %v", len(got), err)
	}
	if n := tr.local.(*Local).Stats().Pages; n < 3 {
		t.Fatalf("%d local pages after a multi-page download", n)
	}
	tr.files.Reclaim(0)
	if got, err := readContent(t, tr, "target/debug/app"); err != nil || got != big {
		t.Fatalf("app from local pages: %d bytes, %v", len(got), err)
	}
	if _, reads := src.counts(); reads != 1 {
		t.Fatalf("%d peer reads, want 1 (refault from local pages)", reads)
	}
	if resolved.Load() != 0 {
		t.Fatal("store resolved while the peer works")
	}

	// The machine is gone.
	src.mu.Lock()
	src.down = true
	src.mu.Unlock()
	tr.Update(nil)
	m, err := tr.List(context.Background(), "target/debug/deps")
	if err != nil || len(m) != 2 {
		t.Fatalf("listing from the store: %v %v", m, err)
	}
	if got, err := readContent(t, tr, "target/debug/deps/x.rlib"); err != nil || got != "rlib-x" {
		t.Fatalf("x.rlib from the store: %q %v", got, err)
	}
	if got, err := readContent(t, tr, "target/debug/app"); err != nil || got != big {
		t.Fatalf("app after peer loss: %d bytes, %v", len(got), err)
	}
	tr.files.Reclaim(0)
	tr.local.(*Local).Reclaim(ctx, 0)
	if got, err := readContent(t, tr, "target/debug/small"); err != nil || got != "small" {
		t.Fatalf("small from the store after reclaim: %q %v", got, err)
	}
	// A listed version the store does not hold (peer listing cached from
	// before) is unavailable, not wrong.
	if _, err := tr.Content(context.Background(), "target/debug/deps/y.rlib", entryOfNode(&Node{Path: "y.rlib", Kind: File, Size: 7, MtimeNs: 1})); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("version missing in the store: %v", err)
	}
	if _, err := tr.List(context.Background(), "target/release"); !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing dir in the store: %v", err)
	}
	if n := resolved.Load(); n != 1 {
		t.Fatalf("backing resolved %d times in one generation", n)
	}
}

func TestRemoteTreeBackingFailureIsUnavailable(t *testing.T) {
	src := newFake()
	src.down = true
	tr := newTestTree(t)
	tr.SetBacking(func(context.Context) (*Tree, Tier, error) { return nil, nil, errors.New("no grant") })
	tr.Update(src)
	if _, err := tr.List(context.Background(), "target"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

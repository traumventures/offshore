package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/access"
	"github.com/traumventures/offshore/internal/adapter"
	"github.com/traumventures/offshore/internal/anyfs"
)

func TestSlotCommitObservesAccess(t *testing.T) {
	a, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	runSession(t, a, func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/libx-0123456789abcdef.rlib"), []byte("rlib"), mt)
		put(t, filepath.Join(data, "target/debug/.fingerprint/x-0123456789abcdef/lib-x"), []byte("fp"), mt)
	})
	if _, err := a.saveSlot(context.Background(), "repo", gr); err != nil {
		t.Fatal(err)
	}
	r, _ := a.cfs.repo("repo")
	if r.access.Sessions() != 1 {
		t.Fatalf("sessions %d", r.access.Sessions())
	}
	if st, ok := r.access.Stats("target/debug/deps/*.rlib"); !ok || st.Creates != 1 {
		t.Fatalf("deps stats %+v", st)
	}
	f := filepath.Join(a.cfs.dir, "repos", "repo", accessFile)
	if _, err := os.Stat(f); err != nil {
		t.Fatal(err)
	}
	if m := loadAccess(filepath.Dir(f)); m.Sessions() != 1 || m.Classify("target/debug/incremental/x").Class != access.Churn {
		t.Fatalf("reloaded model: %d sessions", m.Sessions())
	}
}

func TestClassPrefetchAndColdOrder(t *testing.T) {
	m := access.New(adapter.AccessPriors()...)
	file := func(p string, size int64) anyfs.Node {
		return anyfs.Node{Path: p, Kind: anyfs.File, Size: size, Digest: p}
	}
	tr := anyfs.NewTree([]anyfs.Node{
		file("target/debug/deps/a.rlib", 300),
		file("target/debug/deps/b.rlib", 300),
		file("target/debug/deps/small.d", 10),
		file("target/debug/build/x-0123456789abcdef/out/gen.rs", 200),
		file("target/debug/incremental/x/s-1/a.bin", 500),
		file("target/tmp/scratch", 100),
		file("src/main.rs", 1000),
	})
	got := prefetchPlan(m, tr, 64, 600)
	var paths []string
	for _, n := range got {
		paths = append(paths, n.Path)
	}
	// immutable and write-once only, no small files, within the budget
	if len(paths) != 2 || paths[0] != "target/debug/build/x-0123456789abcdef/out/gen.rs" || paths[1] != "target/debug/deps/a.rlib" {
		t.Fatalf("prefetch %v", paths)
	}
	cold := coldOrder(m, []*anyfs.Tree{tr})
	if len(cold) != 2 || cold[0].Path != "target/debug/incremental/x/s-1/a.bin" || cold[1].Path != "target/tmp/scratch" {
		t.Fatalf("cold order %v", cold)
	}
}

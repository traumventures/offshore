package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/store"
)

func TestSeedFromSiblingHead(t *testing.T) {
	for _, mode := range []fsMode{fsPlain, fsUser} {
		t.Run(mode.String(), func(t *testing.T) { testSeedFromSiblingHead(t, mode) })
	}
}

func grantFor(t *testing.T, storeDir, repo string) *store.Grant {
	t.Helper()
	g, err := store.NewGranter(context.Background(), store.Config{Name: "local", Dir: storeDir, Prefix: "cache"})
	if err != nil {
		t.Fatal(err)
	}
	gr, err := g.Grant(context.Background(), []string{store.RepoScope(repo)}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &gr
}

func sessionOf(t *testing.T, w *Worker, repo string, f func(data string)) (string, bool) {
	t.Helper()
	slot, _, err := w.slots.lease(repo, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := w.cfs.prepare(context.Background(), w.slots, slot, repo, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f(filepath.Join(slot, "data"))
	time.Sleep(1100 * time.Millisecond)
	w.slots.release(slot)
	return slot, warm
}

// A new worktree ("sib") starts from its sibling's head: warm, the
// sibling's target content, and after its first flush its own prefix
// holds every page (a machine granted only "sib" restores it).
func testSeedFromSiblingHead(t *testing.T, mode fsMode) {
	ctx := context.Background()
	storeDir := t.TempDir()
	a, gr := fsWorker(t, mode, storeDir, cacheFSOptions{})
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	big := []byte(strings.Repeat("rlib", 1<<19))
	sessionOf(t, a, "repo", func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/libdep.rlib"), big, mt)
		put(t, filepath.Join(data, "target/debug/.fingerprint/dep/lib"), []byte("fp"), mt)
	})
	res, err := a.saveSlot(ctx, "repo", gr)
	if err != nil {
		t.Fatal(err)
	}
	h := *res.Head

	b, _ := fsWorker(t, mode, storeDir, cacheFSOptions{})
	sib := grantFor(t, storeDir, "sib")
	// The sibling's prefix is not readable with the new worktree's grant.
	if _, err := b.restoreSlotFrom(ctx, "sib", h, sib, nil); err == nil {
		t.Fatal("seed without a grant of the source prefix")
	}
	if _, err := b.restoreSlotFrom(ctx, "sib", h, sib, gr); err != nil {
		t.Fatalf("seed: %v", err)
	}
	slot, warm := sessionOf(t, b, "sib", func(data string) {
		if got, err := os.ReadFile(filepath.Join(data, "target/debug/deps/libdep.rlib")); err != nil || string(got) != string(big) {
			t.Fatalf("seeded content: %v", err)
		}
		put(t, filepath.Join(data, "target/debug/deps/libown.rlib"), []byte("own"), mt)
	})
	if !warm {
		t.Fatal("seeded slot not warm")
	}
	sr := ref(t, b, slot)
	sr.Repo = "sib"
	h2, err := b.writebackSlot(ctx, sr, "test")
	if err != nil || h2.Repo != "sib" {
		t.Fatalf("flush seeded slot: %+v %v", h2, err)
	}
	b.cfs.mu.Lock()
	r := b.cfs.repos["sib"]
	b.cfs.mu.Unlock()
	if r.seed != nil || r.seedPending {
		t.Fatal("seed source kept after the first flush")
	}

	c, _ := fsWorker(t, mode, storeDir, cacheFSOptions{})
	if _, err := c.restoreSlot(ctx, "sib", h2, sib); err != nil {
		t.Fatalf("restore of the seeded repository's own head: %v", err)
	}
	sessionOf(t, c, "sib", func(data string) {
		for p, want := range map[string]string{"target/debug/deps/libdep.rlib": string(big), "target/debug/deps/libown.rlib": "own"} {
			if got, err := os.ReadFile(filepath.Join(data, p)); err != nil || string(got) != want {
				t.Fatalf("%s from the own prefix: %v", p, err)
			}
		}
	})
}

// A head in another provider's store (another store directory here)
// restores with a source grant of that store; the first flush copies
// what the machine's own store lacks.
func TestRestoreFromOtherStore(t *testing.T) {
	ctx := context.Background()
	storeA, storeB := t.TempDir(), t.TempDir()
	a, grA := fsWorker(t, fsPlain, storeA, cacheFSOptions{})
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	sessionOf(t, a, "repo", func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/libdep.rlib"), []byte(strings.Repeat("x", 3<<20)), mt)
	})
	res, err := a.saveSlot(ctx, "repo", grA)
	if err != nil {
		t.Fatal(err)
	}
	b, grB := fsWorker(t, fsPlain, storeB, cacheFSOptions{})
	if _, err := b.restoreSlot(ctx, "repo", *res.Head, grB); err == nil {
		t.Fatal("restore of another store's head without its grant")
	}
	if _, err := b.restoreSlotFrom(ctx, "repo", *res.Head, grB, grA); err != nil {
		t.Fatalf("restore from store A: %v", err)
	}
	slot, warm := sessionOf(t, b, "repo", func(data string) {
		put(t, filepath.Join(data, "target/debug/new"), []byte("n"), mt)
	})
	if !warm {
		t.Fatal("not warm")
	}
	h, err := b.writebackSlot(ctx, ref(t, b, slot), "test")
	if err != nil {
		t.Fatal(err)
	}
	c, grC := fsWorker(t, fsPlain, storeB, cacheFSOptions{})
	if _, err := c.restoreSlot(ctx, "repo", h, grC); err != nil {
		t.Fatalf("store B holds the head: %v", err)
	}
	sessionOf(t, c, "repo", func(data string) {
		if fi, err := os.Stat(filepath.Join(data, "target/debug/deps/libdep.rlib")); err != nil || fi.Size() != 3<<20 {
			t.Fatalf("content in store B: %v", err)
		}
	})
}

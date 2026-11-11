package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startSession leases the repository's slot and prepares it.
func startSession(t *testing.T, w *Worker) string {
	t.Helper()
	slot, _, err := w.slots.lease("repo", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.cfs.prepare(context.Background(), w.slots, slot, "repo", 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	return slot
}

// endSession ends a session like exec: the run is recorded, the session
// captured, the slot released (commit: the background commit runs).
func endSession(t *testing.T, w *Worker, slot string, commit bool) {
	t.Helper()
	time.Sleep(1100 * time.Millisecond)
	w.slots.update(slot, func(m *slotMeta) { m.Runs++ })
	if err := w.snapshotSlot(context.Background(), slot, "repo"); err != nil {
		t.Fatal(err)
	}
	w.slots.release(slot)
	if commit {
		w.slotReleased(slot, "repo")
	}
}

func headTree(t *testing.T, w *Worker, slot string) map[string]bool {
	t.Helper()
	m, _ := readMeta(slot)
	r, _ := w.cfs.repo("repo")
	ch, err := r.chain(context.Background(), m.Head)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, n := range ch.Tree().Nodes() {
		if n.IsFile() {
			out[n.Path] = true
		}
	}
	return out
}

func TestSlotSessionSnapshots(t *testing.T) {
	for _, mode := range []fsMode{fsUser, fsKernel} {
		t.Run(mode.String(), func(t *testing.T) { testSlotSessionSnapshots(t, mode) })
	}
}

func testSlotSessionSnapshots(t *testing.T, mode fsMode) {
	ctx := context.Background()
	cache, storeDir := t.TempDir(), t.TempDir()
	w, gr := fsWorkerAt(t, mode, cache, storeDir, cacheFSOptions{})
	w.grants.put("repo", *gr)
	if err := w.connectStore("repo", nil); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-time.Hour)

	// Session 1 is committed while session 2 already runs; a writeback
	// of the busy slot flushes it.
	slot := startSession(t, w)
	put(t, filepath.Join(slot, "data", "s1"), randBytes(100<<10), mt)
	endSession(t, w, slot, false)
	if startSession(t, w) != slot {
		t.Fatal("next session got another slot")
	}
	put(t, filepath.Join(slot, "data", "s2"), []byte("session 2"), mt)
	w.slotReleased(slot, "repo") // background commit of session 1, slot busy
	h, err := w.writebackSlot(ctx, ref(t, w, slot), "test")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := w.cfs.repo("repo")
	ch, _ := r.chain(ctx, h.Layer)
	if _, ok := ch.Tree().Get("s1"); !ok {
		t.Fatal("busy slot flush lacks the previous session")
	}
	if _, ok := ch.Tree().Get("s2"); ok {
		t.Fatal("busy slot flush holds the running session")
	}

	// Session 2 ends and is captured, the worker crashes before the
	// background commit.
	endSession(t, w, slot, false)
	w.cfs.unmountAll()
	w2, _ := fsWorkerAt(t, mode, cache, storeDir, cacheFSOptions{})
	w2.recoverSnapshots(ctx)
	if got := headTree(t, w2, slot); !got["s1"] || !got["s2"] {
		t.Fatalf("recovered head %v", got)
	}
	if len(pendingSnapshots(slot)) != 0 {
		t.Fatal("snapshots left")
	}

	// Session 3 crashes mid-session: only it is lost.
	if startSession(t, w2) != slot {
		t.Fatal("slot changed")
	}
	put(t, filepath.Join(slot, "data", "s3"), []byte("session 3"), mt)
	os.MkdirAll(filepath.Join(slot, snapsDir, "99999999999999999999"), 0o755) // an incomplete capture
	w2.cfs.unmountAll()
	w3, _ := fsWorkerAt(t, mode, cache, storeDir, cacheFSOptions{})
	w3.recoverSnapshots(ctx)
	if got := headTree(t, w3, slot); !got["s1"] || !got["s2"] || got["s3"] {
		t.Fatalf("head after a crash mid-session %v", got)
	}
	if len(pendingSnapshots(slot)) != 0 {
		t.Fatal("incomplete snapshot kept")
	}
}

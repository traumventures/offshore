package worker

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/sandbox"
)

// Files hashed in the background while a session writes them are not
// read again by the commit; a file rewritten after it was hashed is.
func TestSlotCommitUsesBackgroundDigests(t *testing.T) {
	for _, mode := range []fsMode{fsUser, fsKernel} {
		t.Run(mode.String(), func(t *testing.T) { testSlotCommitUsesBackgroundDigests(t, mode) })
	}
}

func testSlotCommitUsesBackgroundDigests(t *testing.T, mode fsMode) {
	ctx := context.Background()
	w, gr := fsWorker(t, mode, t.TempDir(), cacheFSOptions{})
	w.grants.put("repo", *gr)
	if err := w.connectStore("repo", nil); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-time.Hour)
	slot := startSession(t, w)
	data, upper := filepath.Join(slot, "data"), filepath.Join(slot, "upper")
	big, other := randBytes(3<<20), randBytes(2<<20)
	put(t, filepath.Join(data, "target/debug/deps/libbig.rlib"), big, mt)
	put(t, filepath.Join(data, "target/debug/deps/libother.rlib"), other, mt)
	h := w.cfs.slotHasher(slot)
	if h == nil {
		t.Fatal("no hasher")
	}
	// What fanotify close_write reports (the test's own writes are
	// filtered as the worker's).
	for _, rel := range []string{"target/debug/deps/libbig.rlib", "target/debug/deps/libother.rlib"} {
		h.Queue(rel, filepath.Join(upper, rel))
	}
	for i := 0; h.Stats().Hashed < 2; i++ {
		if i > 300 {
			t.Fatalf("not hashed: %+v", h.Stats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// libother changes after it was hashed.
	other = randBytes(2 << 20)
	put(t, filepath.Join(data, "target/debug/deps/libother.rlib"), other, mt)
	endSession(t, w, slot, false)
	res, err := w.commitPending(ctx, slot, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if res.scan.Known != 1 || res.scan.KnownRead != 3<<20 || res.scan.Read != 2<<20 {
		t.Fatalf("scan %+v", res.scan)
	}
	r, _ := w.cfs.repo("repo")
	for rel, want := range map[string][]byte{"target/debug/deps/libbig.rlib": big, "target/debug/deps/libother.rlib": other} {
		n, ok := res.tree.Get(rel)
		if !ok || n.Digest != string(anyfs.KeyOf(want)) {
			t.Fatalf("%s: %+v", rel, n)
		}
		var got []byte
		for _, p := range n.Pages {
			b, err := r.tier.Get(ctx, p.Key)
			if err != nil {
				t.Fatalf("%s page %s: %v", rel, p.Key.Short(), err)
			}
			got = append(got, b[p.Off:p.Off+p.Len]...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: pages do not hold the content", rel)
		}
	}
	if st := h.Stats(); st.Hits != 1 || len(h.Keys()) != 0 {
		t.Fatalf("hasher %+v keys %d", st, len(h.Keys()))
	}
}

// Writes seen by fanotify (through a session's bind) reach the hasher
// with the slot path; reads are noted as before.
func TestReadTrackerWrites(t *testing.T) {
	tr := newReadTracker("/slot/data")
	var writes []string
	tr.onWrite = func(rel string) { writes = append(writes, rel) }
	tr.beginSession([]sandbox.Bind{{Src: "/slot/data/target", Dst: "/work/target"}})
	tr.notePath("/work/target/debug/x", true)
	tr.notePath("/slot/data/y", true)
	tr.notePath("/work/target/debug/z", false)
	if len(writes) != 2 || writes[0] != "target/debug/x" || writes[1] != "y" {
		t.Fatalf("writes %v", writes)
	}
	if s := tr.endSession(); !s["target/debug/z"] || len(s) != 1 {
		t.Fatalf("reads %v", s)
	}
}

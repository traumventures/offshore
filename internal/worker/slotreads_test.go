package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/sandbox"
)

func TestWorkingSetOrderAndWindow(t *testing.T) {
	w := newWorkingSet()
	w.touch(map[string]int64{"a": 10, "b": 20})
	w.touch(map[string]int64{"a": 10})
	w.touch(map[string]int64{"c": 5})
	tr := anyfs.NewTree([]anyfs.Node{{Path: "a", Kind: anyfs.File, Size: 10}, {Path: "b", Kind: anyfs.File, Size: 20},
		{Path: "c", Kind: anyfs.File, Size: 5}, {Path: "d", Kind: anyfs.File, Size: 1}})
	var got []string
	for _, n := range w.plan(tr) {
		got = append(got, n.Path)
	}
	if fmt.Sprint(got) != "[c a b]" {
		t.Fatalf("plan %v", got)
	}
	if w.bytes() != 35 {
		t.Fatalf("bytes %d", w.bytes())
	}
	w.touch(nil)
	if w.bytes() != 15 { // b fell out of the window
		t.Fatalf("bytes after a session %d", w.bytes())
	}
	small := parseWorkingSet(w.marshal(1))
	if len(small.Files) != 1 || small.Files["c"] == nil || small.Seq != 4 {
		t.Fatalf("capped %+v", small.Files)
	}
	for i := 0; i < wsKeep; i++ {
		w.touch(nil)
	}
	if len(w.Files) != 0 {
		t.Fatalf("stale entries kept: %v", w.Files)
	}
}

// Reads of a session (another process, through a bind like a session's)
// feed the access model and the working set, which travels in the head
// layer and is read ahead when another machine mounts the head.
func TestSlotReadTracking(t *testing.T) {
	for _, mode := range []fsMode{fsUser, fsKernel} {
		t.Run(mode.String(), func(t *testing.T) { testSlotReadTracking(t, mode) })
	}
}

func testSlotReadTracking(t *testing.T, mode fsMode) {
	ctx := context.Background()
	storeDir := t.TempDir()
	a, gr := fsWorker(t, mode, storeDir, cacheFSOptions{})
	mt := time.Now().Add(-time.Hour)
	big := randBytes(1 << 20)
	slot, _ := runSession(t, a, func(data string) {
		put(t, filepath.Join(data, "work/hot-0123456789abcdef.bin"), big, mt)
		put(t, filepath.Join(data, "work/cold-fedcba9876543210.bin"), randBytes(1<<20), mt)
	})
	if _, err := a.saveSlot(ctx, "repo", gr); err != nil {
		t.Fatal(err)
	}
	// A session reads one rlib through a bind (in kernel mode fanotify
	// maps the bind back), the other process reads nothing else.
	lease, _, err := a.slots.lease("repo", 24*time.Hour)
	if err != nil || lease != slot {
		t.Fatalf("lease %s %v", lease, err)
	}
	if _, err := a.cfs.prepare(ctx, a.slots, slot, "repo", 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(slot, "data")
	a.cfs.sessionStarted(slot, []sandbox.Bind{{Src: filepath.Join(data, "work"), Dst: "/nonexistent/repo/work"}})
	if out, err := exec.Command("cat", filepath.Join(data, "work/hot-0123456789abcdef.bin")).Output(); err != nil || len(out) != len(big) {
		t.Fatal(err)
	}
	a.cfs.tracker(slot).notePath("/nonexistent/repo/work/hot-0123456789abcdef.bin", false) // as seen from a session
	time.Sleep(300 * time.Millisecond)
	reads, n, ok := a.cfs.sessionEnded(slot, "repo")
	if !ok || !reads["work/hot-0123456789abcdef.bin"] || len(reads) != 1 || n != int64(len(big)) {
		t.Fatalf("session reads %v %d %v", reads, n, ok)
	}
	put(t, filepath.Join(data, "target/debug/new"), []byte("x"), mt)
	time.Sleep(1100 * time.Millisecond)
	a.slots.release(slot)
	h, err := a.writebackSlot(ctx, ref(t, a, slot), "test")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := a.cfs.repo("repo")
	if st, ok := r.access.Stats("work/*.bin"); !ok || st.Reads != 1 || st.ReadFileSessions < 2 {
		t.Fatalf("access stats %+v", st)
	}
	if a.cfs.vm().WorkingSetBytes != int64(len(big)) {
		t.Fatalf("working set %d", a.cfs.vm().WorkingSetBytes)
	}
	ch, _ := r.chain(ctx, h.Layer)
	var ws workingSet
	if err := json.Unmarshal(ch.Top().Meta, &ws); err != nil || len(ws.Files) != 1 {
		t.Fatalf("layer meta %s %v", ch.Top().Meta, err)
	}
	// Another machine mounting the head reads the working set ahead.
	b, gr2 := fsWorker(t, mode, storeDir, cacheFSOptions{})
	if _, err := b.restoreSlot(ctx, "repo", h, gr2); err != nil {
		t.Fatal(err)
	}
	hot, _ := ch.Tree().Get("work/hot-0123456789abcdef.bin")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := b.cfs.files.Local(hot); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("working set not read ahead")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cold, _ := ch.Tree().Get("work/cold-fedcba9876543210.bin")
	if _, ok := b.cfs.files.Local(cold); ok {
		t.Fatal("file outside the working set read ahead")
	}
}

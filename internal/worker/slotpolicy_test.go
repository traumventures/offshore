package worker

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// sessionIn runs f in a slot of repo and commits it.
func sessionIn(t *testing.T, w *Worker, repo string, f func(data string)) commitResult {
	t.Helper()
	ctx := context.Background()
	slot, _, err := w.slots.lease(repo, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.cfs.prepare(ctx, w.slots, slot, repo, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	f(filepath.Join(slot, "data"))
	time.Sleep(1100 * time.Millisecond)
	w.slots.release(slot)
	m, _ := readMeta(slot)
	res, err := w.commitSlot(ctx, slotRef{Repo: repo, Path: slot, Meta: m})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The same immutable file in two repositories is one page: same key,
// one file on disk (the machine's page pool).
func TestSlotDedupeAcrossRepos(t *testing.T) {
	w, _ := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	rlib := randBytes(64 << 10)
	mt := time.Now().Add(-time.Hour)
	var keys []anyfs.Key
	var repos []*repoFS
	for i, repo := range []string{"a", "b"} {
		res := sessionIn(t, w, repo, func(data string) {
			put(t, filepath.Join(data, "target/debug/deps/libx-0123456789abcdef.rlib"), rlib, mt)
			put(t, filepath.Join(data, "target/debug/deps/libx-0123456789abcdef.d"), []byte(repo+" neighbour"), mt)
			put(t, filepath.Join(data, "src", repo), randBytes(1000*(i+1)), mt)
		})
		n, _ := res.tree.Get("target/debug/deps/libx-0123456789abcdef.rlib")
		if len(n.Pages) != 1 {
			t.Fatalf("rlib pages %+v", n.Pages)
		}
		keys = append(keys, n.Pages[0].Key)
		repos = append(repos, res.repo)
	}
	if keys[0] != keys[1] {
		t.Fatal("same immutable content, different pages")
	}
	fa, _ := os.Stat(repos[0].local.Path(keys[0]))
	fb, _ := os.Stat(repos[1].local.Path(keys[1]))
	if fa == nil || fb == nil || !os.SameFile(fa, fb) {
		t.Fatal("deduped page stored twice")
	}
	shared := repos[1].local.Stats().SharedBytes
	if shared != 64<<10 {
		t.Fatalf("shared bytes %d", shared)
	}
	vm := w.cfs.vm()
	sum := repos[0].local.Stats().ResidentBytes + repos[1].local.Stats().ResidentBytes
	t.Logf("resident pages %d bytes, %d shared, reported %d", sum, shared, vm.ResidentBytes-w.cfs.files.Stats().ResidentBytes)
	if vm.ResidentBytes-w.cfs.files.Stats().ResidentBytes != sum-shared {
		t.Fatal("shared pages counted twice")
	}
}

// Idle commits leave pages to the class writeback: immutable pages are
// written back early, churn and ephemeral pages stay dirty, the store
// head moves when they are clean or after maxFlushLag.
func TestSlotClassWriteback(t *testing.T) {
	ctx := context.Background()
	w, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{WritebackInterval: 10 * time.Millisecond})
	w.grants.put("repo", *gr)
	if err := w.connectStore("repo", nil); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-time.Hour)
	slot, _ := runSession(t, w, func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/liba.rlib"), randBytes(64<<10), mt)
		put(t, filepath.Join(data, "target/debug/incremental/a-1/s-1/q.bin"), randBytes(2<<20), mt)
		put(t, filepath.Join(data, "target/tmp/scratch.bin"), randBytes(2<<20), mt)
	})
	w.maintainSlot(ctx, ref(t, w, slot), "idle")
	r, _ := w.cfs.repo("repo")
	_, _, remote, _ := r.stack()
	st := anyfs.NewStore(remote, anyfs.StoreMedium())
	m, _ := readMeta(slot)
	ch, _ := r.chain(ctx, m.Head)
	tree := ch.Tree()
	key := func(p string) anyfs.Key { n, _ := tree.Get(p); return n.Pages[0].Key }
	imm, churn, eph := key("target/debug/deps/liba.rlib"), key("target/debug/incremental/a-1/s-1/q.bin"), key("target/tmp/scratch.bin")
	deadline := time.Now().Add(5 * time.Second)
	for !st.Has(ctx, imm) {
		if time.Now().After(deadline) {
			t.Fatal("immutable page not written back early")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if st.Has(ctx, churn) || st.Has(ctx, eph) {
		t.Fatal("churn/ephemeral pages written back early")
	}
	if len(w.cfs.flushedHeads()) != 0 {
		t.Fatal("store head moved while pages are dirty")
	}
	// After maxFlushLag the head is flushed whole.
	w.slots.update(slot, func(x *slotMeta) { x.Committed = time.Now().Add(-maxFlushLag - time.Minute).Unix() })
	w.maintainSlots(ctx)
	if h := w.cfs.flushedHeads(); len(h) != 1 || h[0].Layer != m.Head || !st.Has(ctx, churn) || !st.Has(ctx, eph) {
		t.Fatalf("head not flushed after the lag: %+v", h)
	}
}

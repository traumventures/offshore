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
// head moves when they are clean or at the flush deadline.
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
	// After the flush deadline the head is flushed whole.
	w.slots.update(slot, func(x *slotMeta) { x.Committed = time.Now().Add(-w.cfs.opt.FlushDeadline - time.Minute).Unix() })
	w.maintainSlots(ctx)
	if h := w.cfs.flushedHeads(); len(h) != 1 || h[0].Layer != m.Head || !st.Has(ctx, churn) || !st.Has(ctx, eph) {
		t.Fatalf("head not flushed after the lag: %+v", h)
	}
}

// A committed head reaches the store by the flush deadline, churn pages
// included; a churn page superseded before the deadline is discarded
// without upload.
func TestSlotHeadFlushDeadline(t *testing.T) {
	ctx := context.Background()
	deadline := 2500 * time.Millisecond
	w, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{WritebackInterval: 10 * time.Millisecond, FlushDeadline: deadline})
	w.grants.put("repo", *gr)
	if err := w.connectStore("repo", nil); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-time.Hour)
	churn := "target/debug/incremental/a-1/s-1/q.bin"
	slot, _ := runSession(t, w, func(data string) {
		put(t, filepath.Join(data, churn), randBytes(1<<20), mt)
	})
	begin := time.Now()
	w.maintainSlot(ctx, ref(t, w, slot), "idle")
	r, _ := w.cfs.repo("repo")
	_, _, remote, _ := r.stack()
	st := anyfs.NewStore(remote, anyfs.StoreMedium())
	key := func() anyfs.Key {
		m, _ := readMeta(slot)
		ch, _ := r.chain(ctx, m.Head)
		n, _ := ch.Tree().Get(churn)
		return n.Pages[0].Key
	}
	old := key()
	// The next session rewrites the churn file before the deadline.
	lease, _, err := w.slots.lease("repo", 24*time.Hour)
	if err != nil || lease != slot {
		t.Fatalf("lease %s %v", lease, err)
	}
	put(t, filepath.Join(slot, "data", churn), randBytes(1<<20), mt.Add(time.Minute))
	time.Sleep(1100 * time.Millisecond)
	w.slots.release(slot)
	w.maintainSlot(ctx, ref(t, w, slot), "idle")
	cur := key()
	if cur == old {
		t.Fatal("churn page not rewritten")
	}
	m, _ := readMeta(slot)
	for {
		if h := w.cfs.flushedHeads(); len(h) == 1 && h[0].Layer == m.Head {
			break
		}
		if time.Since(begin) > deadline+2*time.Second {
			t.Fatal("head not flushed by the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	lag := time.Since(begin)
	t.Logf("store head moved %.2fs after the first unflushed commit (deadline %s)", lag.Seconds(), deadline)
	if lag < deadline-100*time.Millisecond {
		t.Fatalf("head flushed before the deadline while churn was dirty (%s)", lag)
	}
	if !st.Has(ctx, cur) {
		t.Fatal("churn page of the committed head not in the store")
	}
	if st.Has(ctx, old) || r.local.Has(ctx, old) {
		t.Fatal("superseded churn page uploaded or kept")
	}
}

// Class hints of committed pages survive a worker restart: the new
// worker's link still holds churn and ephemeral pages back and keeps them
// local-only.
func TestSlotHintsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	cache, storeDir := t.TempDir(), t.TempDir()
	w, _ := fsWorkerAt(t, fsPlain, cache, storeDir, cacheFSOptions{})
	mt := time.Now().Add(-time.Hour)
	churn, eph := "target/debug/incremental/a-1/s-1/q.bin", "target/tmp/scratch.bin"
	slot, _ := runSession(t, w, func(data string) {
		put(t, filepath.Join(data, churn), randBytes(1<<20), mt)
		put(t, filepath.Join(data, eph), randBytes(1<<20), mt)
	})
	if _, err := w.commitSlot(ctx, ref(t, w, slot)); err != nil {
		t.Fatal(err)
	}
	r, _ := w.cfs.repo("repo")
	m, _ := readMeta(slot)
	ch, _ := r.chain(ctx, m.Head)
	key := func(p string) anyfs.Key { n, _ := ch.Tree().Get(p); return n.Pages[0].Key }
	want := map[anyfs.Key]anyfs.PageHint{}
	for _, p := range []string{churn, eph} {
		h, ok := r.local.PageHint(key(p))
		if !ok || h == (anyfs.PageHint{}) {
			t.Fatalf("%s: no hint before the restart", p)
		}
		want[key(p)] = h
	}
	w.cfs.unmountAll()

	w2, gr := fsWorkerAt(t, fsPlain, cache, storeDir, cacheFSOptions{WritebackInterval: 10 * time.Millisecond})
	r2, _ := w2.cfs.repo("repo")
	for k, h := range want {
		if got, _ := r2.local.PageHint(k); got != h {
			t.Fatalf("hint %+v after restart, want %+v", got, h)
		}
		if s, _ := r2.local.State(k); s != anyfs.Dirty {
			t.Fatal("page not dirty after restart")
		}
	}
	w2.grants.put("repo", *gr)
	if err := w2.connectStore("repo", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // writeback passes of the new link
	_, _, remote, _ := r2.stack()
	st := anyfs.NewStore(remote, anyfs.StoreMedium())
	for k := range want {
		if st.Has(ctx, k) {
			t.Fatal("restarted worker wrote back a churn/ephemeral page early")
		}
	}
}

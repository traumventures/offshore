package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
)

// A repository a maintenance pass is busy with (a slow flush) delays
// neither the maintenance of the others nor the pass itself.
func TestMaintainSlotsPerRepository(t *testing.T) {
	w, _ := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	mt := time.Now().Add(-time.Hour)
	var slots []string
	for _, repo := range []string{"repo", "other"} {
		slot, _, err := w.slots.lease(repo, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.cfs.prepare(t.Context(), w.slots, slot, repo, 24*time.Hour); err != nil {
			t.Fatal(err)
		}
		put(t, filepath.Join(slot, "data", "f"), []byte(repo), mt)
		w.slots.release(slot)
		w.slots.update(slot, func(m *slotMeta) { m.LastUsed = time.Now().Add(-time.Hour).Unix() })
		slots = append(slots, slot)
	}
	if !w.claimMaint("repo") {
		t.Fatal("claim")
	}
	begin := time.Now()
	w.maintainSlots(t.Context())
	if time.Since(begin) > 10*time.Second {
		t.Fatalf("pass took %s", time.Since(begin))
	}
	m0, _ := readMeta(slots[0])
	m1, _ := readMeta(slots[1])
	if !m0.uncommitted() || m1.uncommitted() {
		t.Fatalf("busy repository maintained (%v) or the other not (%v)", !m0.uncommitted(), m1.uncommitted())
	}
	w.endMaint("repo")
	w.maintainSlots(t.Context())
	if m0, _ = readMeta(slots[0]); m0.uncommitted() {
		t.Fatal("repository not maintained once free")
	}
}

// A session ending while the previous session's snapshot commits is not
// left to the idle commit: its release commits the live slot right after.
func TestQueuedSessionCommittedRightAway(t *testing.T) {
	w, _ := fsWorker(t, fsUser, t.TempDir(), cacheFSOptions{Idle: time.Hour})
	mt := time.Now().Add(-time.Hour)
	slot := startSession(t, w)
	put(t, filepath.Join(slot, "data", "s1"), []byte("session 1"), mt)
	lk := w.cfs.snapLock(slot)
	lk.Lock() // an earlier snapshot is committing
	time.Sleep(1100 * time.Millisecond)
	w.slots.update(slot, func(m *slotMeta) { m.Runs++ })
	if err := w.snapshotSlot(t.Context(), slot, "repo"); err != nil {
		t.Fatal(err)
	}
	w.slots.release(slot)
	if len(pendingSnapshots(slot)) != 0 {
		t.Fatal("captured while a commit runs")
	}
	done := make(chan struct{})
	go func() { w.slotReleased(slot, "repo"); close(done) }()
	time.Sleep(200 * time.Millisecond)
	lk.Unlock() // the earlier commit is done
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("release did not finish")
	}
	m, _ := readMeta(slot)
	if m.uncommitted() || !headTree(t, w, slot)["s1"] {
		t.Fatalf("queued session not committed: %+v", m)
	}
}

// The store head moves as soon as the pages the committed head
// references are clean (here written back by their class at the next
// writeback pass), long before the flush deadline and without the
// maintenance tick. It also measures session end to flushed head.
func TestHeadMovesWhenItsPagesAreClean(t *testing.T) {
	deadline := 20 * time.Second
	w, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{WritebackInterval: 50 * time.Millisecond, FlushDeadline: deadline})
	w.grants.put("repo", *gr)
	if err := w.connectStore("repo", nil); err != nil {
		t.Fatal(err)
	}
	old := headCheckInterval
	headCheckInterval = 100 * time.Millisecond
	defer func() { headCheckInterval = old }()
	mt := time.Now().Add(-time.Hour)
	slot, _ := runSession(t, w, func(data string) {
		put(t, filepath.Join(data, "target/debug/deps/libx-0123456789abcdef.rlib"), randBytes(1<<20), mt)
	})
	r, _ := w.cfs.repo("repo")
	end := time.Now()
	res, err := w.commitSlot(t.Context(), ref(t, w, slot))
	if err != nil || !res.changed {
		t.Fatalf("commit %v changed %v", err, res.changed)
	}
	// Every page of the head is written back early (immutable class).
	for _, k := range res.chain.Tree().Keys() {
		r.local.Hint(k, anyfs.PageHint{Expire: -1})
	}
	w.scheduleHeadFlush("repo")
	m, _ := readMeta(slot)
	for !w.headFlushed("repo", m.Head) {
		if time.Since(end) > deadline/2 {
			t.Fatalf("head not moved %s after the session (deadline %s)", time.Since(end), deadline)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("session end -> flushed head: %.2fs (flush deadline %s)", time.Since(end).Seconds(), deadline)
}

// The deadline flush uploads the head's pages before it takes the
// repository's commit lock: a commit holding it does not hold up the
// upload, only the publication of the head.
func TestFlushUploadsOutsideTheCommitLock(t *testing.T) {
	w, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	w.grants.put("repo", *gr)
	if err := w.connectStore("repo", nil); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-time.Hour)
	slot, _ := runSession(t, w, func(data string) {
		put(t, filepath.Join(data, "target/debug/incremental/a-1/s-1/q.bin"), randBytes(1<<20), mt)
	})
	res, err := w.commitSlot(t.Context(), ref(t, w, slot))
	if err != nil {
		t.Fatal(err)
	}
	r := res.repo
	if res.tree == nil {
		res.tree = res.chain.Tree()
	}
	if w.cfs.dirtyIn(r, res.tree) == 0 {
		t.Fatal("no dirty page to upload")
	}
	r.cmu.Lock() // a commit runs
	done := make(chan error, 1)
	go func() { _, err := w.cfs.flush(context.Background(), res); done <- err }()
	begin := time.Now()
	for w.cfs.dirtyIn(r, res.tree) > 0 {
		select {
		case err := <-done:
			t.Fatalf("flush returned under the lock: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Since(begin) > 20*time.Second {
			r.cmu.Unlock()
			t.Fatal("pages not uploaded while the commit lock is held")
		}
	}
	if w.headFlushed("repo", res.chain.Top().ID) {
		t.Fatal("head published without the lock")
	}
	r.cmu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !w.headFlushed("repo", res.chain.Top().ID) {
		t.Fatal("head not published")
	}
}

package anyfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
)

var ctx = context.Background()

func page(n int) ([]byte, Key) {
	b := make([]byte, n)
	rand.Read(b)
	return b, KeyOf(b)
}

func openLocal(t *testing.T, dir string, cap int64) *Local {
	t.Helper()
	l, err := OpenLocal(LocalOptions{Dir: dir, Capacity: cap})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestLocalScanResistant(t *testing.T) {
	const sz = 1000
	l := openLocal(t, t.TempDir(), 10*sz)
	var hot []Key
	for i := 0; i < 5; i++ {
		b, k := page(sz)
		l.Put(ctx, k, b, Clean)
		l.Get(ctx, k) // touched twice: frequent (T2)
		hot = append(hot, k)
	}
	// A scan streams 50 new pages through the tier once.
	for i := 0; i < 50; i++ {
		b, k := page(sz)
		if err := l.Put(ctx, k, b, Clean); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range hot {
		if !l.Has(ctx, k) {
			t.Fatal("scan evicted a hot page")
		}
	}
	if s := l.Stats(); s.ResidentBytes > 10*sz || s.ReclaimBytes == 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestLocalDirtyNeverReclaimedAndJournal(t *testing.T) {
	dir := t.TempDir()
	l := openLocal(t, dir, 2000)
	b1, k1 := page(1000)
	b2, k2 := page(1000)
	b3, k3 := page(1000)
	l.Put(ctx, k1, b1, Dirty)
	l.Put(ctx, k2, b2, Dirty)
	l.Put(ctx, k3, b3, Clean) // over capacity: only the clean page can go
	if !l.Has(ctx, k1) || !l.Has(ctx, k2) || l.Has(ctx, k3) {
		t.Fatal("dirty pages must stay, the clean one is reclaimed")
	}
	l.MarkClean(k1)
	l.Close()
	l2 := openLocal(t, dir, 2000)
	if s, _ := l2.State(k2); s != Dirty {
		t.Fatal("dirty state lost across reopen")
	}
	if s, _ := l2.State(k1); s != Clean {
		t.Fatal("clean page reopened dirty")
	}
	if d := l2.Stats().DirtyBytes; d != 1000 {
		t.Fatalf("dirty bytes %d", d)
	}
}

func TestLocalThrashDetection(t *testing.T) {
	var mu sync.Mutex
	var evs []Event
	l, err := OpenLocal(LocalOptions{Dir: t.TempDir(), Capacity: 4000, Observer: func(e Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var pages [][]byte
	for i := 0; i < 8; i++ {
		b, _ := page(1000)
		pages = append(pages, b)
	}
	// A working set of 8 pages cycles through a 4 page tier.
	for round := 0; round < 3; round++ {
		for _, b := range pages {
			k := KeyOf(b)
			if _, err := l.Get(ctx, k); err != nil {
				l.Put(ctx, k, b, Clean)
			}
		}
	}
	s := l.Stats()
	if !s.Thrashing || s.Refaults == 0 || s.WorkingSetBytes < 8000 {
		t.Fatalf("expected thrashing: %+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, e := range evs {
		found = found || e.Type == EvThrash
	}
	if !found {
		t.Fatal("no thrash event")
	}
}

func dirStore(t *testing.T) *Store {
	ds, err := blobcache.NewDirStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(ds, StoreMedium())
}

func TestLinkWriteBackFlushDiscard(t *testing.T) {
	up := openLocal(t, t.TempDir(), 0)
	st := dirStore(t)
	var mu sync.Mutex
	var wb int64
	ln := NewLink(up, st, Policy{DirtyExpire: time.Hour}, func(e Event) {
		if e.Type == EvWriteback {
			mu.Lock()
			wb += e.Pages
			mu.Unlock()
		}
	})
	b1, k1 := page(100)
	b2, k2 := page(100)
	ln.Put(ctx, k1, b1, Dirty)
	ln.Put(ctx, k2, b2, Dirty)
	if st.Has(ctx, k1) {
		t.Fatal("write-back must not write through")
	}
	ln.Discard(k2) // deleted before flush: never written
	if err := ln.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !st.Has(ctx, k1) || st.Has(ctx, k2) {
		t.Fatal("flush wrote the wrong pages")
	}
	if s, _ := up.State(k1); s != Clean {
		t.Fatal("written back page still dirty")
	}
	if wb != 1 || ln.Dirty() != 0 {
		t.Fatalf("writebacks %d dirty %d", wb, ln.Dirty())
	}
}

func TestLinkExpiryWriteback(t *testing.T) {
	up := openLocal(t, t.TempDir(), 0)
	st := dirStore(t)
	ln := NewLink(up, st, Policy{DirtyExpire: 10 * time.Millisecond, Interval: 5 * time.Millisecond}, nil)
	ln.Start()
	defer ln.Close()
	b, k := page(100)
	ln.Put(ctx, k, b, Dirty)
	deadline := time.Now().Add(2 * time.Second)
	for !st.Has(ctx, k) {
		if time.Now().After(deadline) {
			t.Fatal("expired dirty page not written back")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLinkFaultPromotesAndCounts(t *testing.T) {
	up := openLocal(t, t.TempDir(), 0)
	st := dirStore(t)
	b, k := page(100)
	st.Put(ctx, k, b, Clean)
	ln := NewLink(up, st, Policy{}, nil)
	got, err := ln.Get(ctx, k)
	if err != nil || !bytes.Equal(got, b) {
		t.Fatal(err)
	}
	if !up.Has(ctx, k) {
		t.Fatal("fault did not promote the page")
	}
	ln.Get(ctx, k)
	if s := ln.Stats(); s.Faults != 1 || s.PageIns != 1 || s.ResidentBytes != 100 {
		t.Fatalf("stats %+v", s)
	}
	if _, err := ln.Get(ctx, KeyOf([]byte("missing"))); err != ErrNotFound {
		t.Fatalf("missing page: %v", err)
	}
}

func TestExclusiveLinkDemotes(t *testing.T) {
	up := openLocal(t, t.TempDir(), 1000)
	vol := openLocal(t, t.TempDir(), 0)
	ln := NewLink(up, vol, Policy{Inclusion: Exclusive}, nil)
	b1, k1 := page(600)
	vol.Put(ctx, k1, b1, Clean)
	if _, err := ln.Get(ctx, k1); err != nil {
		t.Fatal(err)
	}
	if vol.Has(ctx, k1) || !up.Has(ctx, k1) {
		t.Fatal("exclusive fault must move the page up")
	}
	b2, k2 := page(600)
	vol.Put(ctx, k2, b2, Clean)
	ln.Get(ctx, k2) // reclaims k1 from the upper tier: demoted
	if !vol.Has(ctx, k1) || up.Has(ctx, k1) {
		t.Fatal("reclaimed page not demoted")
	}
}

func TestFoldThreeTiers(t *testing.T) {
	local := openLocal(t, t.TempDir(), 0)
	vol := openLocal(t, t.TempDir(), 0)
	st := dirStore(t)
	stack, err := Fold(nil, []Policy{{DirtyExpire: time.Hour}, {DirtyExpire: time.Hour}}, local, vol, st)
	if err != nil {
		t.Fatal(err)
	}
	b, k := page(100)
	stack.Put(ctx, k, b, Dirty)
	if vol.Has(ctx, k) || st.Has(ctx, k) {
		t.Fatal("write-back stack wrote through")
	}
	if err := stack.(Flusher).Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !st.Has(ctx, k) {
		t.Fatal("flush did not reach the store")
	}
	if p := stack.Profile(); p.String() != "local>local>store" || len(Links(stack)) != 2 {
		t.Fatalf("profile %s", p)
	}
}

func TestScanPacksAndFilesAssemble(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "deps"), 0o755)
	small := []byte("fingerprint")
	big := make([]byte, PageSize+123)
	rand.Read(big)
	os.WriteFile(filepath.Join(src, "deps", "a.fp"), small, 0o644)
	os.WriteFile(filepath.Join(src, "deps", "b.rlib"), big, 0o644)
	os.WriteFile(filepath.Join(src, "empty"), nil, 0o644)
	os.Symlink("deps/b.rlib", filepath.Join(src, "link"))
	up := openLocal(t, t.TempDir(), 0)
	sink := func(k Key, b []byte) error { return up.Put(ctx, k, b, Dirty) }
	tr, st, err := Scan(src, ScanOptions{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 || st.Pages != 3 { // one pack + two pieces of the big file
		t.Fatalf("stats %+v", st)
	}
	if l, _ := tr.Get("link"); l.Target != "deps/b.rlib" {
		t.Fatal("symlink")
	}
	// Rescan reuses everything.
	_, st2, err := Scan(src, ScanOptions{Prev: tr, Sink: sink})
	if err != nil || st2.Reused != 3 || st2.Read != 0 {
		t.Fatalf("rescan %+v %v", st2, err)
	}
	files, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"deps/a.fp", "deps/b.rlib", "empty"} {
		n, _ := tr.Get(p)
		fp, err := files.Ensure(ctx, up, n)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(fp)
		want, _ := os.ReadFile(filepath.Join(src, p))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: content differs", p)
		}
	}
	if files.Stats().ResidentBytes != int64(len(small)+len(big)) {
		t.Fatalf("files %+v", files.Stats())
	}
	if freed := files.Reclaim(0); freed != int64(len(small)+len(big)) {
		t.Fatalf("reclaim freed %d", freed)
	}
}

func TestLayerChainStore(t *testing.T) {
	st := dirStore(t).Objects()
	now := time.Unix(1000, 0)
	t1 := NewTree([]Node{d("t"), f("t/a", 1, "a"), f("t/b", 1, "b")})
	l1 := Commit(nil, NewTree(nil), t1, now, nil)
	t2 := NewTree([]Node{d("t"), f("t/a", 2, "a2"), f("t/c", 1, "c")})
	l2 := Commit(Chain{l1}, t1, t2, now.Add(time.Hour), []byte("meta"))
	for _, l := range []*Layer{l1, l2} {
		if err := SaveLayer(ctx, st, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetHead(ctx, st, "repo", l2.ID); err != nil {
		t.Fatal(err)
	}
	id, err := Head(ctx, st, "repo")
	if err != nil || id != l2.ID {
		t.Fatal(id, err)
	}
	c, err := LoadChain(ctx, st, id)
	if err != nil || len(c) != 2 {
		t.Fatal(len(c), err)
	}
	if got := c.Tree(); !reflectPaths(got, t2) || !c.Oldest().Equal(now) || l2.Created != now.Unix() {
		t.Fatalf("chain tree %v oldest %v", paths(got), c.Oldest())
	}
	base := Compact(c, now.Add(2*time.Hour))
	if !base.Base || base.Created != now.Unix() || string(base.Meta) != "meta" || !reflectPaths(Merge(base.Nodes), t2) {
		t.Fatalf("compact %+v", base)
	}
}

func reflectPaths(a, b *Tree) bool {
	pa, pb := paths(a), paths(b)
	if len(pa) != len(pb) {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return false
		}
	}
	return true
}

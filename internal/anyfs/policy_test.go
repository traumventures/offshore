package anyfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Local tiers sharing a pool hold a page once (hardlinks); the pool entry
// goes with the last tier holding the page.
func TestLocalPoolDedupe(t *testing.T) {
	pool := t.TempDir()
	open := func() *Local {
		l, err := OpenLocal(LocalOptions{Dir: t.TempDir(), Pool: pool})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	}
	a, b := open(), open()
	p, k := page(5000)
	must(t, a.Put(ctx, k, p, Dirty))
	must(t, b.Put(ctx, k, p, Dirty))
	if a.Stats().SharedBytes != 0 || b.Stats().SharedBytes != 5000 {
		t.Fatalf("shared a %d b %d", a.Stats().SharedBytes, b.Stats().SharedBytes)
	}
	fa, _ := os.Stat(a.Path(k))
	fb, _ := os.Stat(b.Path(k))
	if !os.SameFile(fa, fb) {
		t.Fatal("page stored twice")
	}
	pp := filepath.Join(pool, string(k[:2]), string(k))
	a.Remove(k)
	if _, err := os.Stat(pp); err != nil {
		t.Fatal("pool entry dropped while b holds the page")
	}
	if got, err := b.Get(ctx, k); err != nil || len(got) != 5000 {
		t.Fatal(err)
	}
	b.Remove(k)
	if _, err := os.Stat(pp); !os.IsNotExist(err) {
		t.Fatal("pool entry kept after the last tier dropped the page")
	}
}

// The periodic writeback follows the pages' hints: early pages go at the
// next pass, NoWriteback pages only with an explicit flush, and LocalOnly
// pages are never demoted.
func TestLinkHintedWriteback(t *testing.T) {
	up := openLocal(t, t.TempDir(), 0)
	st := dirStore(t)
	ln := NewLink(up, st, Policy{DirtyExpire: time.Hour, Interval: 5 * time.Millisecond}, nil)
	early, ke := page(100)
	never, kn := page(100)
	late, kl := page(100)
	for k, b := range map[Key][]byte{ke: early, kn: never, kl: late} {
		must(t, ln.Put(ctx, k, b, Dirty))
	}
	up.Hint(ke, PageHint{Expire: -1})
	up.Hint(kn, PageHint{NoWriteback: true, LocalOnly: true})
	ln.Start()
	defer ln.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !st.Has(ctx, ke) {
		if time.Now().After(deadline) {
			t.Fatal("early page not written back")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if st.Has(ctx, kn) || st.Has(ctx, kl) {
		t.Fatal("delayed pages written back early")
	}
	must(t, ln.Flush(ctx))
	if !st.Has(ctx, kn) || !st.Has(ctx, kl) {
		t.Fatal("flush skipped hinted pages")
	}
	// LocalOnly: reclaim does not demote.
	var demoted []Key
	up.SetDemote(func(k Key, _ []byte) { demoted = append(demoted, k) })
	up.Reclaim(ctx, 0)
	for _, k := range demoted {
		if k == kn {
			t.Fatal("local-only page demoted")
		}
	}
	if len(demoted) != 2 {
		t.Fatalf("demoted %d pages, want 2", len(demoted))
	}
}

// Alone files get a page of their own keyed by their digest: the same
// content packed among different neighbours is the same page.
func TestScanAloneSameKey(t *testing.T) {
	content := make([]byte, 40<<10)
	for i := range content {
		content[i] = byte(i * 7)
	}
	keys := func(neighbour string) Key {
		d := t.TempDir()
		must(t, os.WriteFile(filepath.Join(d, "a"), []byte(neighbour), 0o644))
		must(t, os.WriteFile(filepath.Join(d, "lib.rlib"), content, 0o644))
		tr, _, err := Scan(d, ScanOptions{Sink: func(Key, []byte) error { return nil },
			Alone: func(rel string, size int64) bool { return rel == "lib.rlib" }})
		must(t, err)
		n, _ := tr.Get("lib.rlib")
		if len(n.Pages) != 1 || n.Pages[0].Off != 0 || string(n.Pages[0].Key) != n.Digest {
			t.Fatalf("pages %+v", n.Pages)
		}
		return n.Pages[0].Key
	}
	if keys("x") != keys("something else") {
		t.Fatal("same content, different page keys")
	}
}

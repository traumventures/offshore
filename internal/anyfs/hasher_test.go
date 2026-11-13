package anyfs

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
)

func writeRand(t *testing.T, p string, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

type memSink struct {
	mu    sync.Mutex
	pages map[Key][]byte
}

func (m *memSink) put(k Key, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pages == nil {
		m.pages = map[Key][]byte{}
	}
	if KeyOf(b) != k {
		return fmt.Errorf("bad key")
	}
	m.pages[k] = append([]byte(nil), b...)
	return nil
}

func TestHasherLookup(t *testing.T) {
	dir := t.TempDir()
	sink := &memSink{}
	h := NewHasher(HasherOptions{Sink: sink.put, Settle: time.Millisecond, MinSize: 1})
	defer h.Close()
	big := writeRand(t, filepath.Join(dir, "a/big"), 3*PageSize+17)
	writeRand(t, filepath.Join(dir, "b"), 1000)
	time.Sleep(5 * time.Millisecond) // settle
	h.Queue("a/big", filepath.Join(dir, "a/big"))
	h.Queue("b", filepath.Join(dir, "b"))
	waitHashed(t, h, 2)

	fi, _ := os.Lstat(filepath.Join(dir, "a/big"))
	n := &Node{Path: "a/big", Kind: File, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	if !h.Lookup(n, filepath.Join(dir, "a/big")) {
		t.Fatal("no hit")
	}
	if n.Digest != string(KeyOf(big)) || len(n.Pages) != 4 {
		t.Fatalf("digest/pages %s %d", n.Digest, len(n.Pages))
	}
	var got []byte
	for _, p := range n.Pages {
		got = append(got, sink.pages[p.Key][p.Off:p.Off+p.Len]...)
	}
	if string(got) != string(big) {
		t.Fatal("pages do not hold the content")
	}
	if len(h.Keys()) != 5 {
		t.Fatalf("keys %d", len(h.Keys()))
	}
	// A rewrite changes the identity: the entry is stale.
	writeRand(t, filepath.Join(dir, "b"), 1000)
	fi, _ = os.Lstat(filepath.Join(dir, "b"))
	m := &Node{Path: "b", Kind: File, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	if h.Lookup(m, filepath.Join(dir, "b")) {
		t.Fatal("hit on a rewritten file")
	}
	h.Release([]string{"a/big"}, true)
	if len(h.Keys()) != 0 {
		t.Fatalf("keys after release %d", len(h.Keys()))
	}
	if s := h.Stats(); s.Hits != 1 || s.Stale != 1 || s.Hashed != 2 {
		t.Fatalf("stats %+v", s)
	}
}

func TestHasherWaitsForSettle(t *testing.T) {
	dir := t.TempDir()
	h := NewHasher(HasherOptions{Settle: 200 * time.Millisecond, MinSize: 1})
	defer h.Close()
	p := filepath.Join(dir, "f")
	writeRand(t, p, 100)
	h.Queue("f", p)
	time.Sleep(50 * time.Millisecond)
	if h.Stats().Hashed != 0 {
		t.Fatal("hashed before settled")
	}
	waitHashed(t, h, 1)
}

func TestHasherSkipAndHeld(t *testing.T) {
	dir := t.TempDir()
	h := NewHasher(HasherOptions{Settle: time.Millisecond, MinSize: 1, MaxEntries: 1,
		Skip: func(rel string) bool { return rel == "tmp" }})
	defer h.Close()
	for _, f := range []string{"tmp", "x", "y"} {
		writeRand(t, filepath.Join(dir, f), 10)
	}
	time.Sleep(5 * time.Millisecond)
	h.Queue("tmp", filepath.Join(dir, "tmp"))
	h.Queue("x", filepath.Join(dir, "x"))
	waitHashed(t, h, 1)
	fi, _ := os.Lstat(filepath.Join(dir, "x"))
	n := &Node{Path: "x", Kind: File, Size: 10, MtimeNs: fi.ModTime().UnixNano()}
	if !h.Lookup(n, filepath.Join(dir, "x")) {
		t.Fatal("no hit")
	}
	h.Queue("y", filepath.Join(dir, "y"))
	waitHashed(t, h, 2)
	// x is held by a commit: y does not evict it.
	n2 := &Node{Path: "x", Kind: File, Size: 10, MtimeNs: fi.ModTime().UnixNano()}
	if !h.Lookup(n2, filepath.Join(dir, "x")) {
		t.Fatal("held entry evicted")
	}
	if h.Stats().Queued != 2 {
		t.Fatalf("skip did not apply: %+v", h.Stats())
	}
}

func waitHashed(t *testing.T, h *Hasher, n int64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if h.Stats().Hashed >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("hashed %+v, want %d", h.Stats(), n)
}

// Parallel reads produce the same tree and pages as sequential ones.
func TestScanWorkersDeterministic(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 300; i++ {
		writeRand(t, filepath.Join(dir, fmt.Sprintf("d%d/f%03d", i%7, i)), (i*7919)%(300<<10)+1)
	}
	writeRand(t, filepath.Join(dir, "large1"), 2*PageSize+5)
	writeRand(t, filepath.Join(dir, "large2"), SmallFile+1)
	var trees []*Tree
	var sinks []*memSink
	for _, w := range []int{1, 8} {
		s := &memSink{}
		tr, st, err := Scan(dir, ScanOptions{Sink: s.put, Workers: w, Alone: func(rel string, size int64) bool { return rel == "d1/f001" }})
		if err != nil {
			t.Fatal(err)
		}
		if st.Hashed != 302 || st.Pages != int64(len(s.pages)) {
			t.Fatalf("workers %d: stats %+v, %d pages", w, st, len(s.pages))
		}
		trees, sinks = append(trees, tr), append(sinks, s)
	}
	if !reflect.DeepEqual(trees[0].Nodes(), trees[1].Nodes()) {
		t.Fatal("trees differ")
	}
	if len(sinks[0].pages) != len(sinks[1].pages) {
		t.Fatal("pages differ")
	}
}

func TestChangeSetResolve(t *testing.T) {
	dir := t.TempDir()
	a := writeRand(t, filepath.Join(dir, "a"), 5000)
	writeRand(t, filepath.Join(dir, "b"), 6000)
	cs, err := PlanChanges(dir, ChangeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resolved := cs.Resolve(func(n *Node) bool {
		if n.Path != "a" {
			return false
		}
		n.Digest = string(KeyOf(a))
		return true
	})
	if !reflect.DeepEqual(resolved, []string{"a"}) || !reflect.DeepEqual(cs.Read, []string{"b"}) || cs.Stats.KnownRead != 5000 {
		t.Fatalf("resolved %v read %v stats %+v", resolved, cs.Read, cs.Stats)
	}
	s := &memSink{}
	tr, st, err := cs.Tree(dir, ScanOptions{Sink: s.put})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := tr.Get("a"); n.Digest != string(KeyOf(a)) || st.Read != 6000 {
		t.Fatalf("tree a %+v read %d", n, st.Read)
	}
}

func TestLinkStream(t *testing.T) {
	ds, err := blobcache.NewDirStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	local := openLocal(t, t.TempDir(), 0)
	store := NewStore(ds, StoreMedium())
	l := NewLink(local, store, Policy{Parallel: 2}, nil)
	s := l.Stream(ctx)
	var keys []Key
	for i := 0; i < 10; i++ {
		b, k := page(1000)
		if err := s.Put(k, b); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	st := s.Wait()
	if st.Pages != 10 || st.Err != nil || l.Dirty() != 0 {
		t.Fatalf("stream %+v dirty %d", st, l.Dirty())
	}
	for _, k := range keys {
		if !store.Has(ctx, k) {
			t.Fatal("page not in store")
		}
		if s, _ := local.State(k); s != Clean {
			t.Fatal("page still dirty")
		}
	}
}

// Large files page the same with the concurrent packer as serially, and
// a failing sink stops it.
func TestPackerLargeSerialEqual(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big")
	writeRand(t, p, 3*PageSize+12345)
	pack := func(pk *Packer) Node {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var n Node
		if err := pk.AddLargeFile(&n, f, 1, f); err != nil {
			t.Fatal(err)
		}
		return n
	}
	s1, s2 := &memSink{}, &memSink{}
	a, b := pack(NewPacker(s1.put)), pack(NewPacker(s2.put).Serial())
	if !reflect.DeepEqual(a, b) || len(a.Pages) != 4 || len(s1.pages) != 4 || !reflect.DeepEqual(s1.pages, s2.pages) {
		t.Fatalf("concurrent %+v serial %+v", a, b)
	}
	f, _ := os.Open(p)
	defer f.Close()
	calls := 0
	err := NewPacker(func(Key, []byte) error { calls++; return fmt.Errorf("full") }).AddLargeFile(&Node{}, f, 1, f)
	if err == nil || calls != 1 {
		t.Fatalf("err %v after %d puts", err, calls)
	}
}

// The rate limit applies only under IO pressure; Forget drops queued
// files.
func TestHasherPressureAndForget(t *testing.T) {
	var p float64
	h := NewHasher(HasherOptions{BytesPerSec: 1, Settle: time.Hour, Pressure: func() (float64, bool) { return p, true }})
	defer h.Close()
	if h.underPressure() {
		t.Fatal("no pressure, limited")
	}
	p = 50
	h.pressuredAt = time.Time{}
	if !h.underPressure() {
		t.Fatal("pressure, unlimited")
	}
	h.Queue("a", "/nonexistent/a")
	h.Queue("b", "/nonexistent/b")
	h.Forget([]string{"a"})
	if st := h.Stats(); st.Pending != 1 {
		t.Fatalf("pending %d", st.Pending)
	}
}

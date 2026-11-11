package anyfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// peer is a fake original: content by digest, with a fetch counter.
type peer struct {
	content map[string]string // digest -> content
	fetches atomic.Int64
}

func (p *peer) fetch(_ context.Context, n *Node, w io.Writer) error {
	p.fetches.Add(1)
	c, ok := p.content[n.Digest]
	if !ok {
		return errors.New("changed on the client")
	}
	_, err := io.WriteString(w, c)
	return err
}

func (p *peer) file(path, content string) Node {
	d := string(KeyOf([]byte(content)))
	if p.content == nil {
		p.content = map[string]string{}
	}
	p.content[d] = content
	return Node{Path: path, Kind: File, Mode: 0o644, Size: int64(len(content)), Digest: d, MtimeNs: 1e9}
}

func dir(path string) Node { return Node{Path: path, Kind: Dir, Mode: 0o755} }

func TestTreeSourceApplyReportsChangedPaths(t *testing.T) {
	p := &peer{}
	ts := NewPeerSource(NewTree([]Node{dir("a"), p.file("a/x", "x"), dir("a/d"), p.file("a/d/y", "y"), p.file("b", "b")}), nil, p.fetch)
	got := ts.Apply([]Node{{Path: "a", Whiteout: true}, p.file("c", "c"), p.file("b", "b2")}, false)
	sort.Strings(got)
	want := []string{"a", "a/d", "a/d/y", "a/x", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("changed %v, want %v", got, want)
	}
	if _, ok := ts.Node("a/x"); ok {
		t.Fatal("a/x survived the whiteout of a")
	}
	if n, _ := ts.Node("b"); n.Size != 2 {
		t.Fatalf("b not replaced: %+v", n)
	}
	got = ts.Apply([]Node{p.file("z", "z")}, true)
	sort.Strings(got)
	if !slices.Equal(got, []string{"b", "c", "z"}) {
		t.Fatalf("full apply changed %v", got)
	}
}

func TestFilesEnsureWithVerifiesAndShares(t *testing.T) {
	files, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{}
	n := p.file("f", "hello")
	var calls atomic.Int64
	slow := func(ctx context.Context, n *Node, w io.Writer) error {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return p.fetch(ctx, n, w)
	}
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			path, err := files.EnsureWith(ctx, &n, slow)
			if err == nil {
				var b []byte
				if b, err = os.ReadFile(path); err == nil && string(b) != "hello" {
					err = errors.New("wrong content " + string(b))
				}
			}
			errs <- err
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d fetches for one digest", calls.Load())
	}
	bad := n
	bad.Path, bad.Digest = "g", string(KeyOf([]byte("other")))
	p.content[bad.Digest] = "not other"
	if _, err := files.EnsureWith(ctx, &bad, p.fetch); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("mismatching content accepted: %v", err)
	}
	if _, ok := files.Local(&bad); ok {
		t.Fatal("mismatching content resident")
	}
}

func newWritable(t *testing.T, nodes ...Node) (*WritableFS, *peer) {
	t.Helper()
	files, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{}
	w, err := NewWritable(NewPeerSource(NewTree(nodes), files, p.fetch), t.TempDir(), WritableOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return w, p
}

func TestWritableOriginalWinsAndSettles(t *testing.T) {
	w, p := newWritable(t)
	w.Apply([]Node{dir("src"), p.file("src/a", "a1"), p.file("src/b", "b1")}, true, 1)
	if _, err := w.copyUp(ctx, "src/a", false); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.removeLocked("src/b")
	w.setDirtyLocked("src/new", &dirtyNode{Node: Node{Kind: Dir, Mode: 0o755}})
	w.mu.Unlock()
	ch := w.Changes()
	if len(ch) != 3 || ch[0].Path != "src/a" || ch[1].Path != "src/new" || !ch[2].Deleted {
		t.Fatalf("changes %+v", ch)
	}
	a, _ := w.src.Node("src/a")
	if !ch[0].BaseExists || ch[0].BaseDigest != a.Digest || ch[1].BaseExists {
		t.Fatalf("bases %+v", ch)
	}
	// The client changes src/a: its version replaces the local one.
	w.Apply([]Node{p.file("src/a", "a2")}, false, 2)
	if w.Version() != 2 {
		t.Fatalf("version %d", w.Version())
	}
	if v, _ := w.stat("src/a"); v.dirty {
		t.Fatal("local modification of src/a survived the client's change")
	}
	// The client confirms the write back of the others.
	w.Apply([]Node{{Path: "src/b", Whiteout: true}, dir("src/new")}, false, 3)
	w.Settle()
	if ch := w.Changes(); len(ch) != 0 {
		t.Fatalf("unsettled changes %+v", ch)
	}
}

func TestWritableWaitVersion(t *testing.T) {
	w, _ := newWritable(t)
	c, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := w.WaitVersion(c, 1); err == nil {
		t.Fatal("version 1 before any apply")
	}
	go func() { time.Sleep(20 * time.Millisecond); w.Apply(nil, false, 4) }()
	if err := w.WaitVersion(ctx, 3); err != nil {
		t.Fatal(err)
	}
}

func mountWritableOrSkip(t *testing.T, w *WritableFS) string {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	mnt := filepath.Join(t.TempDir(), "proj")
	if err := w.Mount(mnt); err != nil {
		t.Skipf("cannot mount FUSE: %v", err)
	}
	t.Cleanup(func() { w.Unmount() })
	return mnt
}

func TestWritableFUSE(t *testing.T) {
	files, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{}
	tree := NewTree([]Node{dir("wb"), p.file("wb/a", "old\n"), p.file("wb/b", "keep"), p.file("wb/c", "gone")})
	w, err := NewWritable(NewPeerSource(tree, files, p.fetch), t.TempDir(), WritableOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.SetMountPoints([]string{"target/debug"})
	mnt := mountWritableOrSkip(t, w)
	at := func(rel string) string { return filepath.Join(mnt, filepath.FromSlash(rel)) }

	if b, err := os.ReadFile(at("wb/a")); err != nil || string(b) != "old\n" {
		t.Fatalf("read wb/a: %q %v", b, err)
	}
	if des, err := os.ReadDir(at("target/debug")); err != nil || len(des) != 0 {
		t.Fatalf("mount point: %v %v", des, err)
	}
	f, err := os.OpenFile(at("wb/a"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("new\n")
	f.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Remove(at("wb/c")))
	must(os.MkdirAll(at("wb/d/e"), 0o755))
	must(os.WriteFile(at("wb/d/e/f"), []byte("deep"), 0o644))
	must(os.Symlink("a", at("wb/link")))
	must(os.Rename(at("wb/b"), at("wb/b2")))
	must(os.Chmod(at("wb/b2"), 0o755))
	if b, _ := os.ReadFile(at("wb/a")); string(b) != "old\nnew\n" {
		t.Fatalf("append: %q", b)
	}
	if _, err := os.Stat(at("wb/c")); !os.IsNotExist(err) {
		t.Fatalf("wb/c after remove: %v", err)
	}
	got := map[string]Change{}
	for _, c := range w.Changes() {
		got[c.Path] = c
	}
	for _, want := range []string{"wb/a", "wb/b", "wb/b2", "wb/c", "wb/d", "wb/d/e", "wb/d/e/f", "wb/link"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no change for %s (have %v)", want, got)
		}
	}
	if c := got["wb/b2"]; c.Mode != 0o755 || c.BaseExists {
		t.Errorf("wb/b2: %+v", c)
	}
	if c := got["wb/a"]; c.BaseDigest != string(KeyOf([]byte("old\n"))) {
		t.Errorf("wb/a base: %+v", c)
	}
	if b, _ := os.ReadFile(got["wb/b2"].Upper); string(b) != "keep" {
		t.Errorf("renamed content %q", b)
	}

	// A change on the client shows immediately (kernel caches dropped)
	// and replaces the local modification.
	w.Apply([]Node{p.file("wb/a", "client\n"), p.file("wb/n", "n")}, false, 1)
	if b, err := os.ReadFile(at("wb/a")); err != nil || string(b) != "client\n" {
		t.Fatalf("after client change: %q %v", b, err)
	}
	if b, err := os.ReadFile(at("wb/n")); err != nil || string(b) != "n" {
		t.Fatalf("new client file: %q %v", b, err)
	}
	// Rejected write back.
	w.Discard([]string{"wb/link"})
	if _, err := os.Lstat(at("wb/link")); !os.IsNotExist(err) {
		t.Fatalf("discarded symlink still there: %v", err)
	}
}

func BenchmarkTreeSourceApplySmallDelta(b *testing.B) {
	p := &peer{}
	var nodes []Node
	for i := range 100000 {
		nodes = append(nodes, p.file(fmt.Sprintf("d%03d/f%05d.rs", i%300, i), "x"))
	}
	for i := range 300 {
		nodes = append(nodes, dir(fmt.Sprintf("d%03d", i)))
	}
	ts := NewPeerSource(NewTree(nodes), nil, p.fetch)
	b.ResetTimer()
	for i := range b.N {
		ts.Apply([]Node{p.file("d001/f00001.rs", fmt.Sprint(i))}, false)
	}
}

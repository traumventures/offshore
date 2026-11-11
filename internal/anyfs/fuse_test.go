package anyfs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// lowerFixture scans a directory into a store-backed stack and returns
// a TreeSource over it (nothing resident yet).
func lowerFixture(t *testing.T) (*TreeSource, *Link, string) {
	t.Helper()
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "debug", "deps"), 0o755)
	os.WriteFile(filepath.Join(src, "debug", "deps", "a.fp"), []byte("fp-a"), 0o644)
	os.WriteFile(filepath.Join(src, "debug", "deps", "b.rlib"), []byte(strings.Repeat("r", 100000)), 0o644)
	os.WriteFile(filepath.Join(src, "debug", "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("deps/b.rlib", filepath.Join(src, "debug", "link"))
	mt := time.Unix(1700000000, 123456789)
	os.Chtimes(filepath.Join(src, "debug", "deps", "a.fp"), mt, mt)
	st := dirStore(t)
	tr, _, err := Scan(src, ScanOptions{Sink: func(k Key, b []byte) error { return st.Put(ctx, k, b, Dirty) }})
	if err != nil {
		t.Fatal(err)
	}
	local := openLocal(t, t.TempDir(), 0)
	ln := NewLink(local, st, Policy{}, nil)
	files, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewTreeSource(tr, files, ln), ln, src
}

func mountLowerOrSkip(t *testing.T, src Source, opt LowerOptions) *LowerFS {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	mnt := filepath.Join(t.TempDir(), "lower")
	l, err := MountLower(mnt, src, opt)
	if err != nil {
		t.Skipf("cannot mount FUSE: %v", err)
	}
	t.Cleanup(func() { l.Unmount() })
	return l
}

func TestLowerFSFaultsOnFirstRead(t *testing.T) {
	ts, ln, src := lowerFixture(t)
	l := mountLowerOrSkip(t, ts, LowerOptions{})
	mnt := l.Mountpoint()
	fi, err := os.Stat(filepath.Join(mnt, "debug", "deps", "a.fp"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().UnixNano() != 1700000000123456789 || fi.Size() != 4 {
		t.Fatalf("attrs %v %d", fi.ModTime(), fi.Size())
	}
	// Opening without reading must not fault.
	fh, err := os.Open(filepath.Join(mnt, "debug", "deps", "b.rlib"))
	if err != nil {
		t.Fatal(err)
	}
	fh.Close()
	if s := ln.Stats(); s.Faults != 0 {
		t.Fatalf("open faulted: %+v", s)
	}
	for _, p := range []string{"debug/deps/a.fp", "debug/deps/b.rlib", "debug/tool"} {
		got, err := os.ReadFile(filepath.Join(mnt, p))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(filepath.Join(src, p))
		if string(got) != string(want) {
			t.Fatalf("%s differs", p)
		}
	}
	if s := ln.Stats(); s.Faults == 0 || s.PageIns == 0 {
		t.Fatalf("reads did not fault: %+v", s)
	}
	if tgt, err := os.Readlink(filepath.Join(mnt, "debug", "link")); err != nil || tgt != "deps/b.rlib" {
		t.Fatal(tgt, err)
	}
	des, _ := os.ReadDir(filepath.Join(mnt, "debug"))
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "deps,link,tool" {
		t.Fatal(names)
	}
	if err := os.WriteFile(filepath.Join(mnt, "x"), nil, 0o644); err == nil {
		t.Fatal("lower must be read-only")
	}
}

func TestLowerFSOpsCachedByKernel(t *testing.T) {
	ts, _, _ := lowerFixture(t)
	l := mountLowerOrSkip(t, ts, LowerOptions{})
	p := filepath.Join(l.Mountpoint(), "debug", "deps", "a.fp")
	for range 5 {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p + ".missing"); !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	ops := l.Ops()
	// debug, deps, a.fp, a.fp.missing: each looked up once, then the
	// kernel answers from its entry, attribute and negative caches.
	if got := ops["lookup"].Count; got != 4 {
		t.Fatalf("lookups %d, want 4: %+v", got, ops)
	}
	if got := ops["getattr"].Count; got != 0 {
		t.Fatalf("getattr reached the daemon %d times: %+v", got, ops)
	}
}

func TestLowerFSInvalidate(t *testing.T) {
	ts, _, _ := lowerFixture(t)
	l := mountLowerOrSkip(t, ts, LowerOptions{})
	nodes := append([]Node(nil), ts.Tree().Nodes()...)
	var kept []Node
	for _, n := range nodes {
		if n.Path != "debug/tool" {
			kept = append(kept, n)
		}
	}
	l.Invalidate(ts.Update(NewTree(kept)))
	if _, err := os.Stat(filepath.Join(l.Mountpoint(), "debug", "tool")); !os.IsNotExist(err) {
		t.Fatalf("removed file still visible: %v", err)
	}
}

func TestUserOverlay(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	ts, _, _ := lowerFixture(t)
	upper := t.TempDir()
	mnt := filepath.Join(t.TempDir(), "m")
	u, err := MountUserOverlay(mnt, upper, ts, UserOverlayOptions{NoPassthrough: true})
	if err != nil {
		t.Skipf("cannot mount FUSE: %v", err)
	}
	defer u.Unmount()
	p := func(s string) string { return filepath.Join(mnt, s) }
	if b, err := os.ReadFile(p("debug/deps/a.fp")); err != nil || string(b) != "fp-a" {
		t.Fatal(string(b), err)
	}
	// Write in place: copy-up keeps the rest of the file.
	fh, err := os.OpenFile(p("debug/deps/a.fp"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteAt([]byte("FP"), 0)
	fh.Close()
	if b, _ := os.ReadFile(p("debug/deps/a.fp")); string(b) != "FP-a" {
		t.Fatalf("copy-up content %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(upper, "debug", "deps", "a.fp")); string(b) != "FP-a" {
		t.Fatal("not in upper")
	}
	// Delete a lower file: whiteout.
	if err := os.Remove(p("debug/tool")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p("debug/tool")); !os.IsNotExist(err) {
		t.Fatal("deleted lower file visible")
	}
	if _, err := os.Stat(filepath.Join(upper, "debug", WhiteoutPrefix+"tool")); err != nil {
		t.Fatal("no whiteout")
	}
	// Hardlink of a lower file (cargo uplifts artifacts this way).
	if err := os.Link(p("debug/deps/b.rlib"), p("debug/b-uplift")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p("debug/b-uplift")); err != nil || fi.Size() != 100000 {
		t.Fatal(err)
	}
	// Create, rename over, mkdir, rmdir.
	os.WriteFile(p("debug/new.tmp"), []byte("n"), 0o644)
	if err := os.Rename(p("debug/new.tmp"), p("debug/deps/a.fp")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p("debug/deps/a.fp")); string(b) != "n" {
		t.Fatal("rename over")
	}
	if err := os.Mkdir(p("incremental"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p("incremental")); err != nil {
		t.Fatal(err)
	}
	des, _ := os.ReadDir(p("debug"))
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "b-uplift,deps,link" {
		t.Fatal(names)
	}
}

func TestUserOverlayNewestMtime(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	ts, _, _ := lowerFixture(t) // a.fp has mtime 1700000000
	upper := t.TempDir()
	os.MkdirAll(filepath.Join(upper, "debug", "deps"), 0o755)
	old := filepath.Join(upper, "debug", "deps", "a.fp")
	os.WriteFile(old, []byte("local-old"), 0o644)
	mt := time.Unix(1600000000, 0)
	os.Chtimes(old, mt, mt)
	mnt := filepath.Join(t.TempDir(), "m")
	u, err := MountUserOverlay(mnt, upper, ts, UserOverlayOptions{Merge: NewestMtime, NoPassthrough: true})
	if err != nil {
		t.Skipf("cannot mount FUSE: %v", err)
	}
	defer u.Unmount()
	if b, _ := os.ReadFile(filepath.Join(mnt, "debug", "deps", "a.fp")); string(b) != "fp-a" {
		t.Fatalf("newer lower must win, got %q", b)
	}
}

func TestKernelOverlayOverLowerFS(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("kernel overlay needs root")
	}
	ts, ln, _ := lowerFixture(t)
	l := mountLowerOrSkip(t, ts, LowerOptions{AllowOther: true})
	base := t.TempDir()
	if err := KernelOverlaySupported(base); err != nil {
		t.Skipf("no kernel overlay: %v", err)
	}
	target := filepath.Join(base, "merged")
	if err := MountKernelOverlay(target, filepath.Join(base, "upper"), filepath.Join(base, "work"), l.Mountpoint()); err != nil {
		t.Fatal(err)
	}
	defer Unmount(target)
	p := func(s string) string { return filepath.Join(target, s) }
	// Metadata-only changes and hardlinks must not fault content in.
	os.Chmod(p("debug/deps/b.rlib"), 0o600)
	if err := os.Link(p("debug/deps/b.rlib"), p("debug/uplift")); err != nil {
		t.Fatal(err)
	}
	if s := ln.Stats(); s.Faults != 0 {
		t.Fatalf("metacopy faulted content: %+v", s)
	}
	if b, err := os.ReadFile(p("debug/uplift")); err != nil || len(b) != 100000 {
		t.Fatal(err)
	}
	if err := os.Rename(p("debug/deps"), p("debug/deps2")); err != nil {
		t.Fatalf("redirect_dir rename: %v", err)
	}
	tr, _, err := Scan(target, ScanOptions{Prev: ts.Tree()})
	if err != nil {
		t.Fatal(err)
	}
	layer := Diff(ts.Tree(), tr)
	if len(layer) == 0 {
		t.Fatal("no diff")
	}
}

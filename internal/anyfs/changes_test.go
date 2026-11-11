package anyfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// changeFixture writes the base content of a slot below dir.
func changeFixture(t *testing.T, dir string) {
	t.Helper()
	w := func(p, s string) {
		p = filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 60; i++ {
		w(fmt.Sprintf("keep/d%d/f%d", i%6, i), fmt.Sprintf("keep %d", i))
	}
	w("a/x", "xxxx")
	w("a/y", "y")
	w("a/z", "z")
	w("a/sub/q", "q")
	w("ren/r1", "r1")
	w("ren/r2", "r2")
	w("big", strings.Repeat("b", 2<<20))
}

// changeMutate changes the merged view m like a build session.
func changeMutate(t *testing.T, m string) {
	t.Helper()
	p := func(s string) string { return filepath.Join(m, s) }
	fi, _ := os.Stat(p("a/x"))
	if err := os.WriteFile(p("a/x"), []byte("XXXX"), 0o644); err != nil { // same size, mtime kept
		t.Fatal(err)
	}
	os.Chtimes(p("a/x"), fi.ModTime(), fi.ModTime())
	must(t, os.Remove(p("a/y")))
	must(t, os.RemoveAll(p("a/sub")))
	must(t, os.Remove(p("a/z")))
	must(t, os.MkdirAll(p("a/z"), 0o755))
	must(t, os.WriteFile(p("a/z/c"), []byte("c"), 0o644))
	must(t, os.MkdirAll(p("new/dir"), 0o755))
	must(t, os.WriteFile(p("new/dir/n1"), []byte("n1"), 0o644))
	if err := os.Rename(p("ren"), p("ren2")); err != nil { // like mv: copy and delete across devices
		must(t, os.MkdirAll(p("ren2"), 0o755))
		for _, n := range []string{"r1", "r2"} {
			b, err := os.ReadFile(p("ren/" + n))
			must(t, err)
			must(t, os.WriteFile(p("ren2/"+n), b, 0o644))
		}
		must(t, os.RemoveAll(p("ren")))
	}
	must(t, os.Remove(p("up/old1")))
	must(t, os.Chmod(p("keep/d3/f3"), 0o600))
	must(t, os.Symlink("../big", p("keep/link")))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sameTree compares trees ignoring pages.
func sameTree(t *testing.T, got, want *Tree) {
	t.Helper()
	if got.Len() != want.Len() {
		t.Errorf("got %d nodes, want %d: %v vs %v", got.Len(), want.Len(), paths(got), paths(want))
	}
	for _, w := range want.Nodes() {
		g, ok := got.Get(w.Path)
		if !ok {
			t.Errorf("missing %s", w.Path)
			continue
		}
		if !g.Same(&w) {
			t.Errorf("%s: got %+v want %+v", w.Path, *g, w)
		}
	}
}

// testChanges scans a merged view before and after changeMutate and
// compares ScanChanges with a full scan.
func testChanges(t *testing.T, merged string, opt ChangeOptions) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(merged, "up"), 0o755))
	must(t, os.WriteFile(filepath.Join(merged, "up/old1"), []byte("o"), 0o644))
	must(t, os.WriteFile(filepath.Join(merged, "up/old2"), []byte("o"), 0o644))
	sink := func(Key, []byte) error { return nil }
	prev, _, err := Scan(merged, ScanOptions{Sink: sink})
	must(t, err)
	time.Sleep(20 * time.Millisecond) // coarse ctimes
	since := time.Now().UnixNano()
	time.Sleep(20 * time.Millisecond)
	changeMutate(t, merged)
	opt.Prev, opt.Since, opt.Sink = prev, since, sink
	got, st, err := ScanChanges(merged, opt)
	must(t, err)
	want, _, err := Scan(merged, ScanOptions{})
	must(t, err)
	sameTree(t, got, want)
	// changed: a/x, a/z/c, new/dir/n1, ren2/r1, ren2/r2 (renamed), keep/d3/f3 (chmod)
	if st.Hashed > 6 {
		t.Errorf("hashed %d files, want at most 6 (%+v)", st.Hashed, st)
	}
	if opt.Upper != "" && st.Examined > 40 {
		t.Errorf("examined %d entries of %d", st.Examined, want.Len())
	}
	t.Logf("%d nodes: hashed %d, examined %d, reused %d", want.Len(), st.Hashed, st.Examined, st.Reused)
}

func TestScanChangesPlain(t *testing.T) {
	d := t.TempDir()
	changeFixture(t, d)
	testChanges(t, d, ChangeOptions{})
}

func TestScanChangesUserOverlay(t *testing.T) {
	lower := t.TempDir()
	changeFixture(t, lower)
	upper := t.TempDir()
	mnt := filepath.Join(t.TempDir(), "m")
	u, err := MountUserOverlay(mnt, upper, DirSource{Dir: lower}, UserOverlayOptions{NoPassthrough: true})
	if err != nil {
		t.Skipf("cannot mount FUSE: %v", err)
	}
	defer u.Unmount()
	testChanges(t, mnt, ChangeOptions{Upper: upper, UpperFormat: UpperUser})
}

func TestScanChangesKernelOverlay(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("kernel overlay needs root")
	}
	base := t.TempDir()
	if err := KernelOverlaySupported(base); err != nil {
		t.Skipf("no kernel overlay: %v", err)
	}
	lower := filepath.Join(base, "lower")
	changeFixture(t, lower)
	merged, upper := filepath.Join(base, "merged"), filepath.Join(base, "upper")
	must(t, MountKernelOverlay(merged, upper, filepath.Join(base, "work"), lower))
	defer Unmount(merged)
	testChanges(t, merged, ChangeOptions{Upper: upper, UpperFormat: UpperKernel})
}

package anyfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Kernel overlay metacopy entries of the upper (a chmod or touch of a
// lower file, a rename of one with an absolute path redirect, a hard
// link to a slot meta layer file redirecting by content digest) take
// digest and pages of the lower file without reading the merged view;
// only files whose data changed are read.
func TestChangesReuseMetacopy(t *testing.T) {
	lowerDir, merged, upper := t.TempDir(), t.TempDir(), t.TempDir()
	content := map[string]string{
		"chmod.bin": strings.Repeat("c", 100<<10),
		"orig.bin":  strings.Repeat("o", 90<<10),
		"orig2.bin": strings.Repeat("p", 80<<10),
		"changed":   "old content",
		"keep":      "kept",
	}
	for p, c := range content {
		must(t, os.WriteFile(filepath.Join(lowerDir, p), []byte(c), 0o644))
	}
	sink := func(Key, []byte) error { return nil }
	lower, _, err := Scan(lowerDir, ScanOptions{Sink: sink})
	must(t, err)
	since := time.Now().Add(-time.Second).UnixNano()

	// The merged view after the session: its metadata-only files are
	// unreadable, so reading them fails the test.
	mw := func(p, c string, mode os.FileMode) {
		q := filepath.Join(merged, p)
		os.MkdirAll(filepath.Dir(q), 0o755)
		must(t, os.WriteFile(q, []byte(c), 0o644))
		must(t, os.Chmod(q, mode))
	}
	mw("chmod.bin", content["chmod.bin"], 0o000)
	mw("moved/x.bin", content["orig.bin"], 0o000)
	mw("moved/y.bin", content["orig2.bin"], 0o000)
	mw("changed", "new content", 0o644)
	mw("keep", content["keep"], 0o644)

	// The upper directory: metacopy entries (sparse, xattrs only) and
	// the data of the changed file.
	uw := func(p string, size int64, redirect string) {
		q := filepath.Join(upper, p)
		os.MkdirAll(filepath.Dir(q), 0o755)
		must(t, os.WriteFile(q, nil, 0o600))
		must(t, os.Truncate(q, size))
		if err := unix.Lsetxattr(q, "user.overlay.metacopy", nil, 0); err != nil {
			t.Skipf("user xattrs unsupported: %v", err)
		}
		if redirect != "" {
			must(t, unix.Lsetxattr(q, "user.overlay.redirect", []byte(redirect), 0))
		}
	}
	d, _ := lower.Get("orig.bin")
	uw("chmod.bin", 100<<10, "")
	uw("moved/x.bin", 90<<10, "/"+d.Digest[:2]+"/"+d.Digest)
	uw("moved/y.bin", 80<<10, "/orig2.bin")
	must(t, os.WriteFile(filepath.Join(upper, "changed"), []byte("new content"), 0o644))
	for _, p := range []string{"orig.bin", "orig2.bin"} {
		must(t, os.WriteFile(filepath.Join(upper, p), nil, 0o644)) // stands in for the whiteout
	}
	mt := time.Now().Add(time.Hour) // touched: a new mtime keeps the content
	os.Chtimes(filepath.Join(merged, "chmod.bin"), mt, mt)

	opt := ChangeOptions{ScanOptions: ScanOptions{Prev: lower, Since: since, Sink: sink}, Upper: upper, Lower: lower}
	cs, err := PlanChanges(merged, opt)
	must(t, err)
	if strings.Join(cs.Read, ",") != "changed" {
		t.Fatalf("files to read %v, want only the changed one", cs.Read)
	}
	tree, st, err := cs.Tree(merged, opt.ScanOptions)
	must(t, err)
	if st.Hashed != 1 || st.Read != int64(len("new content")) {
		t.Fatalf("hashed %d files, %d bytes", st.Hashed, st.Read)
	}
	for p, from := range map[string]string{"chmod.bin": "chmod.bin", "moved/x.bin": "orig.bin", "moved/y.bin": "orig2.bin", "keep": "keep"} {
		n, ok := tree.Get(p)
		o, _ := lower.Get(from)
		if !ok || n.Digest != o.Digest || len(n.Pages) != len(o.Pages) || n.Pages[0] != o.Pages[0] {
			t.Fatalf("%s: digest/pages not reused from %s", p, from)
		}
	}
	if n, _ := tree.Get("chmod.bin"); n.Mode != 0 || n.MtimeNs != mt.UnixNano() {
		t.Fatalf("metadata of chmod.bin not taken from the merged view: %+v", n)
	}
	if _, ok := tree.Get("orig.bin"); ok {
		t.Fatal("renamed file still at its old path")
	}
	// A size mismatch (data copied up meanwhile) is read.
	if _, ok := metaSource(&scanned{Node: Node{Path: "moved/y.bin", Kind: File, Size: 1}, meta: true, from: "orig2.bin"}, lower, true, lower, nil); ok {
		t.Fatal("metacopy reused despite a size mismatch")
	}
}

package outputfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

func mountTest(t *testing.T) (*fakeSource, *Tree, *FS, string, string) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil && os.Getuid() != 0 {
		t.Skip("no fusermount3")
	}
	repo := t.TempDir()
	local := filepath.Join(repo, "target", "debug")
	os.MkdirAll(local, 0o755)
	os.WriteFile(filepath.Join(local, "local-only"), []byte("local"), 0o644)
	os.WriteFile(filepath.Join(local, "app"), []byte("old local build"), 0o755)
	src := newFake()
	tr := newTestTree(t)
	upper := filepath.Join(t.TempDir(), "upper")
	src.put("target/debug/app", "remote build", 7)
	src.put("target/debug/deps/libm.so", "dylib", 7)
	tr.Update(src)
	f, err := Mount(tr, repo, "target", upper, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Skipf("mount: %v", err)
	}
	t.Cleanup(func() { f.Unmount() })
	return src, tr, f, filepath.Join(repo, "target"), upper
}

func names(t *testing.T, dir string) string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}

func TestFSOverlayReadsRemoteLazily(t *testing.T) {
	src, tr, f, mp, _ := mountTest(t)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(mp, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	if !IsMounted(mp) {
		t.Fatal("not listed in mountinfo")
	}
	if got := read("debug/app"); got != "remote build" {
		t.Fatalf("remote file: %q", got)
	}
	if got := read("debug/deps/libm.so"); got != "dylib" {
		t.Fatalf("lazy file: %q", got)
	}
	if got := names(t, filepath.Join(mp, "debug")); got != "[app deps]" {
		t.Fatalf("readdir = %v (the covered local directory is hidden)", got)
	}
	fi, err := os.Stat(filepath.Join(mp, "debug", "app"))
	if err != nil || fi.Mode().Perm() != 0o644 || fi.Size() != 12 || fi.ModTime().UnixNano() != 7 {
		t.Fatalf("stat: %v %v", fi, err)
	}

	// A new run: the rebuilt file and a new one appear immediately.
	src.put("target/debug/app", "rebuilt!", 8)
	src.put("target/debug/new", "new", 8)
	tr.Update(nil)
	if got := read("debug/app"); got != "rebuilt!" {
		t.Fatalf("after invalidation: %q", got)
	}
	if got := read("debug/new"); got != "new" {
		t.Fatalf("new file: %q", got)
	}

	// Machine gone: cached content stays readable, the rest fails.
	src.mu.Lock()
	src.down = true
	src.mu.Unlock()
	if got := read("debug/app"); got != "rebuilt!" {
		t.Fatalf("cached after machine stop: %q", got)
	}
	if err := f.Unmount(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(mp, "debug", "app")); err != nil || string(b) != "old local build" {
		t.Fatalf("local directory changed: %q %v", b, err)
	}
}

func TestFSOverlayWrites(t *testing.T) {
	src, tr, _, mp, upper := mountTest(t)
	p := func(rel string) string { return filepath.Join(mp, filepath.FromSlash(rel)) }
	read := func(rel string) string {
		b, err := os.ReadFile(p(rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// New files and directories go to the upper layer.
	must(os.WriteFile(p("debug/.cargo-lock"), nil, 0o644))
	must(os.MkdirAll(p("debug/.fingerprint/x"), 0o755))
	must(os.WriteFile(p("debug/.fingerprint/x/lib-x"), []byte("fp"), 0o644))
	if _, err := os.Stat(filepath.Join(upper, "debug", ".fingerprint", "x", "lib-x")); err != nil {
		t.Fatalf("not in upper: %v", err)
	}
	if got := names(t, p("debug")); got != "[.cargo-lock .fingerprint app deps]" {
		t.Fatalf("merged readdir = %v", got)
	}
	if _, err := os.Create(p("debug/.wh.x")); err == nil {
		t.Fatal("reserved name created")
	}

	// Writing a remote file copies it up; the remote stays untouched.
	fh, err := os.OpenFile(p("debug/app"), os.O_WRONLY|os.O_APPEND, 0)
	must(err)
	fh.WriteString("+local")
	fh.Close()
	if got := read("debug/app"); got != "remote build+local" {
		t.Fatalf("copied up: %q", got)
	}
	if src.files["target/debug/app"] != "remote build" {
		t.Fatal("remote changed")
	}
	// A pure copy up keeps the remote mtime (cargo freshness).
	must(os.Chmod(p("debug/deps/libm.so"), 0o755))
	fi, err := os.Stat(p("debug/deps/libm.so"))
	if err != nil || fi.ModTime().UnixNano() != 7 || fi.Mode().Perm() != 0o755 || read("debug/deps/libm.so") != "dylib" {
		t.Fatalf("chmod copy up: %v %v", fi, err)
	}

	// Deleting a remote file leaves a whiteout.
	src.put("target/debug/gone", "g", 7)
	tr.Update(nil)
	must(os.Remove(p("debug/gone")))
	if _, err := os.Stat(p("debug/gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("whiteout: %v", err)
	}
	if got := names(t, p("debug")); got != "[.cargo-lock .fingerprint app deps]" {
		t.Fatalf("after unlink = %v", got)
	}
	// Recreated with an old mtime: still shadows the remote file.
	must(os.WriteFile(p("debug/gone"), []byte("again"), 0o644))
	must(os.Chtimes(p("debug/gone"), time.Unix(0, 1), time.Unix(0, 1)))
	if got := read("debug/gone"); got != "again" {
		t.Fatalf("recreated: %q", got)
	}

	// Rename of a remote file: copy up, whiteout of the source.
	must(os.Rename(p("debug/deps/libm.so"), p("debug/deps/libm2.so")))
	if got := names(t, p("debug/deps")); got != "[libm2.so]" {
		t.Fatalf("after rename = %v", got)
	}
	// Directories with remote content cannot be renamed (EXDEV).
	if err := os.Rename(p("debug/deps"), p("debug/deps2")); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("rename merged dir: %v", err)
	}
	// Removing a merged directory needs it empty; mkdir over it is opaque.
	if err := os.Remove(p("debug/deps")); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("rmdir non-empty: %v", err)
	}
	must(os.Remove(p("debug/deps/libm2.so")))
	must(os.Remove(p("debug/deps")))
	must(os.Mkdir(p("debug/deps"), 0o755))
	if got := names(t, p("debug/deps")); got != "[]" {
		t.Fatalf("opaque dir = %v", got)
	}

	// cargo's uplift: unlink + hard link of an unchanged remote output does
	// not download it when the remote build has the same link.
	later := time.Now().Add(time.Minute).UnixNano()
	src.put("target/debug/deps/tool-1", "tool", later)
	src.put("target/debug/tool", "tool", later)
	tr.Update(nil)
	_, reads := src.counts()
	must(os.Remove(p("debug/tool")))
	must(os.Link(p("debug/deps/tool-1"), p("debug/tool")))
	if _, r := src.counts(); r != reads {
		t.Fatalf("uplift downloaded %d files", r-reads)
	}
	if got := read("debug/tool"); got != "tool" {
		t.Fatalf("uplifted: %q", got)
	}

	// A newer remote build wins over older local state, including
	// whiteouts and copies; newer local files win over older remote ones.
	future := later + int64(time.Hour)
	src.put("target/debug/app", "remote v2", future)
	src.put("target/debug/gone", "remote again", future)
	src.put("target/debug/.cargo-lock", "remote lock", 1)
	tr.Update(nil)
	if got := read("debug/app"); got != "remote v2" {
		t.Fatalf("newer remote build: %q", got)
	}
	if got := read("debug/gone"); got != "remote again" {
		t.Fatalf("newer remote over whiteout: %q", got)
	}
	if got := read("debug/.cargo-lock"); got != "" {
		t.Fatalf("newer local file: %q", got)
	}
	// Hard link of a remote file without remote counterpart: copy up.
	must(os.Link(p("debug/app"), p("debug/app-link")))
	if got := read("debug/app-link"); got != "remote v2" {
		t.Fatalf("link: %q", got)
	}
	// Truncating open of a remote file needs no download.
	_, reads = src.counts()
	src.put("target/debug/big", "xxxxxxxx", 9)
	tr.Update(nil)
	must(os.WriteFile(p("debug/big"), []byte("y"), 0o644))
	if _, r := src.counts(); r != reads || read("debug/big") != "y" {
		t.Fatal("truncating open downloaded the file")
	}
}

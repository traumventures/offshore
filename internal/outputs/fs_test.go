package outputs

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
)

func TestFSOverlayReadOnly(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil && os.Getuid() != 0 {
		t.Skip("no fusermount3")
	}
	repo := t.TempDir()
	lower := filepath.Join(repo, "target", "debug")
	os.MkdirAll(lower, 0o755)
	os.WriteFile(filepath.Join(lower, "local-only"), []byte("local"), 0o644)
	os.WriteFile(filepath.Join(lower, "app"), []byte("old local build"), 0o755)

	src := newFake()
	src.put("target/debug/app", "remote build", 7)
	src.put("target/debug/deps/libm.so", "dylib", 7)
	tr := newTestTree(t)
	tr.Update(src)
	f, err := Mount(tr, repo, "target", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Skipf("mount: %v", err)
	}
	defer f.Unmount()
	mp := filepath.Join(repo, "target")
	if !IsMounted(mp) {
		t.Fatal("not listed in mountinfo")
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(mp, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	if got := read("debug/app"); got != "remote build" {
		t.Fatalf("remote wins: %q", got)
	}
	if got := read("debug/local-only"); got != "local" {
		t.Fatalf("local entries stay visible: %q", got)
	}
	if got := read("debug/deps/libm.so"); got != "dylib" {
		t.Fatalf("lazy file: %q", got)
	}
	des, err := os.ReadDir(filepath.Join(mp, "debug"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	sort.Strings(names)
	if fmt.Sprint(names) != "[app deps local-only]" {
		t.Fatalf("readdir = %v", names)
	}
	if fi, err := os.Stat(filepath.Join(mp, "debug", "app")); err != nil || fi.Mode().Perm() != 0o644 || fi.Size() != 12 {
		t.Fatalf("stat: %v %v", fi, err)
	}
	if err := os.WriteFile(filepath.Join(mp, "debug", "new"), []byte("x"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write must fail with EROFS, got %v", err)
	}
	if err := os.Mkdir(filepath.Join(mp, "x"), 0o755); err == nil {
		t.Fatal("mkdir succeeded on a read-only mount")
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
	src.put("target/debug/deps/other", "o", 9)
	tr.Update(nil) // listings dropped, machine still down
	src.mu.Lock()
	src.down = true
	src.mu.Unlock()
	if got := read("debug/local-only"); got != "local" {
		t.Fatalf("local fallback: %q", got)
	}
	if _, err := os.ReadFile(filepath.Join(mp, "debug", "deps", "other")); err == nil {
		t.Fatal("uncached remote file readable while the machine is gone")
	}

	if err := f.Unmount(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(lower, "app")); err != nil || string(b) != "old local build" {
		t.Fatalf("local directory changed: %q %v", b, err)
	}
}

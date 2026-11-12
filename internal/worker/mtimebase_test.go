package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
)

// A cargo build directory moves between worktrees (a slot seeded from a
// sibling's head): with the slot's source view, the new worktree's
// unchanged sources present the mtimes the outputs were built against,
// so cargo keeps the workspace crates fresh although the checkout is
// newer; a changed source rebuilds, and an older checkout with other
// content (stale without the view) rebuilds too.
func TestSeededCargoBuildStaysFresh(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("no cargo")
	}
	if testing.Short() {
		t.Skip("runs cargo")
	}
	base := t.TempDir()
	srcFiles, err := anyfs.OpenFiles(filepath.Join(base, "cache", "sources"), nil)
	if err != nil {
		t.Fatal(err)
	}
	srcFiles.MarkAllBacked()
	ws := map[string]string{
		"Cargo.toml":   "[workspace]\nmembers = [\"a\", \"b\"]\nresolver = \"2\"\n",
		"a/Cargo.toml": "[package]\nname = \"a\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"a/src/lib.rs": "pub fn f() -> u32 { 1 }\n",
		"b/Cargo.toml": "[package]\nname = \"b\"\nversion = \"0.1.0\"\nedition = \"2021\"\n[dependencies]\na = { path = \"../a\" }\n",
		"b/src/lib.rs": "pub fn g() -> u32 { a::f() }\n",
	}
	type file struct {
		content string
		mtime   time.Time
	}
	trees := map[string]map[string]file{}
	s := &Worker{cfg: Config{CacheDir: filepath.Join(base, "cache")}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		srcFiles: srcFiles, mounts: map[string]*anyfs.WritableFS{}}
	s.sourceFetch = func(_ context.Context, root string, n *anyfs.Node, w io.Writer) error {
		f, ok := trees[root][n.Path]
		if !ok || string(anyfs.KeyOf([]byte(f.content))) != n.Digest {
			return fmt.Errorf("%s is not a file of %s", n.Path, root)
		}
		_, err := io.WriteString(w, f.content)
		return err
	}
	t.Cleanup(func() {
		for _, p := range s.mounts {
			p.Unmount()
		}
	})
	version := uint64(0)
	sync := func(root string) *anyfs.WritableFS {
		t.Helper()
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		p, err := s.projection(root, nil)
		if err != nil {
			t.Skipf("cannot mount FUSE: %v", err)
		}
		dirs := map[string]bool{}
		var paths []string
		for rel := range trees[root] {
			paths = append(paths, rel)
			for d := filepath.Dir(rel); d != "."; d = filepath.Dir(d) {
				dirs[d] = true
			}
		}
		sort.Strings(paths)
		var layer []anyfs.Node
		for d := range dirs {
			layer = append(layer, anyfs.Node{Path: d, Kind: anyfs.Dir, Mode: 0o755})
		}
		for _, rel := range paths {
			f := trees[root][rel]
			layer = append(layer, anyfs.Node{Path: rel, Kind: anyfs.File, Mode: 0o644, Size: int64(len(f.content)),
				Digest: string(anyfs.KeyOf([]byte(f.content))), MtimeNs: f.mtime.UnixNano()})
		}
		version++
		p.Apply(layer, true, version)
		p.Settle()
		return p
	}
	worktree := func(root string, mtime time.Time, edits map[string]string) {
		trees[root] = map[string]file{}
		for rel, c := range ws {
			trees[root][rel] = file{c, mtime}
		}
		for rel, c := range edits {
			trees[root][rel] = file{c, mtime}
		}
	}
	// session runs cargo check in root over the slot data directory
	// (with or without its source view) and returns the compiled crates.
	session := func(root, data string, view bool) []string {
		t.Helper()
		p := sync(root)
		id := "session-" + filepath.Base(data)
		if b := loadSourcesBase(data); b != nil && view {
			p.AddMtimeBase(id, b)
		}
		cmd := exec.Command("cargo", "check", "--offline", "-v")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+filepath.Join(data, "target"),
			"RUSTC_WRAPPER=", "CARGO_BUILD_RUSTC_WRAPPER=", "CARGO_INCREMENTAL=0", "RUSTFLAGS=", "CARGO_TERM_COLOR=never")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cargo check in %s: %v\n%s", root, err, out)
		}
		if _, err := saveSourcesBase(data, p.PresentedBase()); err != nil {
			t.Fatal(err)
		}
		p.RemoveMtimeBase(id)
		var built []string
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "Checking" {
				built = append(built, f[1])
			}
		}
		sort.Strings(built)
		return built
	}
	seed := func(from, to string) {
		t.Helper()
		if out, err := exec.Command("cp", "-a", from, to).CombinedOutput(); err != nil {
			t.Fatalf("seed: %v %s", err, out)
		}
	}
	want := func(what string, got []string, w ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Fatalf("%s: compiled %v, want %v", what, got, w)
		}
	}
	now := time.Now()
	a, b, c := filepath.Join(base, "wt-a"), filepath.Join(base, "wt-b"), filepath.Join(base, "wt-c")
	slotA := filepath.Join(base, "slot-a")
	worktree(a, now.Add(-time.Hour), nil)
	want("first build", session(a, slotA, true), "a", "b")
	want("no-op", session(a, slotA, true))

	// A new worktree: same contents, checked out after the build.
	worktree(b, time.Now(), nil)
	seed(slotA, filepath.Join(base, "slot-b-plain"))
	want("seeded, client mtimes", session(b, filepath.Join(base, "slot-b-plain"), false), "a", "b")
	slotB := filepath.Join(base, "slot-b")
	seed(slotA, slotB)
	want("seeded, source view", session(b, slotB, true))
	want("seeded, no-op", session(b, slotB, true))

	// An edit in the new worktree rebuilds what depends on it, and the
	// view keeps up.
	trees[b]["a/src/lib.rs"] = file{"pub fn f() -> u32 { 2 }\n", time.Now()}
	want("edit", session(b, slotB, true), "a", "b")
	want("after edit", session(b, slotB, true))

	// An old checkout with other content: client mtimes call it fresh
	// (stale outputs); the view rebuilds it.
	worktree(c, now.Add(-24*time.Hour), map[string]string{"b/src/lib.rs": "pub fn g() -> u32 { a::f() + 1 }\n"})
	seed(slotA, filepath.Join(base, "slot-c-plain"))
	want("old checkout, client mtimes (stale)", session(c, filepath.Join(base, "slot-c-plain"), false))
	slotC := filepath.Join(base, "slot-c")
	seed(slotA, slotC)
	want("old checkout, source view", session(c, slotC, true), "b")
}

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/traumventures/offshore/internal/anyfs"
)

// worktree is a fake client root: path -> content.
type worktree map[string]string

func (wt worktree) layer() []anyfs.Node {
	dirs := map[string]bool{}
	var out []anyfs.Node
	paths := make([]string, 0, len(wt))
	for p := range wt {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	ds := make([]string, 0, len(dirs))
	for d := range dirs {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	for _, d := range ds {
		out = append(out, anyfs.Node{Path: d, Kind: anyfs.Dir, Mode: 0o755})
	}
	for _, p := range paths {
		c := wt[p]
		out = append(out, anyfs.Node{Path: p, Kind: anyfs.File, Mode: 0o644, Size: int64(len(c)),
			Digest: string(anyfs.KeyOf([]byte(c))), MtimeNs: 1e9})
	}
	return out
}

// Two worktrees of one repository on one machine, back to back: each
// projection shows exactly its own client's files (workspace members,
// rust-toolchain.toml), although contents are shared by digest in the
// machine's source cache and a session wrote into the first worktree.
// (Production report: a session saw a workspace member and a toolchain
// that were not its worktree's; the cause was ENOSPC on source faults,
// see TestSourceFaultSurvivesFullDisk, but the isolation is the contract.)
func TestWorktreeProjectionsAreIsolated(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	base := t.TempDir()
	cache := filepath.Join(base, "cache")
	srcFiles, err := anyfs.OpenFiles(filepath.Join(cache, "sources"), nil)
	if err != nil {
		t.Fatal(err)
	}
	srcFiles.MarkAllBacked()
	member := "[package]\nname = \"app-store\"\n"
	trees := map[string]worktree{
		filepath.Join(base, "wt-agent"): {
			"app/Cargo.toml":                           "[workspace]\nmembers = [\"crates/*/*\"]\n",
			"app/rust-toolchain.toml":                  "[toolchain]\nchannel = \"1.99.1\"\n",
			"app/crates/adapters/app-agent/Cargo.toml": "[package]\nname = \"app-agent\"\n",
			"app/crates/domain/app-store/Cargo.toml":   member,
		},
		filepath.Join(base, "wt-pr539"): {
			"app/Cargo.toml":                         "[workspace]\nmembers = [\"crates/*/*\"]\n",
			"app/rust-toolchain.toml":                "[toolchain]\nchannel = \"1.98.1\"\n",
			"app/crates/domain/app-store/Cargo.toml": member,
		},
	}
	s := &Worker{cfg: Config{CacheDir: cache}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		srcFiles: srcFiles, mounts: map[string]*anyfs.WritableFS{}}
	s.sourceFetch = func(_ context.Context, root string, n *anyfs.Node, w io.Writer) error {
		c, ok := trees[root][n.Path]
		if !ok || string(anyfs.KeyOf([]byte(c))) != n.Digest {
			return fmt.Errorf("%s is not a file of %s", n.Path, root)
		}
		_, err := io.WriteString(w, c)
		return err
	}
	t.Cleanup(func() {
		for _, p := range s.mounts {
			p.Unmount()
		}
	})
	attach := func(root string) {
		t.Helper()
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		p, err := s.projection(root, nil)
		if err != nil {
			t.Skipf("cannot mount FUSE: %v", err)
		}
		p.Apply(trees[root].layer(), true, 1)
		p.Settle()
	}
	check := func(root string) {
		t.Helper()
		var got []string
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(root, p)
				got = append(got, rel)
			}
			return nil
		})
		var want []string
		for p := range trees[root] {
			want = append(want, p)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s shows %v, want %v", root, got, want)
		}
		for p, c := range trees[root] {
			b, err := os.ReadFile(filepath.Join(root, p))
			if err != nil || string(b) != c {
				t.Fatalf("%s/%s: %q, %v; want %q", root, p, b, err, c)
			}
		}
	}
	a, b := filepath.Join(base, "wt-agent"), filepath.Join(base, "wt-pr539")
	attach(a)
	check(a)
	// A session of the first worktree writes into its sources.
	if err := os.WriteFile(filepath.Join(a, "app/Cargo.lock"), []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	trees[a]["app/Cargo.lock"] = "lock"
	attach(b)
	check(b)
	check(a)
}

// A source fault on a full cache disk reclaims unopened contents and
// retries; a read that still cannot be served fails with EIO, never with
// the cache volume's ENOSPC (which cargo reported as a manifest error).
func TestSourceFaultSurvivesFullDisk(t *testing.T) {
	files, err := anyfs.OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	files.MarkAllBacked()
	c := "[toolchain]\nchannel = \"1.98.1\"\n"
	n := &anyfs.Node{Path: "app/rust-toolchain.toml", Kind: anyfs.File, Size: int64(len(c)), Digest: string(anyfs.KeyOf([]byte(c)))}
	full := 1
	fetch := func(_ context.Context, _ *anyfs.Node, w io.Writer) error {
		if full > 0 {
			full--
			return fmt.Errorf("write: %w", syscall.ENOSPC)
		}
		_, err := io.WriteString(w, c)
		return err
	}
	p, err := files.EnsureWith(context.Background(), n, fetch)
	if err != nil {
		t.Fatalf("fault after a reclaim failed: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != c {
		t.Fatalf("content %q", b)
	}
	n2 := &anyfs.Node{Path: "app/Cargo.toml", Kind: anyfs.File, Size: 3, Digest: string(anyfs.KeyOf([]byte("abc")))}
	full = 2
	_, err = files.EnsureWith(context.Background(), n2, func(_ context.Context, _ *anyfs.Node, w io.Writer) error {
		return fmt.Errorf("write: %w", syscall.ENOSPC)
	})
	var errno syscall.Errno
	if !errors.As(err, &errno) || errno != syscall.EIO {
		t.Fatalf("err %v, want EIO", err)
	}
}

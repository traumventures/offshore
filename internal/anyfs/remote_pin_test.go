package anyfs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/proto"
)

// committedTree scans files (rel -> content, mtime 1) into a tree whose
// pages are in a store: the slot's last commit.
func committedTree(t *testing.T, files map[string]string) (*Tree, Tier) {
	t.Helper()
	dir := t.TempDir()
	for rel, c := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	st := dirStore(t)
	tr, _, err := Scan(dir, ScanOptions{Sink: func(k Key, b []byte) error { return st.Put(ctx, k, b, Dirty) }})
	if err != nil {
		t.Fatal(err)
	}
	return tr, st
}

// TestRemoteTreeUncommittedOutputSurvivesMachineLoss: an output built
// after the slot's last commit that the laptop fetched keeps its content
// (pinned against reclaim) and appears in listings served from the store
// once the machine is gone, also after a daemon restart (persisted
// index); one it never fetched fails with a clear reason.
func TestRemoteTreeUncommittedOutputSurvivesMachineLoss(t *testing.T) {
	committed, st := committedTree(t, map[string]string{"target/debug/old": "old"})
	src := newFake()
	src.put("target/debug/old", "old", committedMtime(t, committed, "target/debug/old"))
	src.put("target/debug/new/app", "fresh binary", 50)
	src.put("target/debug/unfetched", "never read", 50)

	filesDir, pagesDir, index := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "index.json")
	open := func() *RemoteTree {
		files, err := OpenFiles(filesDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		files.MarkAllBacked()
		tr := NewRemoteTree(files, openLocal(t, pagesDir, 0), slog.New(slog.NewTextHandler(io.Discard, nil)))
		tr.SetBacking(func(context.Context) (*Tree, Tier, error) { return committed, st, nil })
		if err := tr.SetIndexFile(index); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	tr := open()
	tr.Update(src)
	if got, err := readContent(t, tr, "target/debug/new/app"); err != nil || got != "fresh binary" {
		t.Fatalf("from the peer: %q %v", got, err)
	}
	if _, err := tr.Stat(ctx, "target/debug/unfetched"); err != nil { // listed, not read
		t.Fatal(err)
	}
	if n, _ := tr.Pinned(); n != 1 {
		t.Fatalf("%d pinned outputs, want 1", n)
	}

	// The machine disappears before committing.
	src.mu.Lock()
	src.down = true
	src.mu.Unlock()
	tr.files.Reclaim(0)
	tr.local.(*Local).Reclaim(ctx, 0)
	if got, err := readContent(t, tr, "target/debug/new/app"); err != nil || got != "fresh binary" {
		t.Fatalf("pinned output after machine loss and reclaim: %q %v", got, err)
	}
	e := mustStat(t, tr, "target/debug/unfetched")
	_, err := tr.Content(ctx, "target/debug/unfetched", e)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), NotCommitted) || !strings.Contains(err.Error(), "target/debug/unfetched") {
		t.Fatalf("unfetched uncommitted output: %v", err)
	}
	if u := tr.Unavailable(); len(u) != 1 || u[0].Path != "target/debug/unfetched" || u[0].Reason != NotCommitted {
		t.Fatalf("reported unavailable: %+v", u)
	}
	tr.Close()

	// Daemon restart: the listing comes from the store merged with the
	// pinned output, whose content is still resident.
	tr = open()
	tr.Update(goneFake())
	m, err := tr.List(ctx, "target/debug")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["old"]; !ok {
		t.Fatalf("committed file missing: %v", m)
	}
	if e, ok := m["new"]; !ok || e.Kind != Dir {
		t.Fatalf("directory of the pinned output missing: %v", m)
	}
	if _, ok := m["unfetched"]; ok {
		t.Fatal("never fetched, never committed output listed after restart")
	}
	tr.files.Reclaim(0)
	if got, err := readContent(t, tr, "target/debug/new/app"); err != nil || got != "fresh binary" {
		t.Fatalf("pinned output after restart: %q %v", got, err)
	}
	if got, err := readContent(t, tr, "target/debug/old"); err != nil || got != "old" {
		t.Fatalf("committed output: %q %v", got, err)
	}
}

// TestRemoteTreePinsReleasedWhenCommittedOrSuperseded: a pin ends once
// the committed slot holds the output, or when a newer listing replaced
// it.
func TestRemoteTreePinsReleasedWhenCommittedOrSuperseded(t *testing.T) {
	committed, st := committedTree(t, map[string]string{"target/a": "aaa"})
	src := newFake()
	src.put("target/a", "aaa", committedMtime(t, committed, "target/a"))
	src.put("target/b", "bbb", 9)
	tr := newTestTree(t)
	tr.SetBacking(func(context.Context) (*Tree, Tier, error) { return committed, st, nil })
	tr.Update(src)
	readContent(t, tr, "target/a")
	readContent(t, tr, "target/b")
	if n, _ := tr.Pinned(); n != 2 {
		t.Fatalf("%d pinned, want 2", n)
	}
	if n, err := tr.CheckCommitted(ctx); err != nil || n != 1 {
		t.Fatalf("released %d (%v), want 1 (target/a is committed)", n, err)
	}
	src.put("target/b", "rebuilt", 10)
	tr.Update(src)
	if _, err := tr.List(ctx, "target"); err != nil {
		t.Fatal(err)
	}
	if n, _ := tr.Pinned(); n != 0 {
		t.Fatalf("%d pinned after the output was rebuilt, want 0", n)
	}
	if n, _ := tr.files.Pinned(); n != 0 {
		t.Fatalf("%d pinned contents left", n)
	}
}

func TestFilesReclaimSkipsPinned(t *testing.T) {
	f, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*Node
	for _, c := range []string{"one", "two"} {
		src := filepath.Join(t.TempDir(), "f")
		os.WriteFile(src, []byte(c), 0o644)
		n := &Node{Path: c, Kind: File, Size: 3, Digest: string(KeyOf([]byte(c)))}
		if err := f.Adopt(n, src, true, false); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, n)
	}
	f.Pin(nodes[0])
	f.Reclaim(0)
	if _, ok := f.Local(nodes[0]); !ok {
		t.Fatal("pinned content reclaimed")
	}
	if _, ok := f.Local(nodes[1]); ok {
		t.Fatal("unpinned content kept")
	}
	f.Unpin(nodes[0])
	f.Reclaim(0)
	if _, ok := f.Local(nodes[0]); ok {
		t.Fatal("content kept after unpin")
	}
}

func committedMtime(t *testing.T, tr *Tree, rel string) int64 {
	t.Helper()
	n, ok := tr.Get(rel)
	if !ok {
		t.Fatalf("%s not committed", rel)
	}
	return n.MtimeNs
}

func mustStat(t *testing.T, tr *RemoteTree, rel string) proto.OutEntry {
	t.Helper()
	e, err := tr.Stat(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func goneFake() *fakeRemote {
	f := newFake()
	f.down = true
	return f
}

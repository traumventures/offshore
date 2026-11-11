package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/pool"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/store"
)

// storePool hands out the head and grant of a committed slot.
type storePool struct {
	pool.Pool
	out   orchestrator.OutputsStore
	calls int
}

func (p *storePool) OutputsStore(_ context.Context, repo string) (orchestrator.OutputsStore, error) {
	p.calls++
	if repo != "r1" {
		return orchestrator.OutputsStore{}, orchestrator.ErrNoHead
	}
	return p.out, nil
}

// goneRemote is the machine of the latest build after it was terminated.
type goneRemote struct{}

func (goneRemote) Name() string { return "gone" }
func (goneRemote) Close()       {}
func (goneRemote) List(context.Context, string) ([]proto.OutEntry, error) {
	return nil, errors.New("no route to host")
}
func (goneRemote) Read(context.Context, string, int64, int64, io.Writer) error {
	return errors.New("no route to host")
}

// TestOutputsFallBackToTheStore: with the machine gone the overlay lists
// and reads the repository's committed slot from the store, through the
// head and grant the control plane returns.
func TestOutputsFallBackToTheStore(t *testing.T) {
	t.Setenv("OFFSHORE_STATE_DIR", t.TempDir())
	ctx := context.Background()
	slot := t.TempDir()
	os.MkdirAll(filepath.Join(slot, "target", "debug"), 0o755)
	os.WriteFile(filepath.Join(slot, "target", "debug", "app"), []byte("remote binary"), 0o755)

	base := t.TempDir()
	blobs, err := blobcache.NewDirStore(filepath.Join(base, "cache", store.RepoScope("r1")), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pages := anyfs.NewStore(blobs, anyfs.StoreMedium())
	tree, _, err := anyfs.Scan(slot, anyfs.ScanOptions{Sink: func(k anyfs.Key, b []byte) error { return pages.Put(ctx, k, b, anyfs.Dirty) }})
	if err != nil {
		t.Fatal(err)
	}
	layer := anyfs.Commit(nil, anyfs.NewTree(nil), tree, time.Now(), nil)
	if err := anyfs.SaveLayer(ctx, blobs, layer); err != nil {
		t.Fatal(err)
	}
	p := &storePool{out: orchestrator.OutputsStore{Head: proto.LayerHead{Repo: "r1", Layer: layer.ID},
		Grant: store.Grant{Store: "storage", Dir: base, Prefix: "cache", Scopes: []string{store.RepoScope("r1")}}}}
	m, err := newOutputsMgr(testDaemon(p), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	r := m.repoLocked("/repo")
	r.setState(outputsState{RepoID: "r1", Machine: "m1"})
	m.mu.Unlock()
	r.tree.Update(goneRemote{})

	e, err := r.tree.Stat(ctx, "target/debug/app")
	if err != nil {
		t.Fatalf("stat from the store: %v", err)
	}
	path, err := r.tree.Content(ctx, "target/debug/app", e)
	if err != nil {
		t.Fatalf("content from the store: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "remote binary" {
		t.Fatalf("content %q", b)
	}
	if p.calls != 1 {
		t.Fatalf("%d control plane calls", p.calls)
	}

	// Eager outputs from the proxy's spool are served without either.
	h, err := m.spool.Ingest(strings.NewReader("eager"))
	if err != nil {
		t.Fatal(err)
	}
	m.hint("/repo", []manifest.Entry{{Path: "target/debug/eager", Kind: manifest.File, Size: 5, MtimeNs: 7, Hash: h}})
	if m.spool.Has(h) {
		t.Fatal("spooled output not moved into the cache")
	}
	path, err = r.tree.Content(ctx, "target/debug/eager", proto.OutEntry{Name: "eager", Kind: manifest.File, Size: 5, MtimeNs: 7})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "eager" {
		t.Fatalf("eager content %q", b)
	}
}

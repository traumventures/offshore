package daemon

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
)

// TestOutputsEagerDigestSkip: an eager output whose content the output
// cache already holds is reported by outputs-have (the proxy skips the
// download) and indexed from the cache at its new version, without the
// spool or the machine.
func TestOutputsEagerDigestSkip(t *testing.T) {
	t.Setenv("OFFSHORE_STATE_DIR", t.TempDir())
	m, err := newOutputsMgr(testDaemon(&storePool{}), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	h, err := m.spool.Ingest(strings.NewReader("eager"))
	if err != nil {
		t.Fatal(err)
	}
	e := manifest.Entry{Path: "target/debug/eager", Kind: manifest.File, Mode: 0o755, Size: 5, MtimeNs: 7, Hash: h}
	other := manifest.Entry{Path: "target/debug/other", Kind: manifest.File, Size: 3, MtimeNs: 7, Hash: manifest.HashBytes([]byte("new"))}
	if got := m.have([]manifest.Entry{e, other}); len(got) != 1 || got[0] != h {
		t.Fatalf("spooled content: have %v", got)
	}
	m.hint("/repo", []manifest.Entry{e})
	if m.spool.Has(h) {
		t.Fatal("spooled output not moved into the cache")
	}
	if got := m.have([]manifest.Entry{e, other}); len(got) != 1 || got[0] != h {
		t.Fatalf("cached content: have %v, want only %s", got, h)
	}

	// The next run rebuilt it with the same content: nothing downloaded,
	// the cache serves the new version.
	e.MtimeNs = 8
	m.hint("/repo", []manifest.Entry{e})
	m.mu.Lock()
	r := m.repoLocked("/repo")
	m.mu.Unlock()
	oe := proto.OutEntry{Name: "eager", Kind: manifest.File, Mode: 0o755, Size: 5, MtimeNs: 8}
	if !r.tree.Known(e.Path, oe, h) {
		t.Fatal("new version not indexed from the cache")
	}
	p, err := r.tree.Content(context.Background(), e.Path, oe)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "eager" {
		t.Fatalf("content %q", b)
	}
	if n, _ := r.tree.Pinned(); n != 1 {
		t.Fatalf("%d pinned outputs, want 1 (not committed yet)", n)
	}
}

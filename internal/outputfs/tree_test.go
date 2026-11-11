package outputfs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/cas"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
)

// fakeSource serves an in-memory build slot.
type fakeSource struct {
	mu    sync.Mutex
	files map[string]string // rel -> content
	mtime map[string]int64
	dirs  map[string]bool
	down  bool
	lists int
	reads int
}

func newFake() *fakeSource {
	return &fakeSource{files: map[string]string{}, mtime: map[string]int64{}, dirs: map[string]bool{"": true}}
}

func (f *fakeSource) put(rel, content string, mtime int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[rel], f.mtime[rel] = content, mtime
	for d := path.Dir(rel); ; d = path.Dir(d) {
		if d == "." {
			break
		}
		f.dirs[d] = true
		// Like a real directory: adding or replacing an entry (a rename
		// of a fresh output) updates its mtime.
		f.mtime[d+"/"] = max(f.mtime[d+"/"], mtime)
	}
}

func (f *fakeSource) Name() string { return "fake" }
func (f *fakeSource) Close()       {}

func (f *fakeSource) List(ctx context.Context, dir string) ([]proto.OutEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.down {
		return nil, errors.New("connection refused")
	}
	if !f.dirs[dir] {
		return nil, &proto.Error{Stage: "notfound", Msg: "no such directory"}
	}
	var out []proto.OutEntry
	seen := map[string]bool{}
	for rel, c := range f.files {
		if cleanDir(path.Dir(rel)) == dir {
			out = append(out, proto.OutEntry{Name: path.Base(rel), Kind: manifest.File, Mode: 0o644, Size: int64(len(c)), MtimeNs: f.mtime[rel]})
		}
	}
	for d := range f.dirs {
		if d != "" && cleanDir(path.Dir(d)) == dir && !seen[d] {
			seen[d] = true
			out = append(out, proto.OutEntry{Name: path.Base(d), Kind: manifest.Dir, Mode: 0o755, MtimeNs: f.mtime[d+"/"]})
		}
	}
	return out, nil
}

func (f *fakeSource) Read(ctx context.Context, rel string, size, mtime int64, w io.Writer) error {
	f.mu.Lock()
	f.reads++
	down := f.down
	c, ok := f.files[rel]
	m := f.mtime[rel]
	f.mu.Unlock()
	if down {
		return errors.New("connection refused")
	}
	if !ok {
		return &proto.Error{Stage: "notfound", Msg: "gone"}
	}
	if int64(len(c)) != size || m != mtime {
		return &proto.Error{Stage: "stale", Msg: "changed"}
	}
	_, err := io.WriteString(w, c)
	return err
}

func (f *fakeSource) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists, f.reads
}

func newTestTree(t *testing.T) *Tree {
	t.Helper()
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewTree(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func readContent(t *testing.T, tr *Tree, rel string) (string, error) {
	t.Helper()
	e, err := tr.Stat(context.Background(), rel)
	if err != nil {
		return "", err
	}
	p, err := tr.Content(context.Background(), rel, e)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

func TestTreeListsLazilyAndCaches(t *testing.T) {
	src := newFake()
	src.put("target/debug/app", "binary", 1)
	src.put("target/debug/deps/libm.so", "dylib", 2)
	tr := newTestTree(t)
	tr.Update(src)

	got, err := readContent(t, tr, "target/debug/deps/libm.so")
	if err != nil || got != "dylib" {
		t.Fatalf("content = %q, %v", got, err)
	}
	if _, err := readContent(t, tr, "target/debug/deps/libm.so"); err != nil {
		t.Fatal(err)
	}
	lists, reads := src.counts()
	if lists != 1 || reads != 1 {
		t.Fatalf("lists=%d reads=%d, want one listing of the directory and one download", lists, reads)
	}
	if _, err := tr.Stat(context.Background(), "target/debug/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := tr.List(context.Background(), "target/release"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
	if _, err := tr.List(context.Background(), "target/release"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing directory not remembered")
	}
	if lists, _ := src.counts(); lists != 3 {
		t.Fatalf("lists = %d, want 3 (deps, debug, release once)", lists)
	}
}

func TestTreeUpdateInvalidatesListingsKeepsUnchangedContent(t *testing.T) {
	src := newFake()
	src.put("target/debug/a", "aaa", 1)
	src.put("target/debug/b", "bbb", 1)
	tr := newTestTree(t)
	invalidations := 0
	tr.OnInvalidate(func() { invalidations++ })
	tr.Update(src)
	for _, f := range []string{"a", "b"} {
		if _, err := readContent(t, tr, "target/debug/"+f); err != nil {
			t.Fatal(err)
		}
	}
	// A new run rebuilds b and adds c.
	src.put("target/debug/b", "BBBB", 2)
	src.put("target/debug/c", "c", 2)
	if _, err := tr.Stat(context.Background(), "target/debug/c"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("listing must stay stable within a generation")
	}
	tr.Update(nil)
	if invalidations != 2 {
		t.Fatalf("invalidations = %d", invalidations)
	}
	if got, _ := readContent(t, tr, "target/debug/b"); got != "BBBB" {
		t.Fatalf("b = %q after rebuild", got)
	}
	if got, _ := readContent(t, tr, "target/debug/c"); got != "c" {
		t.Fatalf("c = %q", got)
	}
	_, reads := src.counts()
	if got, _ := readContent(t, tr, "target/debug/a"); got != "aaa" {
		t.Fatalf("a = %q", got)
	}
	if _, r := src.counts(); r != reads {
		t.Fatal("unchanged file downloaded again after invalidation")
	}
}

func TestTreeHintServesEagerOutputsWithoutMachine(t *testing.T) {
	src := newFake()
	src.put("target/debug/app", "eager-binary", 5)
	tr := newTestTree(t)
	tr.Update(src)
	h, err := tr.store.Ingest(strings.NewReader("eager-binary"))
	if err != nil {
		t.Fatal(err)
	}
	tr.Hint([]manifest.Entry{{Path: "target/debug/app", Kind: manifest.File, Size: 12, MtimeNs: 5, Hash: h}})
	if got, err := readContent(t, tr, "target/debug/app"); err != nil || got != "eager-binary" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, reads := src.counts(); reads != 0 {
		t.Fatalf("hinted file downloaded (%d reads)", reads)
	}
}

func TestTreeMachineGoneServesCache(t *testing.T) {
	src := newFake()
	src.put("target/debug/deps/cached", "c", 1)
	src.put("target/debug/deps/uncached", "u", 1)
	tr := newTestTree(t)
	now := time.Unix(1000, 0)
	tr.now = func() time.Time { return now }
	tr.Update(src)
	if _, err := readContent(t, tr, "target/debug/deps/cached"); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.down = true
	src.mu.Unlock()
	if got, err := readContent(t, tr, "target/debug/deps/cached"); err != nil || got != "c" {
		t.Fatalf("cached content after machine stop: %q, %v", got, err)
	}
	if _, err := readContent(t, tr, "target/debug/deps/uncached"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("uncached content: %v", err)
	}
	if _, err := tr.List(context.Background(), "target/release"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("uncached dir: %v", err)
	}
	lists, reads := src.counts()
	// Within the back-off window the machine is not asked again.
	readContent(t, tr, "target/debug/deps/uncached")
	tr.List(context.Background(), "target/other")
	if l, r := src.counts(); l != lists || r != reads {
		t.Fatalf("unreachable machine asked again: lists %d->%d reads %d->%d", lists, l, reads, r)
	}
	now = now.Add(downFor + time.Second)
	src.mu.Lock()
	src.down = false
	src.mu.Unlock()
	if got, err := readContent(t, tr, "target/debug/deps/uncached"); err != nil || got != "u" {
		t.Fatalf("after recovery: %q, %v", got, err)
	}
}

func TestTreeStaleReadDropsListing(t *testing.T) {
	src := newFake()
	src.put("target/x", "one", 1)
	tr := newTestTree(t)
	tr.Update(src)
	e, err := tr.Stat(context.Background(), "target/x")
	if err != nil {
		t.Fatal(err)
	}
	src.put("target/x", "two!", 2) // rebuilt by a run that is still going on
	if _, err := tr.Content(context.Background(), "target/x", e); !errors.Is(err, ErrStale) {
		t.Fatalf("want stale, got %v", err)
	}
	if got, err := readContent(t, tr, "target/x"); err != nil || got != "two!" {
		t.Fatalf("after relisting: %q, %v", got, err)
	}
}

func TestTreeWithoutSource(t *testing.T) {
	tr := newTestTree(t)
	if _, err := tr.List(context.Background(), "target"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestParseMountinfo(t *testing.T) {
	in := `36 35 98:0 /mnt1 /mnt1 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
100 29 0:77 / /home/u/my\040repo/target ro,nosuid,nodev,relatime shared:5 - fuse.offshore-outputs offshore ro,user_id=1000
101 29 0:78 / /home/u/proj ro shared:6 - fuse.offshore offshore rw
`
	got := parseMountinfo(bufio.NewScanner(strings.NewReader(in)))
	if len(got) != 1 || got[0] != "/home/u/my repo/target" {
		t.Fatalf("got %q", got)
	}
	if Covering(got, "/home/u/my repo/target/debug/x") == "" || Covering(got, "/home/u/my repo/targetx") != "" {
		t.Fatal("Covering")
	}
}

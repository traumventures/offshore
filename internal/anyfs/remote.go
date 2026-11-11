package anyfs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/zeebo/blake3"
)

// Remote reads the latest build slot of one repository on one machine
// (the peer tier of the laptop's output overlay) over the OutList and
// OutRead RPCs. Paths are relative to the repository root and use "/".
type Remote interface {
	// Name identifies the remote in logs (the machine).
	Name() string
	List(ctx context.Context, dir string) ([]proto.OutEntry, error)
	// Read writes the whole file to w. size and mtimeNs are those of the
	// listing; a changed file fails with a *proto.Error of stage "stale".
	Read(ctx context.Context, rel string, size, mtimeNs int64, w io.Writer) error
	Close()
}

// ErrUnavailable means the remote build cannot be reached and the
// requested data is not cached.
var ErrUnavailable = errors.New("remote build outputs unavailable and not cached")

// ErrStale means a file changed on the machine after it was listed.
var ErrStale = errors.New("remote output changed while reading")

// downFor is how long an unreachable remote is not asked again.
const downFor = 20 * time.Second

// Backing resolves the committed state of the remote build slot in the
// object store: the tree of its layer head and a tier holding its pages.
// It is the bottom of the laptop's stack local -> peer -> store, used
// when the machine of the latest build cannot be reached.
type Backing func(ctx context.Context) (*Tree, Tier, error)

// RemoteTree caches the remote view of one repository's build
// directories: the stack local -> peer (the machine of the latest build,
// over QUIC) -> store (the slot's committed layers). Directory listings
// are fetched lazily, one directory at a time, and belong to a generation
// that ends with every new remote run (Update). File contents become
// anyfs pages in the local tier and materialized files (Files), indexed
// by path, size and mtime, so unchanged outputs are never downloaded
// twice and reclaimed contents fault in again from the local pages, the
// peer or the store. When the peer is unreachable, listings and contents
// come from the Backing (SetBacking). Source(prefix) serves one build
// directory as the lower layer of a UserOverlay.
type RemoteTree struct {
	files *Files
	local Tier
	log   *slog.Logger

	mu        sync.Mutex
	gen       uint64
	src       Remote
	dirs      map[string]map[string]proto.OutEntry // current generation
	missing   map[string]bool                      // dirs absent remotely in this generation
	index     map[string]indexed
	downUntil time.Time
	downErr   error
	downGen   uint64 // generation of the last "unreachable" log line
	flights   map[string]*flight
	hooks     []func()
	now       func() time.Time

	backing  Backing
	bGen     uint64 // generation of the resolution below
	bTree    *Tree
	bTier    Tier // local -> store
	bErr     error
	bUntil   time.Time
	ingested atomic.Uint64
}

// indexed is the known content of a remote file: Node carries its digest
// and pages.
type indexed struct {
	Size    int64
	MtimeNs int64
	Node    Node
}

type flight struct {
	done chan struct{}
	val  string
	err  error
}

// NewRemoteTree creates an empty tree whose contents are kept as pages
// in local and materialized by files.
func NewRemoteTree(files *Files, local Tier, log *slog.Logger) *RemoteTree {
	return &RemoteTree{files: files, local: local, log: log, dirs: map[string]map[string]proto.OutEntry{}, missing: map[string]bool{},
		index: map[string]indexed{}, flights: map[string]*flight{}, now: time.Now}
}

// SetBacking sets the store fallback (nil: none).
func (t *RemoteTree) SetBacking(b Backing) {
	t.mu.Lock()
	t.backing, t.bTree, t.bTier, t.bErr, t.bUntil = b, nil, nil, nil, time.Time{}
	t.mu.Unlock()
}

// resolveBacking returns the committed tree and the stack local -> store
// of the current generation, resolving it once per generation (failures
// are remembered for downFor).
func (t *RemoteTree) resolveBacking(ctx context.Context) (*Tree, Tier, error) {
	t.mu.Lock()
	b, gen := t.backing, t.gen
	switch {
	case b == nil:
		t.mu.Unlock()
		return nil, nil, ErrUnavailable
	case t.bGen == gen && t.bTree != nil:
		tr, tier := t.bTree, t.bTier
		t.mu.Unlock()
		return tr, tier, nil
	case t.bGen == gen && t.bErr != nil && t.now().Before(t.bUntil):
		err := t.bErr
		t.mu.Unlock()
		return nil, nil, err
	}
	t.mu.Unlock()
	_, err := t.do(fmt.Sprintf("backing\x00%d", gen), func() (string, error) {
		rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		tr, tier, err := b(rctx)
		t.mu.Lock()
		defer t.mu.Unlock()
		if gen != t.gen {
			return "", nil
		}
		t.bGen = gen
		if err != nil {
			t.bTree, t.bTier, t.bErr, t.bUntil = nil, nil, err, t.now().Add(downFor)
			return "", err
		}
		t.bTree, t.bTier, t.bErr = tr, NewLink(t.local, tier, Policy{}, nil), nil
		return "", nil
	})
	if err != nil {
		t.log.Info("outputs: store fallback unavailable", "err", err)
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return t.resolveBacking(ctx)
}

// OnInvalidate registers a function called (outside the tree lock) after
// every Update, e.g. to drop kernel caches.
func (t *RemoteTree) OnInvalidate(f func()) {
	t.mu.Lock()
	t.hooks = append(t.hooks, f)
	t.mu.Unlock()
}

// Update starts a new generation after a remote run. src replaces the
// current remote unless it is nil.
func (t *RemoteTree) Update(src Remote) {
	t.mu.Lock()
	t.gen++
	if src != nil {
		if t.src != nil && t.src != src {
			t.src.Close()
		}
		t.src = src
	}
	t.dirs = map[string]map[string]proto.OutEntry{}
	t.missing = map[string]bool{}
	t.downUntil, t.downErr = time.Time{}, nil
	hooks := append([]func(){}, t.hooks...)
	t.mu.Unlock()
	for _, h := range hooks {
		h()
	}
}

// Close releases the source.
func (t *RemoteTree) Close() {
	t.mu.Lock()
	src := t.src
	t.src = nil
	t.mu.Unlock()
	if src != nil {
		src.Close()
	}
}

// Generation returns the current generation.
func (t *RemoteTree) Generation() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// SourceName returns the current source's name ("" without source).
func (t *RemoteTree) SourceName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.src == nil {
		return ""
	}
	return t.src.Name()
}

// Stats reports cached directories and indexed files.
func (t *RemoteTree) Stats() (dirs, files int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.dirs), len(t.index)
}

// Ingest stores the content of the remote file rel (size and mtime as
// listed) from r as pages in the local tier and as a materialized file,
// so it is served without the machine (e.g. eager outputs the proxy
// already transferred), and returns the path of the materialized file.
func (t *RemoteTree) Ingest(ctx context.Context, rel string, size, mtimeNs int64, r io.Reader) (string, error) {
	rel = cleanRel(rel)
	tmp := filepath.Join(t.files.dir, fmt.Sprintf(".ingest-%d-%d", time.Now().UnixNano(), t.ingested.Add(1)))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp) // moved into place on success
	h := blake3.New()
	buf := make([]byte, PageSize)
	n := Node{Path: rel, Kind: File, MtimeNs: mtimeNs}
	for {
		k, rerr := io.ReadFull(r, buf)
		if k > 0 {
			b := buf[:k]
			key := KeyOf(b)
			h.Write(b)
			if _, err := out.Write(b); err != nil {
				out.Close()
				return "", err
			}
			if err := t.local.Put(ctx, key, b, Clean); err != nil {
				out.Close()
				return "", err
			}
			n.Pages = append(n.Pages, PageRef{Key: key, Len: int64(k)})
			n.Size += int64(k)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			out.Close()
			return "", rerr
		}
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if n.Size != size {
		return "", fmt.Errorf("short read: %d of %d bytes", n.Size, size)
	}
	n.Digest = hex.EncodeToString(h.Sum(nil))
	if err := t.files.Adopt(&n, tmp, true, true); err != nil {
		return "", err
	}
	t.mu.Lock()
	t.index[rel] = indexed{Size: size, MtimeNs: mtimeNs, Node: n}
	t.mu.Unlock()
	p, ok := t.files.Local(&n)
	if !ok {
		return "", fmt.Errorf("ingest %s: %w", rel, ErrNotFound)
	}
	return p, nil
}

// local returns the content of an indexed file from the local tiers:
// the materialized file, or assembled from resident pages (or the store
// pages of a resolved backing).
func (t *RemoteTree) localContent(ctx context.Context, rel string, size, mtimeNs int64) (string, bool) {
	t.mu.Lock()
	ix, ok := t.index[rel]
	tier := t.local
	if t.bTier != nil && t.bGen == t.gen {
		tier = t.bTier
	}
	t.mu.Unlock()
	if !ok || ix.Size != size || ix.MtimeNs != mtimeNs {
		return "", false
	}
	if p, ok := t.files.Local(&ix.Node); ok {
		return p, true
	}
	p, err := t.files.Ensure(ctx, tier, &ix.Node)
	return p, err == nil
}

// classify turns a source error into ErrNotExist, ErrStale or a remote
// error; anything that is not an answer of the server marks the source as
// unreachable.
func (t *RemoteTree) classify(gen uint64, what string, err error) error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		switch pe.Stage {
		case "notfound":
			return fs.ErrNotExist
		case "stale":
			return ErrStale
		}
		return err
	}
	t.mu.Lock()
	if gen == t.gen {
		t.downUntil = t.now().Add(downFor)
		t.downErr = err
	}
	logIt := t.downGen != gen || gen == 0
	t.downGen = gen
	name := ""
	if t.src != nil {
		name = t.src.Name()
	}
	t.mu.Unlock()
	if logIt {
		t.log.Warn("outputs: machine of the latest build is unreachable, serving cached outputs only", "machine", name, "err", err)
	}
	t.log.Info("outputs: not available locally", "path", what)
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// available returns the source unless it is missing or recently down.
func (t *RemoteTree) availableLocked() (Remote, error) {
	if t.src == nil {
		return nil, ErrUnavailable
	}
	if t.now().Before(t.downUntil) {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, t.downErr)
	}
	return t.src, nil
}

// do runs fn once per key at a time.
func (t *RemoteTree) do(key string, fn func() (string, error)) (string, error) {
	t.mu.Lock()
	if f, ok := t.flights[key]; ok {
		t.mu.Unlock()
		<-f.done
		return f.val, f.err
	}
	f := &flight{done: make(chan struct{})}
	t.flights[key] = f
	t.mu.Unlock()
	f.val, f.err = fn()
	t.mu.Lock()
	delete(t.flights, key)
	t.mu.Unlock()
	close(f.done)
	return f.val, f.err
}

// List returns the remote entries of dir (relative to the repository).
func (t *RemoteTree) List(ctx context.Context, dir string) (map[string]proto.OutEntry, error) {
	dir = cleanRel(dir)
	for {
		t.mu.Lock()
		if m, ok := t.dirs[dir]; ok {
			t.mu.Unlock()
			return m, nil
		}
		if t.missing[dir] {
			t.mu.Unlock()
			return nil, fs.ErrNotExist
		}
		src, err := t.availableLocked()
		gen := t.gen
		t.mu.Unlock()
		if err != nil {
			return t.listBacking(ctx, dir, err)
		}
		_, err = t.do(fmt.Sprintf("list\x00%d\x00%s", gen, dir), func() (string, error) {
			lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			entries, err := src.List(lctx, dir)
			if err != nil {
				err = t.classify(gen, dir+"/", err)
				if errors.Is(err, fs.ErrNotExist) {
					t.mu.Lock()
					if gen == t.gen {
						t.missing[dir] = true
					}
					t.mu.Unlock()
				}
				return "", err
			}
			m := make(map[string]proto.OutEntry, len(entries))
			for _, e := range entries {
				m[e.Name] = e
			}
			t.mu.Lock()
			if gen == t.gen {
				t.dirs[dir] = m
			}
			t.mu.Unlock()
			return "", nil
		})
		if err != nil && !errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		// Read the result back from the cache (or fetch again if a new
		// generation started meanwhile, or list the backing now that the
		// machine is marked unreachable).
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// listBacking lists dir from the committed tree of the backing; cause is
// returned if there is none.
func (t *RemoteTree) listBacking(ctx context.Context, dir string, cause error) (map[string]proto.OutEntry, error) {
	tr, _, err := t.resolveBacking(ctx)
	if err != nil {
		return nil, cause
	}
	if dir != "" {
		if n, ok := tr.Get(dir); !ok || n.Kind != Dir {
			return nil, fs.ErrNotExist
		}
	}
	ch := tr.Children(dir)
	m := make(map[string]proto.OutEntry, len(ch))
	for _, n := range ch {
		m[path.Base(n.Path)] = entryOfNode(n)
	}
	return m, nil
}

// Stat returns the remote entry of rel.
func (t *RemoteTree) Stat(ctx context.Context, rel string) (proto.OutEntry, error) {
	rel = cleanRel(rel)
	if rel == "" {
		return proto.OutEntry{Kind: manifest.Dir, Mode: 0o755}, nil
	}
	m, err := t.List(ctx, path.Dir(rel))
	if err != nil {
		return proto.OutEntry{}, err
	}
	e, ok := m[path.Base(rel)]
	if !ok {
		return proto.OutEntry{}, fs.ErrNotExist
	}
	return e, nil
}

// Content returns the local path of the content of the remote file rel
// described by e: from the local tiers, else downloaded from the peer
// into pages, else (peer unreachable) from the store pages of the
// committed slot.
func (t *RemoteTree) Content(ctx context.Context, rel string, e proto.OutEntry) (string, error) {
	rel = cleanRel(rel)
	if p, ok := t.localContent(ctx, rel, e.Size, e.MtimeNs); ok {
		return p, nil
	}
	key := fmt.Sprintf("read\x00%s\x00%d\x00%d", rel, e.Size, e.MtimeNs)
	return t.do(key, func() (string, error) {
		t.mu.Lock()
		src, err := t.availableLocked()
		gen := t.gen
		t.mu.Unlock()
		if err != nil {
			return t.contentBacking(ctx, rel, e, err)
		}
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		pr, pw := io.Pipe()
		go func() {
			cw := &countWriter{w: pw}
			err := src.Read(rctx, rel, e.Size, e.MtimeNs, cw)
			if err == nil && cw.n != e.Size {
				err = fmt.Errorf("short read: %d of %d bytes", cw.n, e.Size)
			}
			pw.CloseWithError(err)
		}()
		p, err := t.Ingest(rctx, rel, e.Size, e.MtimeNs, pr)
		pr.CloseWithError(err)
		if err != nil {
			err = t.classify(gen, rel, err)
			switch {
			case errors.Is(err, ErrStale):
				t.mu.Lock()
				if gen == t.gen {
					delete(t.dirs, path.Dir(rel))
				}
				t.mu.Unlock()
			case errors.Is(err, ErrUnavailable):
				return t.contentBacking(ctx, rel, e, err)
			}
			return "", err
		}
		return p, nil
	})
}

// contentBacking materializes rel from the store pages of the committed
// slot if it holds the listed version (same size and mtime); cause is
// returned otherwise.
func (t *RemoteTree) contentBacking(ctx context.Context, rel string, e proto.OutEntry, cause error) (string, error) {
	tr, tier, err := t.resolveBacking(ctx)
	if err != nil {
		t.log.Info("outputs: not available locally", "path", rel)
		return "", cause
	}
	n, ok := tr.Get(rel)
	if !ok || n.Kind != File || n.Size != e.Size || n.MtimeNs != e.MtimeNs {
		t.log.Info("outputs: listed version not in the store", "path", rel)
		return "", cause
	}
	p, err := t.files.Ensure(ctx, tier, n)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	t.mu.Lock()
	t.index[rel] = indexed{Size: n.Size, MtimeNs: n.MtimeNs, Node: *n}
	t.mu.Unlock()
	return p, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func cleanRel(p string) string {
	p = path.Clean("/" + p)
	if p == "/" {
		return ""
	}
	return p[1:]
}

// Source returns the build directory prefix (relative to the repository,
// e.g. "target") of the tree as a Source: the lower layer of the output
// overlay mounted there. Its node paths are relative to prefix.
func (t *RemoteTree) Source(prefix string) *RemoteSource {
	return &RemoteSource{t: t, prefix: cleanRel(prefix)}
}

// RemoteSource is one build directory of a RemoteTree as a Source. It
// implements StatSource: lookups distinguish absent (fs.ErrNotExist),
// unreachable and not cached (ErrUnavailable) and remote errors, and
// Fetch fails with ErrStale when the file changed since it was listed.
type RemoteSource struct {
	t      *RemoteTree
	prefix string
}

var _ StatSource = (*RemoteSource)(nil)

func (s *RemoteSource) full(rel string) string {
	rel = cleanRel(rel)
	switch {
	case rel == "":
		return s.prefix
	case s.prefix == "":
		return rel
	}
	return s.prefix + "/" + rel
}

func nodeOfEntry(rel string, e proto.OutEntry) *Node {
	return &Node{Path: rel, Kind: e.Kind, Mode: e.Mode, Size: e.Size, MtimeNs: e.MtimeNs, Target: e.Target}
}

// Stat implements StatSource.
func (s *RemoteSource) Stat(ctx context.Context, rel string) (*Node, error) {
	rel = cleanRel(rel)
	e, err := s.t.Stat(ctx, s.full(rel))
	if err != nil {
		return nil, err
	}
	return nodeOfEntry(rel, e), nil
}

// Node implements Source.
func (s *RemoteSource) Node(rel string) (*Node, bool) {
	n, err := s.Stat(context.Background(), rel)
	return n, err == nil
}

// Children implements Source.
func (s *RemoteSource) Children(dir string) ([]*Node, error) {
	dir = cleanRel(dir)
	m, err := s.t.List(context.Background(), s.full(dir))
	if err != nil {
		return nil, err
	}
	out := make([]*Node, 0, len(m))
	for name, e := range m {
		out = append(out, nodeOfEntry(join(dir, name), e))
	}
	return out, nil
}

func entryOfNode(n *Node) proto.OutEntry {
	return proto.OutEntry{Name: path.Base(n.Path), Kind: n.Kind, Mode: n.Mode, Size: n.Size, MtimeNs: n.MtimeNs, Target: n.Target}
}

// Local implements Source.
func (s *RemoteSource) Local(n *Node) (string, bool) {
	rel := s.full(n.Path)
	s.t.mu.Lock()
	ix, ok := s.t.index[rel]
	s.t.mu.Unlock()
	if ok && ix.Size == n.Size && ix.MtimeNs == n.MtimeNs {
		return s.t.files.Local(&ix.Node)
	}
	return "", false
}

// Fetch implements Source.
func (s *RemoteSource) Fetch(ctx context.Context, n *Node) (string, error) {
	return s.t.Content(ctx, s.full(n.Path), entryOfNode(n))
}

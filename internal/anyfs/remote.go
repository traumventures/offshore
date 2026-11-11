package anyfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/cas"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
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

// RemoteTree caches the remote view of one repository's build
// directories. Directory listings are fetched lazily, one directory at a
// time, and belong to a generation that ends with every new remote run
// (Update); file contents are kept across generations in the content
// store (the laptop CAS), indexed by path, size and mtime, so unchanged
// outputs are never downloaded twice. Source(prefix) serves one build
// directory as the lower layer of a UserOverlay.
type RemoteTree struct {
	store *cas.Store
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
}

type indexed struct {
	Size    int64
	MtimeNs int64
	Hash    string
}

type flight struct {
	done chan struct{}
	val  string
	err  error
}

// NewRemoteTree creates an empty tree backed by store.
func NewRemoteTree(store *cas.Store, log *slog.Logger) *RemoteTree {
	return &RemoteTree{store: store, log: log, dirs: map[string]map[string]proto.OutEntry{}, missing: map[string]bool{},
		index: map[string]indexed{}, flights: map[string]*flight{}, now: time.Now}
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

// Hint records known contents (eager outputs that were already
// transferred into the store), so they are served without the machine.
func (t *RemoteTree) Hint(files []manifest.Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range files {
		if e.Kind == manifest.File && e.Hash != "" {
			t.index[e.Path] = indexed{Size: e.Size, MtimeNs: e.MtimeNs, Hash: e.Hash}
		}
	}
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
			return nil, err
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
		if err != nil {
			return nil, err
		}
		// Read the result back from the cache (or fetch again if a new
		// generation started meanwhile).
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
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
// described by e, downloading it into the store if needed.
func (t *RemoteTree) Content(ctx context.Context, rel string, e proto.OutEntry) (string, error) {
	rel = cleanRel(rel)
	t.mu.Lock()
	ix, ok := t.index[rel]
	t.mu.Unlock()
	if ok && ix.Size == e.Size && ix.MtimeNs == e.MtimeNs && t.store.Has(ix.Hash) {
		return t.store.Path(ix.Hash), nil
	}
	key := fmt.Sprintf("read\x00%s\x00%d\x00%d", rel, e.Size, e.MtimeNs)
	hash, err := t.do(key, func() (string, error) {
		t.mu.Lock()
		src, err := t.availableLocked()
		gen := t.gen
		t.mu.Unlock()
		if err != nil {
			t.log.Info("outputs: not available locally", "path", rel)
			return "", err
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
		h, err := t.store.Ingest(pr)
		pr.CloseWithError(err)
		if err != nil {
			err = t.classify(gen, rel, err)
			if errors.Is(err, ErrStale) {
				t.mu.Lock()
				if gen == t.gen {
					delete(t.dirs, path.Dir(rel))
				}
				t.mu.Unlock()
			}
			return "", err
		}
		t.mu.Lock()
		t.index[rel] = indexed{Size: e.Size, MtimeNs: e.MtimeNs, Hash: h}
		t.mu.Unlock()
		return h, nil
	})
	if err != nil {
		return "", err
	}
	return t.store.Path(hash), nil
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
	if ok && ix.Size == n.Size && ix.MtimeNs == n.MtimeNs && s.t.store.Has(ix.Hash) {
		return s.t.store.Path(ix.Hash), true
	}
	return "", false
}

// Fetch implements Source.
func (s *RemoteSource) Fetch(ctx context.Context, n *Node) (string, error) {
	return s.t.Content(ctx, s.full(n.Path), entryOfNode(n))
}

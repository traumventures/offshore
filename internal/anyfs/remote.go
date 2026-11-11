package anyfs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
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

// NotCommitted is the reason reported for an output the laptop never
// fetched whose machine is gone before the build slot was committed.
const NotCommitted = "machine gone, output not committed; rerun the build"

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
//
// Contents the laptop fetched or ingested (eager outputs) are pinned in
// Files until they are known to be committed (CheckCommitted) or
// superseded by a newer listing: when the machine disappears before its
// slot is committed, they still appear in listings served from the
// backing and keep their contents. Outputs that are neither fetched nor
// committed fail with NotCommitted and are reported by Unavailable.
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
	pinned    map[string]bool // index paths whose content is pinned
	unavail   []Unavailable   // most recent first
	downUntil time.Time
	downErr   error
	downGen   uint64 // generation of the last "unreachable" log line
	flights   map[string]*flight
	hooks     []func()
	now       func() time.Time

	indexPath string      // persisted index ("": in memory only)
	saveTimer *time.Timer // pending debounced save

	backing  Backing
	bGen     uint64 // generation of the resolution below
	bTree    *Tree
	bTier    Tier // local -> store
	bErr     error
	bUntil   time.Time
	ingested atomic.Uint64
}

// Unavailable is an output that could not be served.
type Unavailable struct {
	Path   string    `json:"path"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// maxUnavailable bounds the reported unavailable outputs.
const maxUnavailable = 50

// indexed is the known content of a remote file: Node carries its digest,
// mode and pages. Pinned contents are not known to be committed.
type indexed struct {
	Size    int64 `json:"size"`
	MtimeNs int64 `json:"mtime_ns"`
	Node    Node  `json:"node"`
	Pinned  bool  `json:"pinned,omitempty"`
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
		index: map[string]indexed{}, pinned: map[string]bool{}, flights: map[string]*flight{}, now: time.Now}
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
	t.unavail = nil // reported for the build they belong to
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
	if t.saveTimer != nil {
		t.saveTimer.Stop()
		t.saveTimer = nil
	}
	t.mu.Unlock()
	if src != nil {
		src.Close()
	}
	if err := t.SaveIndex(); err != nil {
		t.log.Warn("outputs: cannot save index", "err", err)
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

// Ingest stores the content of the remote file rel (size, mtime and mode
// as listed in e) from r as pages in the local tier and as a materialized
// file, so it is served without the machine (e.g. eager outputs the proxy
// already transferred), and returns the path of the materialized file.
// The content stays pinned until it is known to be committed.
func (t *RemoteTree) Ingest(ctx context.Context, rel string, e proto.OutEntry, r io.Reader) (string, error) {
	size, mtimeNs := e.Size, e.MtimeNs
	rel = cleanRel(rel)
	tmp := filepath.Join(t.files.dir, fmt.Sprintf(".ingest-%d-%d", time.Now().UnixNano(), t.ingested.Add(1)))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp) // moved into place on success
	h := blake3.New()
	buf := make([]byte, PageSize)
	n := Node{Path: rel, Kind: File, Mode: e.Mode, MtimeNs: mtimeNs}
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
	t.files.Pin(&n) // before it is resident: no reclaim in between
	err = t.files.Adopt(&n, tmp, true, true)
	if err == nil {
		t.mu.Lock()
		t.setIndexLocked(rel, indexed{Size: size, MtimeNs: mtimeNs, Node: n, Pinned: true})
		t.mu.Unlock()
	}
	t.files.Unpin(&n)
	if err != nil {
		return "", err
	}
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
			t.supersedeLocked(dir, m)
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

// listBacking lists dir from the committed tree of the backing merged
// with the pinned outputs (fetched but maybe not committed; newest mtime
// wins); cause is returned if there is neither.
func (t *RemoteTree) listBacking(ctx context.Context, dir string, cause error) (map[string]proto.OutEntry, error) {
	tr, _, err := t.resolveBacking(ctx)
	t.mu.Lock()
	pinned := t.pinnedChildrenLocked(dir)
	t.mu.Unlock()
	m := map[string]proto.OutEntry{}
	if err != nil {
		if len(pinned) == 0 {
			return nil, cause
		}
	} else if n, ok := tr.Get(dir); dir == "" || ok && n.Kind == Dir {
		for _, n := range tr.Children(dir) {
			m[path.Base(n.Path)] = entryOfNode(n)
		}
	} else if len(pinned) == 0 {
		return nil, fs.ErrNotExist
	}
	for name, e := range pinned {
		c, ok := m[name]
		switch {
		case !ok:
			m[name] = e
		case e.Kind == manifest.File && c.Kind != manifest.Dir && e.MtimeNs > c.MtimeNs:
			m[name] = e
		}
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
		p, err := t.Ingest(rctx, rel, e, pr)
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
// slot if it holds the listed version (same size and mtime); otherwise
// the output is unavailable (reported with the reason).
func (t *RemoteTree) contentBacking(ctx context.Context, rel string, e proto.OutEntry, cause error) (string, error) {
	tr, tier, err := t.resolveBacking(ctx)
	if err != nil {
		return "", t.unavailable(rel, NotCommitted+" (committed outputs unavailable too)", cause)
	}
	n, ok := tr.Get(rel)
	if !ok || n.Kind != File || n.Size != e.Size || n.MtimeNs != e.MtimeNs {
		return "", t.unavailable(rel, NotCommitted, cause)
	}
	p, err := t.files.Ensure(ctx, tier, n)
	if err != nil {
		return "", t.unavailable(rel, "committed output cannot be read from the store", err)
	}
	t.mu.Lock()
	t.setIndexLocked(rel, indexed{Size: n.Size, MtimeNs: n.MtimeNs, Node: *n})
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

// setIndexLocked records the content of rel, moving the pin from the
// previous content to the new one.
func (t *RemoteTree) setIndexLocked(rel string, ix indexed) {
	old, had := t.index[rel]
	t.index[rel] = ix
	delete(t.pinned, rel)
	if ix.Pinned {
		t.files.Pin(&ix.Node) // first: the same content stays pinned throughout
		t.pinned[rel] = true
	}
	if had && old.Pinned {
		t.files.Unpin(&old.Node)
	}
	t.scheduleSaveLocked()
}

// unpinLocked releases the pin of rel (committed or superseded).
func (t *RemoteTree) unpinLocked(rel string) {
	ix, ok := t.index[rel]
	if !ok || !ix.Pinned {
		return
	}
	ix.Pinned = false
	t.index[rel] = ix
	delete(t.pinned, rel)
	t.files.Unpin(&ix.Node)
	t.scheduleSaveLocked()
}

// SetIndexFile persists the index (known contents and their pins) in
// path, loading what a previous run saved there: pinned outputs survive
// daemon restarts.
func (t *RemoteTree) SetIndexFile(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.indexPath = path
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved map[string]indexed
	if err := json.Unmarshal(b, &saved); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for rel, ix := range saved {
		if _, ok := t.index[rel]; !ok {
			t.setIndexLocked(rel, ix)
		}
	}
	return nil
}

func (t *RemoteTree) scheduleSaveLocked() {
	if t.indexPath == "" || t.saveTimer != nil {
		return
	}
	t.saveTimer = time.AfterFunc(2*time.Second, func() {
		t.mu.Lock()
		t.saveTimer = nil
		t.mu.Unlock()
		t.SaveIndex()
	})
}

// SaveIndex writes the persisted index now.
func (t *RemoteTree) SaveIndex() error {
	t.mu.Lock()
	p := t.indexPath
	if p == "" {
		t.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(t.index)
	t.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Known reports whether rel is indexed with the content digest in the
// version e (size and mtime) and that content is resident.
func (t *RemoteTree) Known(rel string, e proto.OutEntry, digest string) bool {
	t.mu.Lock()
	ix, ok := t.index[cleanRel(rel)]
	t.mu.Unlock()
	if !ok || ix.Size != e.Size || ix.MtimeNs != e.MtimeNs || ix.Node.Digest != digest || ix.Node.Mode != e.Mode {
		return false
	}
	_, ok = t.files.Local(&ix.Node)
	return ok
}

// Pinned returns the number of pinned (not known to be committed)
// outputs and their bytes.
func (t *RemoteTree) Pinned() (n int, bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for rel := range t.pinned {
		n++
		bytes += t.index[rel].Size
	}
	return n, bytes
}

// Unavailable returns the most recent outputs that could not be served,
// newest first.
func (t *RemoteTree) Unavailable() []Unavailable {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Unavailable(nil), t.unavail...)
}

// unavailable records and logs an output that cannot be served and
// returns the error for the caller (ErrUnavailable).
func (t *RemoteTree) unavailable(rel, reason string, cause error) error {
	t.mu.Lock()
	machine := ""
	if t.src != nil {
		machine = t.src.Name()
	}
	u := Unavailable{Path: rel, Reason: reason, At: t.now()}
	out := []Unavailable{u}
	for _, o := range t.unavail {
		if o.Path != rel && len(out) < maxUnavailable {
			out = append(out, o)
		}
	}
	t.unavail = out
	t.mu.Unlock()
	t.log.Warn("outputs: "+rel+": "+reason, "path", rel, "machine", machine, "err", cause)
	return fmt.Errorf("%s: %s: %w", rel, reason, ErrUnavailable)
}

// CheckCommitted resolves the committed state of the slot (the backing)
// and releases the pins of outputs it holds in the same or a newer
// version; it returns the number released.
func (t *RemoteTree) CheckCommitted(ctx context.Context) (int, error) {
	t.mu.Lock()
	b := t.backing
	empty := len(t.pinned) == 0
	t.mu.Unlock()
	if b == nil || empty {
		return 0, nil
	}
	tr, _, err := b(ctx)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	released := 0
	for rel := range t.pinned {
		ix := t.index[rel]
		n, ok := tr.Get(rel)
		if !ok || n.Kind != File {
			continue
		}
		if n.Digest == ix.Node.Digest || (n.Size == ix.Size && n.MtimeNs == ix.MtimeNs) || n.MtimeNs > ix.MtimeNs {
			t.unpinLocked(rel)
			released++
		}
	}
	return released, nil
}

// supersedeLocked releases the pins of dir's indexed files that a fresh
// listing m of the peer no longer holds in the same version.
func (t *RemoteTree) supersedeLocked(dir string, m map[string]proto.OutEntry) {
	for rel := range t.pinned {
		if parent := path.Dir(rel); parent != dir && !(parent == "." && dir == "") {
			continue
		}
		ix := t.index[rel]
		if e, ok := m[path.Base(rel)]; !ok || e.Kind != File || e.Size != ix.Size || e.MtimeNs != ix.MtimeNs {
			t.unpinLocked(rel)
		}
	}
}

// pinnedChildrenLocked returns the entries of dir implied by pinned
// outputs: their files and the directories leading to them.
func (t *RemoteTree) pinnedChildrenLocked(dir string) map[string]proto.OutEntry {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	out := map[string]proto.OutEntry{}
	for rel := range t.pinned {
		rest, ok := strings.CutPrefix(rel, prefix)
		if !ok || rest == "" {
			continue
		}
		ix := t.index[rel]
		name, _, nested := strings.Cut(rest, "/")
		if !nested {
			out[name] = proto.OutEntry{Name: name, Kind: manifest.File, Mode: ix.Node.Mode, Size: ix.Size, MtimeNs: ix.MtimeNs}
			continue
		}
		if e, ok := out[name]; !ok || e.Kind == manifest.Dir && e.MtimeNs < ix.MtimeNs {
			out[name] = proto.OutEntry{Name: name, Kind: manifest.Dir, Mode: 0o755, MtimeNs: ix.MtimeNs}
		}
	}
	return out
}

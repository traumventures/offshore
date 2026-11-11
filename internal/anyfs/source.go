package anyfs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Source is a read-only view served by LowerFS or used as the lower
// layer of a UserOverlay: metadata by path, content as local files.
type Source interface {
	// Node returns the node at rel ("" is the root directory).
	Node(rel string) (*Node, bool)
	// Children lists a directory.
	Children(dir string) ([]*Node, error)
	// Local returns a local file with n's content if it is resident
	// (never fetches).
	Local(n *Node) (string, bool)
	// Fetch makes n's content local (a page fault) and returns its path.
	Fetch(ctx context.Context, n *Node) (string, error)
}

// rootNode is the implicit root directory.
var rootNode = Node{Kind: Dir, Mode: 0o755}

// TreeSource serves a Tree whose contents are materialized by Files from
// the pages of a Tier, or by a FileFetcher (a peer serving whole files by
// digest). The tree can be replaced (Update, Apply) while mounted.
type TreeSource struct {
	files *Files
	tier  Tier
	fetch FileFetcher
	mu    sync.RWMutex
	tree  *Tree
}

// NewTreeSource returns a source for t.
func NewTreeSource(t *Tree, files *Files, tier Tier) *TreeSource {
	if t == nil {
		t = NewTree(nil)
	}
	return &TreeSource{files: files, tier: tier, tree: t}
}

// NewPeerSource returns a source for t whose file contents are fetched
// whole by fetch (the peer tier: e.g. the laptop over QUIC, keyed by the
// content digest of its manifest).
func NewPeerSource(t *Tree, files *Files, fetch FileFetcher) *TreeSource {
	s := NewTreeSource(t, files, nil)
	s.fetch = fetch
	return s
}

// Tree returns the current tree.
func (s *TreeSource) Tree() *Tree {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tree
}

// Update replaces the tree and returns the paths that changed (new,
// changed or removed), for kernel invalidation.
func (s *TreeSource) Update(t *Tree) []string {
	s.mu.Lock()
	old := s.tree
	s.tree = t
	s.mu.Unlock()
	var out []string
	for _, n := range Diff(old, t) {
		out = append(out, n.Path)
	}
	return out
}

// Apply puts layer on top of the tree (with full, the layer's nodes
// replace the tree) and returns the paths that changed: the layer's
// paths and every path below a removed or replaced directory. Cheaper
// than Update for small deltas of large trees.
func (s *TreeSource) Apply(layer []Node, full bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.tree
	if full {
		s.tree = NewTree(layer)
		var out []string
		for _, n := range Diff(old, s.tree) {
			out = append(out, n.Path)
		}
		return out
	}
	s.tree = Apply(old, layer)
	var out []string
	var below func(dir string)
	below = func(dir string) {
		for _, c := range old.Children(dir) {
			out = append(out, c.Path)
			if c.Kind == Dir {
				below(c.Path)
			}
		}
	}
	for _, n := range layer {
		out = append(out, n.Path)
		if n.Whiteout || n.Kind != Dir || n.Opaque {
			if o, ok := old.Get(n.Path); ok && o.Kind == Dir {
				below(n.Path)
			}
		}
	}
	return out
}

// Node implements Source.
func (s *TreeSource) Node(rel string) (*Node, bool) {
	if rel == "" {
		return &rootNode, true
	}
	return s.Tree().Get(rel)
}

// Children implements Source.
func (s *TreeSource) Children(dir string) ([]*Node, error) {
	t := s.Tree()
	if dir != "" {
		if n, ok := t.Get(dir); !ok || !n.IsDir() {
			return nil, syscall.ENOTDIR
		}
	}
	return t.Children(dir), nil
}

// Local implements Source.
func (s *TreeSource) Local(n *Node) (string, bool) { return s.files.Local(n) }

// Fetch implements Source.
func (s *TreeSource) Fetch(ctx context.Context, n *Node) (string, error) {
	if s.fetch != nil {
		return s.files.EnsureWith(ctx, n, s.fetch)
	}
	return s.files.Ensure(ctx, s.tier, n)
}

// Files returns the materialized file cache.
func (s *TreeSource) Files() *Files { return s.files }

// DirSource is a local directory as a Source (content is always local).
type DirSource struct{ Dir string }

func (s DirSource) path(rel string) string { return filepath.Join(s.Dir, filepath.FromSlash(rel)) }

// Node implements Source.
func (s DirSource) Node(rel string) (*Node, bool) {
	fi, err := os.Lstat(s.path(rel))
	if err != nil {
		return nil, false
	}
	n := nodeOf(rel, fi, s.path(rel))
	return &n, true
}

// Children implements Source.
func (s DirSource) Children(dir string) ([]*Node, error) {
	des, err := os.ReadDir(s.path(dir))
	if err != nil {
		return nil, err
	}
	out := make([]*Node, 0, len(des))
	for _, de := range des {
		rel := de.Name()
		if dir != "" {
			rel = dir + "/" + de.Name()
		}
		if n, ok := s.Node(rel); ok && n.Kind != 0 {
			out = append(out, n)
		}
	}
	return out, nil
}

// Local implements Source.
func (s DirSource) Local(n *Node) (string, bool) { return s.path(n.Path), true }

// Fetch implements Source.
func (s DirSource) Fetch(_ context.Context, n *Node) (string, error) { return s.path(n.Path), nil }

// nodeOf describes a local file (no digest).
func nodeOf(rel string, fi os.FileInfo, p string) Node {
	n := Node{Path: rel, Mode: uint32(fi.Mode().Perm()), MtimeNs: fi.ModTime().UnixNano()}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		n.Kind = Symlink
		n.Target, _ = os.Readlink(p)
	case fi.Mode().IsRegular():
		n.Kind, n.Size = File, fi.Size()
	case fi.IsDir():
		n.Kind = Dir
	}
	return n
}

// StatSource is a Source whose lookups can fail for other reasons than
// absence (a remote listing). Stat returns an error wrapping
// fs.ErrNotExist or ErrUnavailable (both shown as absent by a
// UserOverlay) or another error (EIO).
type StatSource interface {
	Source
	Stat(ctx context.Context, rel string) (*Node, error)
}

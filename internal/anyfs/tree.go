package anyfs

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/traumventures/offshore/internal/manifest"
)

// Kinds of nodes (shared with the source manifest).
const (
	File    = manifest.File
	Dir     = manifest.Dir
	Symlink = manifest.Symlink
)

// Node is one path of a tree: a file, directory or symlink, or (in a
// layer) a whiteout hiding the path of a lower layer. Path is relative,
// uses "/" and is never empty (the root is implicit).
type Node struct {
	Path    string        `json:"p"`
	Kind    manifest.Kind `json:"k,omitempty"`
	Mode    uint32        `json:"m,omitempty"` // permission bits
	Size    int64         `json:"s,omitempty"`
	MtimeNs int64         `json:"t,omitempty"`
	// Digest is the BLAKE3 hex of a file's content (files only).
	Digest string    `json:"d,omitempty"`
	Pages  []PageRef `json:"r,omitempty"`
	Target string    `json:"l,omitempty"` // symlink target
	// Whiteout (layers only) deletes the path and everything below it.
	Whiteout bool `json:"w,omitempty"`
	// Opaque (layer directories only) hides the lower directory's
	// children: the directory was replaced.
	Opaque bool `json:"o,omitempty"`
}

// IsDir, IsFile, IsLink report the node kind.
func (n *Node) IsDir() bool  { return n.Kind == Dir }
func (n *Node) IsFile() bool { return n.Kind == File }
func (n *Node) IsLink() bool { return n.Kind == Symlink }

// FileMode returns the node's mode as an fs.FileMode.
func (n *Node) FileMode() fs.FileMode {
	m := fs.FileMode(n.Mode & 0o7777)
	switch n.Kind {
	case Dir:
		m |= fs.ModeDir
	case Symlink:
		m |= fs.ModeSymlink
	}
	return m
}

// SameContent reports whether two nodes describe the same object
// (timestamps ignored).
func (n *Node) SameContent(o *Node) bool {
	return n.Kind == o.Kind && n.Mode == o.Mode && n.Size == o.Size && n.Digest == o.Digest &&
		n.Target == o.Target && n.Whiteout == o.Whiteout
}

// Same reports whether two nodes are identical including the mtime
// (directories: kind and mode only; their mtimes follow the children).
func (n *Node) Same(o *Node) bool {
	if n.Kind == Dir && o.Kind == Dir {
		return n.Mode == o.Mode && n.Whiteout == o.Whiteout && n.Opaque == o.Opaque
	}
	return n.SameContent(o) && n.MtimeNs == o.MtimeNs
}

// Entry converts a node to a source manifest entry.
func (n *Node) Entry() manifest.Entry {
	return manifest.Entry{Path: n.Path, Kind: n.Kind, Mode: n.Mode, Size: n.Size, MtimeNs: n.MtimeNs, Hash: n.Digest, Target: n.Target}
}

// FromEntry converts a source manifest entry to a node (no pages).
func FromEntry(e manifest.Entry) Node {
	return Node{Path: e.Path, Kind: e.Kind, Mode: e.Mode, Size: e.Size, MtimeNs: e.MtimeNs, Digest: e.Hash, Target: e.Target}
}

// Tree is an immutable set of nodes sorted by path (parents before
// children), indexed by path and by directory.
type Tree struct {
	nodes []Node
	idx   map[string]int
	kids  map[string][]int // directory ("" = root) -> child indexes
	bytes int64
}

// NewTree builds a tree from nodes (copied, sorted; whiteouts dropped;
// a later node for the same path wins).
func NewTree(nodes []Node) *Tree {
	ns := make([]Node, 0, len(nodes))
	seen := map[string]int{}
	for _, n := range nodes {
		if n.Whiteout || n.Path == "" {
			continue
		}
		n.Opaque = false
		if i, ok := seen[n.Path]; ok {
			ns[i] = n
			continue
		}
		seen[n.Path] = len(ns)
		ns = append(ns, n)
	}
	sortNodes(ns)
	t := &Tree{nodes: ns, idx: make(map[string]int, len(ns)), kids: map[string][]int{}}
	for i := range ns {
		n := &ns[i]
		t.idx[n.Path] = i
		d := parent(n.Path)
		t.kids[d] = append(t.kids[d], i)
		if n.Kind == File {
			t.bytes += n.Size
		}
	}
	return t
}

// sortNodes orders nodes so that a directory precedes its children:
// by path with "/" sorting before every other byte.
func sortNodes(ns []Node) {
	sort.Slice(ns, func(i, j int) bool { return pathLess(ns[i].Path, ns[j].Path) })
}

func pathLess(a, b string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if ca, cb := a[i], b[i]; ca != cb {
			switch {
			case ca == '/':
				return true
			case cb == '/':
				return false
			}
			return ca < cb
		}
	}
	return len(a) < len(b)
}

func parent(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// Len is the number of nodes.
func (t *Tree) Len() int {
	if t == nil {
		return 0
	}
	return len(t.nodes)
}

// Bytes is the total size of the files.
func (t *Tree) Bytes() int64 {
	if t == nil {
		return 0
	}
	return t.bytes
}

// Nodes returns the nodes in order (do not modify).
func (t *Tree) Nodes() []Node {
	if t == nil {
		return nil
	}
	return t.nodes
}

// Get returns the node at p.
func (t *Tree) Get(p string) (*Node, bool) {
	if t == nil {
		return nil, false
	}
	i, ok := t.idx[p]
	if !ok {
		return nil, false
	}
	return &t.nodes[i], true
}

// Children returns the nodes directly below dir ("" = root), in order.
func (t *Tree) Children(dir string) []*Node {
	if t == nil {
		return nil
	}
	is := t.kids[dir]
	out := make([]*Node, len(is))
	for k, i := range is {
		out[k] = &t.nodes[i]
	}
	return out
}

// Keys returns the distinct page keys the tree references.
func (t *Tree) Keys() []Key {
	seen := map[Key]bool{}
	var out []Key
	for i := range t.Nodes() {
		for _, r := range t.nodes[i].Pages {
			if !seen[r.Key] {
				seen[r.Key] = true
				out = append(out, r.Key)
			}
		}
	}
	return out
}

// Diff returns the layer that turns base into top: nodes of top that are
// new or changed, and whiteouts for paths of base missing in top (one
// whiteout per removed subtree). A directory that replaces a non
// directory is opaque.
func Diff(base, top *Tree) []Node {
	var out []Node
	for _, n := range top.Nodes() {
		b, ok := base.Get(n.Path)
		switch {
		case !ok:
			out = append(out, n)
		case !b.Same(&n):
			if n.Kind == Dir && b.Kind != Dir {
				n.Opaque = true
			}
			out = append(out, n)
		}
	}
	removed := ""
	for _, b := range base.Nodes() {
		if removed != "" && strings.HasPrefix(b.Path, removed+"/") {
			continue
		}
		if t, ok := top.Get(b.Path); ok {
			if b.Kind == Dir && t.Kind != Dir {
				removed = b.Path // children hidden by the replacing node
			}
			continue
		}
		out = append(out, Node{Path: b.Path, Whiteout: true})
		removed = b.Path
	}
	sortNodes(out)
	return out
}

// Apply returns base with the layer applied on top (top wins): whiteouts
// delete subtrees, a non directory node replaces a subtree, opaque
// directories drop the lower children.
func Apply(base *Tree, layer []Node) *Tree {
	if len(layer) == 0 {
		return base
	}
	gone := map[string]bool{} // subtrees removed from base
	over := map[string]*Node{}
	for i := range layer {
		n := &layer[i]
		over[n.Path] = n
		if n.Whiteout || n.Kind != Dir || n.Opaque {
			gone[n.Path] = true
		}
	}
	hidden := func(p string) bool {
		for d := p; d != ""; d = parent(d) {
			if gone[d] && d != p {
				return true
			}
		}
		return false
	}
	out := make([]Node, 0, base.Len()+len(layer))
	for _, b := range base.Nodes() {
		if _, ok := over[b.Path]; ok || hidden(b.Path) {
			continue
		}
		out = append(out, b)
	}
	for _, n := range layer {
		if !n.Whiteout && !hiddenBy(over, n.Path) {
			out = append(out, n)
		}
	}
	return NewTree(out)
}

// hiddenBy reports whether a layer node lies below a whiteout or non
// directory of the same layer (inconsistent layers: the upper entry wins).
func hiddenBy(over map[string]*Node, p string) bool {
	for d := parent(p); d != ""; d = parent(d) {
		if n, ok := over[d]; ok && (n.Whiteout || n.Kind != Dir) {
			return true
		}
	}
	return false
}

// Merge folds layers bottom to top into one tree.
func Merge(layers ...[]Node) *Tree {
	t := NewTree(nil)
	for _, l := range layers {
		t = Apply(t, l)
	}
	return t
}

// MergeNewest merges two trees: where both have a non directory at the
// same path the newer mtime wins (ties: a), everything else is the union
// (a's node wins on kind conflicts). Used where both sides are written
// independently (the laptop's output overlay over a remote build).
func MergeNewest(a, b *Tree) *Tree {
	out := make([]Node, 0, a.Len()+b.Len())
	out = append(out, a.Nodes()...)
	for _, n := range b.Nodes() {
		o, ok := a.Get(n.Path)
		if !ok {
			if !shadowed(a, n.Path) {
				out = append(out, n)
			}
			continue
		}
		if o.Kind != Dir && n.Kind != Dir && n.MtimeNs > o.MtimeNs {
			out = append(out, n) // later node wins in NewTree
		}
	}
	return NewTree(out)
}

// shadowed reports whether an ancestor of p is a non directory in t.
func shadowed(t *Tree, p string) bool {
	for d := parent(p); d != ""; d = parent(d) {
		if n, ok := t.Get(d); ok {
			return n.Kind != Dir
		}
	}
	return false
}

// Encode writes nodes as gzip compressed JSON.
func Encode(w io.Writer, v any) error {
	zw := gzip.NewWriter(w)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		return err
	}
	return zw.Close()
}

// Decode reads what Encode wrote.
func Decode(r io.Reader, v any) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	return json.NewDecoder(zr).Decode(v)
}

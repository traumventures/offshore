package anyfs

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// UpperFormat is how an overlay's upper directory records deletions.
type UpperFormat int

const (
	// UpperKernel: kernel overlayfs (whiteouts are 0/0 character devices
	// or files with an overlay.whiteout xattr; opaque and redirected
	// directories carry overlay.opaque and overlay.redirect xattrs).
	UpperKernel UpperFormat = iota
	// UpperUser: the userspace overlay (.wh.NAME markers, .wh..wh..opq).
	UpperUser
)

// ChangeOptions tune ScanChanges.
type ChangeOptions struct {
	ScanOptions
	// Upper is the overlay's upper directory ("": dir is a plain
	// directory, which is walked whole with the ctime watermark).
	Upper       string
	UpperFormat UpperFormat
}

// ScanChanges builds the tree of dir (an overlay's merged view) from the
// tree it had at the watermark (opt.Prev at opt.Since) and what changed
// since: only the entries of the upper directory whose ctime is at or
// after the watermark are stat'ed in the merged view (and their
// directories listed); everything else is taken from Prev unread. A
// rename, deletion or creation changes the ctime of the directories
// involved, a write that of the file; whiteouts and opaque or redirected
// directories (a renamed lower directory) cause a rescan of their
// subtree. Without an upper directory, a watermark or Prev, dir is
// scanned whole (Scan with the watermark).
func ScanChanges(dir string, opt ChangeOptions) (*Tree, ScanStats, error) {
	if opt.Upper == "" || opt.Since <= 0 || opt.Prev == nil {
		return Scan(dir, opt.ScanOptions)
	}
	c := &changeScan{dir: dir, opt: opt, nodes: map[string]*scanned{}, added: map[string]bool{}, kids: map[string]map[string]bool{}, rescanned: map[string]bool{}}
	for _, n := range opt.Prev.Nodes() {
		c.nodes[n.Path] = &scanned{Node: n}
	}
	ops, err := c.upperOps()
	if err != nil {
		return nil, c.st, err
	}
	for _, o := range ops {
		if c.underRescan(o.path) {
			continue
		}
		var err error
		switch o.kind {
		case opRescan:
			err = c.rescan(o.path)
		case opDir:
			err = c.listDir(o.path)
		default:
			err = c.entry(o.path)
		}
		if err != nil {
			return nil, c.st, err
		}
	}
	var changed []*scanned
	all := make([]scanned, 0, len(c.nodes))
	for p, n := range c.nodes {
		if c.added[p] {
			changed = append(changed, n)
		}
	}
	if err := hashNodes(dir, changed, opt.ScanOptions, &c.st); err != nil {
		return nil, c.st, err
	}
	for _, n := range c.nodes {
		all = append(all, *n)
	}
	return buildTree(all, &c.st), c.st, nil
}

type opKind int

const (
	opRescan opKind = iota // walk the subtree of the merged view
	opDir                  // the directory's entries changed: list it
	opEntry                // the entry changed: stat it
)

type changeOp struct {
	path string
	kind opKind
}

type changeScan struct {
	dir       string
	opt       ChangeOptions
	st        ScanStats
	nodes     map[string]*scanned
	added     map[string]bool            // nodes set by this scan (files are hashed unless reusable)
	kids      map[string]map[string]bool // dir -> added children Prev lacks
	rescanned map[string]bool
}

// set records a node found by this scan.
func (c *changeScan) set(n scanned) {
	c.nodes[n.Path] = &n
	c.added[n.Path] = true
	if _, prev := c.opt.Prev.Get(n.Path); !prev {
		d := parent(n.Path)
		if c.kids[d] == nil {
			c.kids[d] = map[string]bool{}
		}
		c.kids[d][n.Path] = true
	}
}

// unset forgets a node.
func (c *changeScan) unset(p string) {
	delete(c.nodes, p)
	delete(c.added, p)
	delete(c.kids[parent(p)], p)
}

func overlayXattr(p, name string) bool {
	for _, ns := range []string{"trusted.overlay.", "user.overlay."} {
		if _, err := unix.Lgetxattr(p, ns+name, nil); err == nil {
			return true
		}
	}
	return false
}

// upperOps walks the upper directory and returns what to look at in the
// merged view, parents first.
func (c *changeScan) upperOps() ([]changeOp, error) {
	since := c.opt.Since
	var ops []changeOp
	up := c.opt.Upper
	err := filepath.WalkDir(up, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p != up {
				return nil
			}
			return err
		}
		fi, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		fresh := ctimeOf(fi) >= since
		if p == up {
			if fresh {
				ops = append(ops, changeOp{"", opDir})
			}
			return nil
		}
		rel := filepath.ToSlash(p[len(up)+1:])
		if c.opt.UpperFormat == UpperUser {
			name := path.Base(rel)
			switch {
			case name == OpaqueMarker:
				if fresh {
					ops = append(ops, changeOp{parent(rel), opRescan})
				}
				return nil
			case strings.HasPrefix(name, internalPrefix):
				return nil // temporary files of the overlay
			case strings.HasPrefix(name, WhiteoutPrefix):
				if fresh {
					ops = append(ops, changeOp{path.Join(parent(rel), name[len(WhiteoutPrefix):]), opEntry})
				}
				return nil
			}
		}
		if !fresh {
			return nil // unchanged since the watermark (directories are still walked)
		}
		if d.IsDir() {
			if c.opt.UpperFormat == UpperKernel && (overlayXattr(p, "opaque") || overlayXattr(p, "redirect")) {
				ops = append(ops, changeOp{rel, opRescan})
				return filepath.SkipDir
			}
			ops = append(ops, changeOp{rel, opDir})
			return nil
		}
		ops = append(ops, changeOp{rel, opEntry})
		return nil
	})
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].path != ops[j].path {
			return pathLess(ops[i].path, ops[j].path)
		}
		return ops[i].kind < ops[j].kind
	})
	return ops, err
}

func (c *changeScan) underRescan(p string) bool {
	for q := p; ; q = parent(q) {
		if c.rescanned[q] {
			return true
		}
		if q == "" {
			return false
		}
	}
}

// remove drops a node and everything below it.
func (c *changeScan) remove(p string) {
	if p == "" {
		c.nodes, c.added, c.kids = map[string]*scanned{}, map[string]bool{}, map[string]map[string]bool{}
		return
	}
	if n := c.nodes[p]; n == nil || n.Kind != Dir {
		c.unset(p)
		return
	}
	for _, k := range c.children(p) {
		c.remove(k)
	}
	c.unset(p)
}

// children returns the current paths directly below dir.
func (c *changeScan) children(dir string) []string {
	var out []string
	for _, n := range c.opt.Prev.Children(dir) {
		if c.nodes[n.Path] != nil {
			out = append(out, n.Path)
		}
	}
	for p := range c.kids[dir] {
		out = append(out, p)
	}
	return out
}

func (c *changeScan) skip(rel string, fi fs.FileInfo) bool {
	return c.opt.Skip != nil && c.opt.Skip(rel, fs.FileInfoToDirEntry(fi))
}

func (c *changeScan) abs(rel string) string {
	if rel == "" {
		return c.dir
	}
	return filepath.Join(c.dir, filepath.FromSlash(rel))
}

// rescan replaces the subtree at p with a walk of the merged view.
func (c *changeScan) rescan(p string) error {
	c.remove(p)
	c.rescanned[p] = true
	if p != "" {
		fi, err := os.Lstat(c.abs(p))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if c.skip(p, fi) {
			return nil
		}
		if !fi.IsDir() {
			c.rescanned[p] = false
			return c.entry(p)
		}
	}
	ns, err := walk(c.dir, p, c.opt.Skip, &c.st)
	if err != nil {
		return err
	}
	for _, n := range ns {
		c.set(n)
	}
	return nil
}

// entry updates the node at p from the merged view.
func (c *changeScan) entry(p string) error {
	if p == "" {
		return nil
	}
	fi, err := os.Lstat(c.abs(p))
	if os.IsNotExist(err) || errorIs(err, syscall.ENOTDIR) {
		c.remove(p)
		return nil
	}
	if err != nil {
		return err
	}
	if c.skip(p, fi) {
		return nil
	}
	c.st.Examined++
	n, ok, err := scanNode(c.abs(p), p, fi)
	if err != nil {
		if os.IsNotExist(err) {
			c.remove(p)
			return nil
		}
		return err
	}
	old := c.nodes[p]
	if !ok {
		c.remove(p)
		return nil
	}
	if n.Kind == Dir && (old == nil || old.Kind != Dir) {
		return c.rescan(p) // a new directory: everything below is new
	}
	if old != nil && old.Kind == Dir && n.Kind != Dir {
		c.remove(p)
	}
	c.set(n)
	return nil
}

// listDir lists a directory of the merged view whose entries changed:
// vanished children are removed, new ones added.
func (c *changeScan) listDir(p string) error {
	if p != "" {
		if err := c.entry(p); err != nil {
			return err
		}
		if n := c.nodes[p]; n == nil || n.Kind != Dir || c.rescanned[p] {
			return nil
		}
	}
	des, err := os.ReadDir(c.abs(p))
	if os.IsNotExist(err) {
		c.remove(p)
		return nil
	}
	if err != nil {
		return err
	}
	names := make(map[string]bool, len(des))
	for _, d := range des {
		names[path.Join(p, d.Name())] = true
	}
	for _, k := range c.children(p) {
		if !names[k] {
			c.remove(k)
		}
	}
	for k := range names {
		if c.nodes[k] == nil {
			if err := c.entry(k); err != nil {
				return err
			}
		}
	}
	return nil
}

func errorIs(err error, target syscall.Errno) bool {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err == target
	}
	return false
}

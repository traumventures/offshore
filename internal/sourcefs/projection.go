// Package fusefs serves a projected directory tree over FUSE. Metadata comes
// entirely from the manifest, file contents are fetched on demand into a
// content addressed store, and local writes go to a dirty overlay that is
// later written back to the client.
package sourcefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/traumventures/offshore/internal/cas"
	"github.com/traumventures/offshore/internal/manifest"
)

// Fetcher retrieves the content of path with the given hash into the store.
type Fetcher func(ctx context.Context, rel, hash string) error

// dirty is a local modification of the projection.
type dirty struct {
	manifest.Entry
	Deleted    bool
	Upper      string // file content for kind File
	BaseHash   string // content of the base tree entry ("" if it did not exist)
	BaseExists bool
}

// Projection is one mounted root.
type Projection struct {
	Root     string // absolute mount point (same path as on the client)
	store    *cas.Store
	upperDir string
	fetch    atomic.Pointer[Fetcher]

	mu        sync.RWMutex
	tree      manifest.Tree
	children  map[string]map[string]struct{}
	dirty     map[string]*dirty
	synthetic map[string]bool // worker-local mount points, shown as empty dirs
	version   uint64
	changed   chan struct{} // closed and replaced on every version change

	rootNode *node
	server   *fuse.Server
	upperSeq atomic.Uint64
}

// New creates a projection that is not yet mounted.
func New(root string, store *cas.Store, upperDir string, workerLocal []string) (*Projection, error) {
	if err := os.MkdirAll(upperDir, 0o755); err != nil {
		return nil, err
	}
	p := &Projection{
		Root:      root,
		store:     store,
		upperDir:  upperDir,
		tree:      manifest.Tree{},
		children:  map[string]map[string]struct{}{},
		dirty:     map[string]*dirty{},
		synthetic: map[string]bool{},
		changed:   make(chan struct{}),
	}
	p.SetWorkerLocal(workerLocal)
	return p, nil
}

// SetWorkerLocal sets the directories that are shown as empty mount points.
func (p *Projection) SetWorkerLocal(paths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, wl := range paths {
		wl = strings.Trim(path.Clean(filepath.ToSlash(wl)), "/")
		for wl != "" && wl != "." {
			p.synthetic[wl] = true
			wl = parentOf(wl)
		}
	}
}

// SetFetcher sets the function used to fetch missing content.
func (p *Projection) SetFetcher(f Fetcher) { p.fetch.Store(&f) }

// Version returns the applied manifest version.
func (p *Projection) Version() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.version
}

// WaitVersion blocks until the applied version is at least v.
func (p *Projection) WaitVersion(ctx context.Context, v uint64) error {
	for {
		p.mu.RLock()
		cur, ch := p.version, p.changed
		p.mu.RUnlock()
		if cur >= v {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("waiting for manifest version %d (have %d): %w", v, cur, ctx.Err())
		}
	}
}

// Mount mounts the projection at Root.
func (p *Projection) Mount() error {
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	p.rootNode = &node{p: p}
	long := time.Hour
	opts := &fs.Options{
		EntryTimeout:    &long,
		AttrTimeout:     &long,
		NegativeTimeout: &long,
		MountOptions: fuse.MountOptions{
			AllowOther:  true,
			DirectMount: true,
			FsName:      "offshore",
			Name:        "offshore",
			MaxWrite:    1 << 20,
		},
	}
	srv, err := fs.Mount(p.Root, p.rootNode, opts)
	if err != nil {
		return err
	}
	p.server = srv
	return nil
}

// Unmount unmounts the projection.
func (p *Projection) Unmount() error {
	if p.server == nil {
		return nil
	}
	return p.server.Unmount()
}

// Apply applies a manifest delta and invalidates kernel caches for every
// affected path before returning.
func (p *Projection) Apply(d manifest.Delta) {
	p.mu.Lock()
	var affected []string
	if d.Full {
		for k := range p.tree {
			affected = append(affected, k)
		}
		p.tree = manifest.Tree{}
		p.children = map[string]map[string]struct{}{}
		for k := range p.dirty {
			affected = append(affected, k)
		}
	}
	for _, del := range d.Deletes {
		affected = append(affected, del)
		p.deleteSubtreeLocked(del, &affected)
	}
	for _, e := range d.Upserts {
		affected = append(affected, e.Path)
		if old, ok := p.tree[e.Path]; ok && old.Kind == manifest.Dir && e.Kind != manifest.Dir {
			p.deleteSubtreeLocked(e.Path, &affected)
		}
		p.tree[e.Path] = e
		p.addChildLocked(e.Path)
	}
	// Client state is authoritative: changes to a path supersede local writes.
	for _, a := range affected {
		if dt, ok := p.dirty[a]; ok {
			p.dropDirtyLocked(a, dt)
		}
	}
	if d.Full {
		for k, dt := range p.dirty {
			p.dropDirtyLocked(k, dt)
		}
	}
	if d.Version > p.version {
		p.version = d.Version
	}
	p.mu.Unlock()

	p.invalidate(affected, d.Full)

	p.mu.Lock()
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

func (p *Projection) dropDirtyLocked(rel string, dt *dirty) {
	if dt.Upper != "" {
		// Only remove the upper file if no other entry references it.
		shared := false
		for k, o := range p.dirty {
			if k != rel && o.Upper == dt.Upper {
				shared = true
			}
		}
		if !shared {
			os.Remove(dt.Upper)
		}
	}
	delete(p.dirty, rel)
}

func (p *Projection) deleteSubtreeLocked(rel string, affected *[]string) {
	if _, ok := p.tree[rel]; !ok {
		return
	}
	for name := range p.children[rel] {
		child := join(rel, name)
		*affected = append(*affected, child)
		p.deleteSubtreeLocked(child, affected)
	}
	delete(p.children, rel)
	delete(p.tree, rel)
	if set := p.children[parentOf(rel)]; set != nil {
		delete(set, path.Base(rel))
	}
}

func (p *Projection) addChildLocked(rel string) {
	parent := parentOf(rel)
	set := p.children[parent]
	if set == nil {
		set = map[string]struct{}{}
		p.children[parent] = set
	}
	set[path.Base(rel)] = struct{}{}
}

// invalidate drops kernel caches for affected paths.
func (p *Projection) invalidate(affected []string, all bool) {
	if p.rootNode == nil || p.server == nil {
		return
	}
	root := p.rootNode.EmbeddedInode()
	if all {
		var walk func(n *fs.Inode)
		walk = func(n *fs.Inode) {
			n.NotifyContent(0, 0)
			for name, ch := range n.Children() {
				n.NotifyEntry(name)
				walk(ch)
			}
		}
		walk(root)
		return
	}
	seen := map[string]bool{}
	for _, rel := range affected {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		parent := lookupInode(root, parentOf(rel))
		if parent != nil {
			if ch := parent.GetChild(path.Base(rel)); ch != nil {
				ch.NotifyContent(0, 0)
			}
			parent.NotifyEntry(path.Base(rel))
			parent.NotifyContent(0, 0)
		}
	}
}

func lookupInode(root *fs.Inode, rel string) *fs.Inode {
	n := root
	if rel == "" {
		return n
	}
	for _, part := range strings.Split(rel, "/") {
		n = n.GetChild(part)
		if n == nil {
			return nil
		}
	}
	return n
}

// view is the effective state of a path.
type view struct {
	manifest.Entry
	upper string
	dirty bool
}

func (p *Projection) statLocked(rel string) (view, bool) {
	if rel == "" {
		return view{Entry: manifest.Entry{Kind: manifest.Dir, Mode: 0o755}}, true
	}
	if dt, ok := p.dirty[rel]; ok {
		if dt.Deleted {
			return view{}, false
		}
		return view{Entry: dt.Entry, upper: dt.Upper, dirty: true}, true
	}
	if e, ok := p.tree[rel]; ok {
		return view{Entry: e}, true
	}
	if p.synthetic[rel] {
		return view{Entry: manifest.Entry{Path: rel, Kind: manifest.Dir, Mode: 0o755}}, true
	}
	return view{}, false
}

func (p *Projection) stat(rel string) (view, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.statLocked(rel)
}

func (p *Projection) listLocked(rel string) []fuse.DirEntry {
	names := map[string]bool{}
	for n := range p.children[rel] {
		names[n] = true
	}
	for k := range p.dirty {
		if parentOf(k) == rel && k != "" {
			names[path.Base(k)] = true
		}
	}
	for k := range p.synthetic {
		if parentOf(k) == rel {
			names[path.Base(k)] = true
		}
	}
	var out []fuse.DirEntry
	for n := range names {
		v, ok := p.statLocked(join(rel, n))
		if !ok {
			continue
		}
		out = append(out, fuse.DirEntry{Name: n, Mode: kindMode(v.Kind)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ensureBlob makes sure the content of a clean file is in the store,
// following manifest updates if the client content changed meanwhile.
func (p *Projection) ensureBlob(ctx context.Context, rel string) (view, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, ok := p.stat(rel)
		if !ok {
			return v, syscall.ENOENT
		}
		if v.dirty || v.Kind != manifest.File || v.Hash == "" || p.store.Has(v.Hash) {
			return v, nil
		}
		fp := p.fetch.Load()
		if fp == nil {
			return v, errors.New("no client connected")
		}
		err := (*fp)(ctx, rel, v.Hash)
		if err == nil {
			return v, nil
		}
		if time.Now().After(deadline) {
			return v, err
		}
		// The client may have changed the file; wait for a newer manifest.
		p.mu.RLock()
		ch := p.changed
		p.mu.RUnlock()
		select {
		case <-ch:
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return v, ctx.Err()
		}
	}
}

func (p *Projection) newUpper() string {
	return filepath.Join(p.upperDir, fmt.Sprintf("u%d-%d", time.Now().UnixNano(), p.upperSeq.Add(1)))
}

// copyUp turns rel into a dirty file and returns its upper path.
func (p *Projection) copyUp(ctx context.Context, rel string, truncate bool) (string, error) {
	v, err := p.ensureBlob(ctx, rel)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.statLocked(rel)
	if !ok {
		return "", syscall.ENOENT
	}
	if cur.Kind != manifest.File {
		return "", syscall.EISDIR
	}
	if cur.dirty {
		return cur.upper, nil
	}
	if cur.Hash != v.Hash {
		return "", syscall.EAGAIN
	}
	upper := p.newUpper()
	if truncate || cur.Hash == "" {
		f, err := os.OpenFile(upper, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return "", err
		}
		f.Close()
	} else if err := copyFile(p.store.Path(cur.Hash), upper); err != nil {
		return "", err
	}
	e := cur.Entry
	p.dirty[rel] = &dirty{Entry: e, Upper: upper, BaseHash: cur.Hash, BaseExists: true}
	return upper, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// setDirtyLocked records a new dirty state for rel, remembering the base.
func (p *Projection) setDirtyLocked(rel string, d *dirty) {
	if old, ok := p.dirty[rel]; ok {
		d.BaseHash, d.BaseExists = old.BaseHash, old.BaseExists
	} else if e, ok := p.tree[rel]; ok {
		d.BaseHash, d.BaseExists = e.Hash, true
	}
	d.Path = rel
	p.dirty[rel] = d
}

// removeLocked deletes rel from the effective view.
func (p *Projection) removeLocked(rel string) {
	_, inTree := p.tree[rel]
	old, wasDirty := p.dirty[rel]
	if !inTree {
		if wasDirty {
			p.dropDirtyLocked(rel, old)
		}
		return
	}
	d := &dirty{Deleted: true}
	if wasDirty && old.Upper != "" {
		// keep nothing of the removed upper
		p.dropDirtyLocked(rel, old)
	}
	p.setDirtyLocked(rel, d)
}

// DirtyChange describes one local modification for write back.
type DirtyChange struct {
	Path       string
	Deleted    bool
	Kind       manifest.Kind
	Mode       uint32
	Target     string
	Upper      string
	BaseHash   string
	BaseExists bool
}

// Dirty returns all local modifications, parents before children and
// deletions deepest first.
func (p *Projection) Dirty() []DirtyChange {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []DirtyChange
	for rel, d := range p.dirty {
		// Skip entries whose effective parent is gone.
		out = append(out, DirtyChange{Path: rel, Deleted: d.Deleted, Kind: d.Kind, Mode: d.Mode, Target: d.Target, Upper: d.Upper, BaseHash: d.BaseHash, BaseExists: d.BaseExists})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Deleted != b.Deleted {
			return !a.Deleted // creations first
		}
		if a.Deleted {
			return a.Path > b.Path
		}
		return a.Path < b.Path
	})
	return out
}

// Discard drops local modifications of the given paths (e.g. rejected by
// the client) and invalidates caches.
func (p *Projection) Discard(paths []string) {
	p.mu.Lock()
	for _, rel := range paths {
		if d, ok := p.dirty[rel]; ok {
			p.dropDirtyLocked(rel, d)
		}
	}
	p.mu.Unlock()
	p.invalidate(paths, false)
}

// Settle drops dirty entries that already equal the tree (written back and
// confirmed by the client manifest).
func (p *Projection) Settle() {
	p.mu.Lock()
	var settled []string
	for rel, d := range p.dirty {
		e, inTree := p.tree[rel]
		switch {
		case d.Deleted && !inTree:
			settled = append(settled, rel)
		case !d.Deleted && inTree && d.Kind == e.Kind && d.Kind == manifest.Dir:
			settled = append(settled, rel)
		}
	}
	for _, rel := range settled {
		p.dropDirtyLocked(rel, p.dirty[rel])
	}
	p.mu.Unlock()
}

func parentOf(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func kindMode(k manifest.Kind) uint32 {
	switch k {
	case manifest.Dir:
		return syscall.S_IFDIR
	case manifest.Symlink:
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

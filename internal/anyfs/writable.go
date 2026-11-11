package anyfs

import (
	"context"
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
	"github.com/traumventures/offshore/internal/manifest"
	"golang.org/x/sys/unix"
)

// WritableOptions configure a WritableFS mount.
type WritableOptions struct {
	// FsName shows in /proc/self/mountinfo (fuse.<FsName>).
	FsName string
	// Timeout is the kernel's entry and attribute cache time of clean
	// entries (default 1h; Apply invalidates changed paths).
	Timeout    time.Duration
	AllowOther bool
	// NoPassthrough disables kernel passthrough of clean files (it needs
	// CAP_SYS_ADMIN and kernel >= 6.9; without it reads go through the
	// daemon).
	NoPassthrough bool
}

// WritableStats counts the file operations that reached the daemon.
type WritableStats struct {
	// Opens of regular files, PassthroughOpens those handed to the kernel
	// (their reads never reach the daemon).
	Opens            int64 `json:"opens"`
	PassthroughOpens int64 `json:"passthrough_opens"`
	Reads            int64 `json:"reads"`
	Writes           int64 `json:"writes"`
	CopyUps          int64 `json:"copy_ups"`
}

// WritableFS is a read-write FUSE file system over a TreeSource whose
// tree follows a remote original (the worker's projection of a laptop
// root): metadata comes from the tree, contents are faulted in through
// the source on open, and local writes go to a dirty overlay of whole
// files in an upper directory. Unlike a kernel overlay the lower tree
// may change at any time (Apply): the original is authoritative, so a
// change of a path drops its local modification, and the kernel caches
// of every changed path are invalidated before Apply returns. Local
// modifications are written back to the original incrementally
// (Changes, then Discard or Settle) together with the base each was made
// on, so the original can detect conflicts.
type WritableFS struct {
	src   *TreeSource
	upper string
	opt   WritableOptions
	mnt   string

	mu      sync.RWMutex // guards the tree of src together with the fields below
	dirty   map[string]*dirtyNode
	mounts  map[string]bool // mount points of other file systems, shown as empty dirs
	written map[string]bool // paths written here (kept out of passthrough)
	version uint64
	changed chan struct{} // closed and replaced on every Apply

	root *wnode
	srv  *fuse.Server
	pt   bool // the kernel accepted passthrough
	seq  atomic.Uint64

	opens, ptOpens, reads, writes, copyUps atomic.Int64
}

// dirtyNode is a local modification.
type dirtyNode struct {
	Node
	Deleted    bool
	Upper      string // content of a file
	BaseDigest string // content of the tree node it replaces
	BaseExists bool   // whether the tree had a node there
}

// NewWritable returns an unmounted writable file system over src that
// keeps local modifications in upper (emptied first).
func NewWritable(src *TreeSource, upper string, opt WritableOptions) (*WritableFS, error) {
	if opt.Timeout == 0 {
		opt.Timeout = time.Hour
	}
	if opt.FsName == "" {
		opt.FsName = "offshore-writable"
	}
	os.RemoveAll(upper)
	if err := os.MkdirAll(upper, 0o755); err != nil {
		return nil, err
	}
	return &WritableFS{src: src, upper: upper, opt: opt, dirty: map[string]*dirtyNode{},
		mounts: map[string]bool{}, written: map[string]bool{}, changed: make(chan struct{})}, nil
}

// Mount mounts the file system at mnt.
func (w *WritableFS) Mount(mnt string) error {
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	w.mnt = mnt
	w.root = &wnode{w: w}
	to := w.opt.Timeout
	opts := &fs.Options{
		EntryTimeout: &to, AttrTimeout: &to, NegativeTimeout: &to,
		MountOptions: fuse.MountOptions{
			AllowOther: w.opt.AllowOther, DirectMount: true,
			FsName: w.opt.FsName, Name: w.opt.FsName, MaxWrite: 1 << 20,
		},
	}
	if w.opt.NoPassthrough {
		opts.DisabledCapabilities |= fuse.CAP_PASSTHROUGH
	}
	srv, err := fs.Mount(mnt, w.root, opts)
	if err != nil {
		return err
	}
	w.srv = srv
	w.pt = !w.opt.NoPassthrough && os.Geteuid() == 0 && srv.KernelSettings().Flags64()&fuse.CAP_PASSTHROUGH != 0
	return nil
}

func (w *WritableFS) markWritten(rel string) {
	w.mu.Lock()
	w.written[rel] = true
	w.mu.Unlock()
}

func (w *WritableFS) wasWritten(rel string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.written[rel]
}

// Passthrough reports whether clean files are served by kernel
// passthrough.
func (w *WritableFS) Passthrough() bool { return w.pt }

// Stats returns the operation counters.
func (w *WritableFS) Stats() WritableStats {
	return WritableStats{Opens: w.opens.Load(), PassthroughOpens: w.ptOpens.Load(), Reads: w.reads.Load(),
		Writes: w.writes.Load(), CopyUps: w.copyUps.Load()}
}

// Mountpoint returns the mount point.
func (w *WritableFS) Mountpoint() string { return w.mnt }

// Unmount unmounts (lazily if busy).
func (w *WritableFS) Unmount() error {
	if w.srv == nil {
		return nil
	}
	err := w.srv.Unmount()
	if err != nil {
		syscall.Unmount(w.mnt, syscall.MNT_DETACH)
	}
	return err
}

// Source returns the source.
func (w *WritableFS) Source() *TreeSource { return w.src }

// SetMountPoints adds directories that are shown as empty directories
// (with their parents) whatever the tree holds: mount points of other
// file systems (e.g. worker-local build directories).
func (w *WritableFS) SetMountPoints(paths []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range paths {
		p = strings.Trim(path.Clean(filepath.ToSlash(p)), "/")
		for p != "" && p != "." {
			w.mounts[p] = true
			p = parent(p)
		}
	}
}

// Version returns the version of the last Apply.
func (w *WritableFS) Version() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.version
}

// WaitVersion blocks until a version of at least v is applied.
func (w *WritableFS) WaitVersion(ctx context.Context, v uint64) error {
	for {
		w.mu.RLock()
		cur, ch := w.version, w.changed
		w.mu.RUnlock()
		if cur >= v {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("waiting for version %d (have %d): %w", v, cur, ctx.Err())
		}
	}
}

// Apply applies a delta of the original (see TreeSource.Apply) as
// version: local modifications of changed paths are dropped (the
// original wins) and kernel caches are invalidated before it returns.
func (w *WritableFS) Apply(layer []Node, full bool, version uint64) {
	w.mu.Lock()
	affected := w.src.Apply(layer, full)
	for _, a := range affected {
		if d, ok := w.dirty[a]; ok {
			w.dropLocked(a, d)
		}
	}
	if full {
		for k, d := range w.dirty {
			affected = append(affected, k)
			w.dropLocked(k, d)
		}
	}
	if version > w.version {
		w.version = version
	}
	w.mu.Unlock()

	w.invalidate(affected, full)

	w.mu.Lock()
	close(w.changed)
	w.changed = make(chan struct{})
	w.mu.Unlock()
}

func (w *WritableFS) dropLocked(rel string, d *dirtyNode) {
	if d.Upper != "" {
		shared := false
		for k, o := range w.dirty {
			if k != rel && o.Upper == d.Upper {
				shared = true
			}
		}
		if !shared {
			os.Remove(d.Upper)
		}
	}
	delete(w.dirty, rel)
}

// invalidate drops kernel caches of the given paths (every cached path
// with all).
func (w *WritableFS) invalidate(paths []string, all bool) {
	if w.root == nil || w.srv == nil {
		return
	}
	root := w.root.EmbeddedInode()
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
	for _, rel := range paths {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		if p := inodeAt(root, parent(rel)); p != nil {
			if ch := p.GetChild(path.Base(rel)); ch != nil {
				ch.NotifyContent(0, 0)
			}
			p.NotifyEntry(path.Base(rel))
			p.NotifyContent(0, 0)
		}
	}
}

func inodeAt(root *fs.Inode, rel string) *fs.Inode {
	n := root
	if rel == "" {
		return n
	}
	for _, part := range strings.Split(rel, "/") {
		if n = n.GetChild(part); n == nil {
			return nil
		}
	}
	return n
}

// view is the effective state of a path.
type view struct {
	Node
	upper string
	dirty bool
}

func (w *WritableFS) statLocked(rel string) (view, bool) {
	if rel == "" {
		return view{Node: rootNode}, true
	}
	if d, ok := w.dirty[rel]; ok {
		if d.Deleted {
			return view{}, false
		}
		return view{Node: d.Node, upper: d.Upper, dirty: true}, true
	}
	if n, ok := w.src.Tree().Get(rel); ok {
		return view{Node: *n}, true
	}
	if w.mounts[rel] {
		return view{Node: Node{Path: rel, Kind: Dir, Mode: 0o755}}, true
	}
	return view{}, false
}

func (w *WritableFS) stat(rel string) (view, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.statLocked(rel)
}

func (w *WritableFS) listLocked(rel string) []fuse.DirEntry {
	names := map[string]bool{}
	for _, n := range w.src.Tree().Children(rel) {
		names[path.Base(n.Path)] = true
	}
	for k := range w.dirty {
		if parent(k) == rel {
			names[path.Base(k)] = true
		}
	}
	for k := range w.mounts {
		if parent(k) == rel {
			names[path.Base(k)] = true
		}
	}
	out := make([]fuse.DirEntry, 0, len(names))
	for n := range names {
		if v, ok := w.statLocked(join(rel, n)); ok {
			out = append(out, fuse.DirEntry{Name: n, Mode: kindBits(uint8(v.Kind))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ensure makes the content of rel local and returns its path. While the
// fault of a clean file fails it follows changes of the original (the
// file changed there, a newer tree is on its way).
func (w *WritableFS) ensure(ctx context.Context, rel string) (view, string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, ok := w.stat(rel)
		if !ok {
			return v, "", syscall.ENOENT
		}
		if v.dirty || v.Kind != File {
			return v, v.upper, nil
		}
		if v.Digest == "" {
			return v, os.DevNull, nil
		}
		p, err := w.src.Fetch(ctx, &v.Node)
		if err == nil {
			return v, p, nil
		}
		if time.Now().After(deadline) {
			return v, "", err
		}
		w.mu.RLock()
		ch := w.changed
		w.mu.RUnlock()
		select {
		case <-ch:
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return v, "", ctx.Err()
		}
	}
}

func (w *WritableFS) newUpper() string {
	return filepath.Join(w.upper, fmt.Sprintf("u%d-%d", time.Now().UnixNano(), w.seq.Add(1)))
}

// openClean opens the materialized content of a clean file and marks it
// open in the file cache (reclaim skips it until the returned release).
// For other views it returns no file.
func (w *WritableFS) openClean(ctx context.Context, rel string) (view, *os.File, func(), error) {
	for attempt := 0; ; attempt++ {
		v, _, err := w.ensure(ctx, rel)
		if err != nil || v.dirty || v.Kind != File || v.Digest == "" {
			return v, nil, nil, err
		}
		files := w.src.Files()
		if f, ok := files.Open(&v.Node); ok {
			nd := v.Node
			return v, f, func() { files.Release(&nd) }, nil
		}
		if attempt == 3 { // reclaimed right after every fault
			return v, nil, nil, syscall.EIO
		}
	}
}

// copyUp turns rel into a dirty file and returns its upper path.
func (w *WritableFS) copyUp(ctx context.Context, rel string, truncate bool) (string, error) {
	v, in, release, err := w.openClean(ctx, rel)
	if err != nil {
		return "", err
	}
	if in != nil {
		defer func() {
			in.Close()
			release()
		}()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cur, ok := w.statLocked(rel)
	switch {
	case !ok:
		return "", syscall.ENOENT
	case cur.Kind != File:
		return "", syscall.EISDIR
	case cur.dirty:
		return cur.upper, nil
	case cur.Digest != v.Digest:
		return "", syscall.EAGAIN
	}
	upper := w.newUpper()
	if truncate {
		in = nil
	}
	if err := copyHandle(in, upper); err != nil {
		os.Remove(upper)
		return "", err
	}
	w.copyUps.Add(1)
	w.dirty[rel] = &dirtyNode{Node: cur.Node, Upper: upper, BaseDigest: cur.Digest, BaseExists: true}
	return upper, nil
}

// copyHandle writes the content of in (nil: nothing) to a new file dst,
// with a reflink where the file system supports it.
func copyHandle(in *os.File, dst string) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if in != nil && unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) != nil {
		if _, err := io.Copy(out, io.NewSectionReader(in, 0, 1<<62)); err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}

// setDirtyLocked records a new state of rel, keeping the base of an
// earlier modification.
func (w *WritableFS) setDirtyLocked(rel string, d *dirtyNode) {
	if old, ok := w.dirty[rel]; ok {
		d.BaseDigest, d.BaseExists = old.BaseDigest, old.BaseExists
	} else if n, ok := w.src.Tree().Get(rel); ok {
		d.BaseDigest, d.BaseExists = n.Digest, true
	}
	d.Path = rel
	w.dirty[rel] = d
}

// removeLocked deletes rel from the effective view. (A modification is
// dropped whenever its tree node changes, so the base of a deletion is
// the current tree node.)
func (w *WritableFS) removeLocked(rel string) {
	if old, ok := w.dirty[rel]; ok {
		w.dropLocked(rel, old)
	}
	if _, ok := w.src.Tree().Get(rel); ok {
		w.setDirtyLocked(rel, &dirtyNode{Deleted: true})
	}
}

// Change is one local modification to write back.
type Change struct {
	Path    string
	Deleted bool
	Kind    manifest.Kind
	Mode    uint32
	Target  string
	// Upper holds the new content of a file.
	Upper string
	// BaseDigest is the content digest of the tree node the change
	// replaces ("" if it was no regular file) and BaseExists whether
	// there was one: the original rejects changes whose base it no
	// longer has (a conflict).
	BaseDigest string
	BaseExists bool
}

// Changes returns the local modifications: creations parents first,
// then deletions deepest first.
func (w *WritableFS) Changes() []Change {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]Change, 0, len(w.dirty))
	for rel, d := range w.dirty {
		out = append(out, Change{Path: rel, Deleted: d.Deleted, Kind: d.Kind, Mode: d.Mode, Target: d.Target,
			Upper: d.Upper, BaseDigest: d.BaseDigest, BaseExists: d.BaseExists})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Deleted != b.Deleted {
			return !a.Deleted
		}
		if a.Deleted {
			return pathLess(b.Path, a.Path)
		}
		return pathLess(a.Path, b.Path)
	})
	return out
}

// Discard drops the local modifications of paths (rejected by the
// original) and invalidates their caches.
func (w *WritableFS) Discard(paths []string) {
	w.mu.Lock()
	for _, rel := range paths {
		if d, ok := w.dirty[rel]; ok {
			w.dropLocked(rel, d)
		}
	}
	w.mu.Unlock()
	w.invalidate(paths, false)
}

// Settle drops modifications the tree already shows (written back and
// confirmed by the original) that Apply kept because the path did not
// change: deletions of absent paths and directories that exist.
func (w *WritableFS) Settle() {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.src.Tree()
	for rel, d := range w.dirty {
		n, inTree := t.Get(rel)
		if (d.Deleted && !inTree) || (!d.Deleted && inTree && d.Kind == Dir && n.Kind == Dir) {
			w.dropLocked(rel, d)
		}
	}
}

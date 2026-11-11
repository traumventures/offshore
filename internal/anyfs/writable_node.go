package anyfs

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// wnode is a node of a WritableFS (identified by its path).
//
// Passthrough. With kernel passthrough the opens of a clean file get its
// materialized content as backing file, so reads (and mmaps) never reach
// the daemon. go-fuse registers one backing file per inode and reuses it
// for every passthrough open while one is open, so an inode must never
// change content under open passthrough handles: the inode of a clean
// file is keyed by its path and content digest (a change on the laptop
// gives the path a new inode after the kernel looks it up again), and
// the kernel refuses to mix passthrough and cached opens of one inode,
// so each inode counts both and falls back to direct I/O (neutral to
// both modes) for the minority: e.g. the copy-up on the first write of a
// file that is open for reading keeps its upper file out of the kernel
// while the clean handles stay open. Dirty files never use passthrough
// (their writes and size changes go through the daemon).
type wnode struct {
	fs.Inode
	w *WritableFS

	mu         sync.Mutex
	ptOpen     int // open passthrough handles
	cachedOpen int // open handles using the page cache
}

var (
	_ fs.NodeLookuper   = (*wnode)(nil)
	_ fs.NodeGetattrer  = (*wnode)(nil)
	_ fs.NodeReaddirer  = (*wnode)(nil)
	_ fs.NodeOpener     = (*wnode)(nil)
	_ fs.NodeReadlinker = (*wnode)(nil)
	_ fs.NodeCreater    = (*wnode)(nil)
	_ fs.NodeMkdirer    = (*wnode)(nil)
	_ fs.NodeUnlinker   = (*wnode)(nil)
	_ fs.NodeRmdirer    = (*wnode)(nil)
	_ fs.NodeRenamer    = (*wnode)(nil)
	_ fs.NodeSetattrer  = (*wnode)(nil)
	_ fs.NodeSymlinker  = (*wnode)(nil)
	_ fs.NodeStatfser   = (*wnode)(nil)
	_ fs.NodeAccesser   = (*wnode)(nil)
)

func (n *wnode) rel() string { return n.Path(n.w.root.EmbeddedInode()) }

// errnoOf maps an error to an errno (EIO for errors of other origin,
// e.g. a failed fetch).
func errnoOf(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		return e
	}
	return syscall.EIO
}

// fill sets the attributes of a view.
func (w *WritableFS) fill(v view, out *fuse.Attr) syscall.Errno {
	out.Mode = kindBits(uint8(v.Kind)) | (v.Mode & 0o7777)
	out.Nlink = 1
	if v.Kind == Dir {
		out.Nlink = 2
	}
	mt := time.Unix(0, v.MtimeNs)
	if v.MtimeNs == 0 {
		mt = time.Unix(1, 0)
	}
	out.Size = uint64(v.Size)
	if v.dirty && v.Kind == File && v.upper != "" {
		fi, err := os.Stat(v.upper)
		if err != nil {
			return errnoOf(err)
		}
		out.Size = uint64(fi.Size())
		mt = fi.ModTime()
	}
	if v.Kind == Symlink {
		out.Size = uint64(len(v.Target))
	}
	out.SetTimes(&mt, &mt, &mt)
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
	return 0
}

func (n *wnode) child(ctx context.Context, name string, v view, out *fuse.EntryOut) *fs.Inode {
	n.w.fill(v, &out.Attr)
	if v.dirty {
		out.SetAttrTimeout(0)
	}
	attr := fs.StableAttr{Mode: kindBits(uint8(v.Kind))}
	if v.Kind == File && !v.dirty {
		attr.Ino = cleanIno(v.Path, v.Digest)
	}
	if ch := n.GetChild(name); ch != nil && ch.StableAttr().Mode == attr.Mode && (attr.Ino == 0 || ch.StableAttr().Ino == attr.Ino) {
		return ch
	}
	return n.NewInode(ctx, &wnode{w: n.w}, attr)
}

// cleanIno is the inode number of a clean file: one per path and
// content (below go-fuse's automatic numbers, which start at 1<<63).
func cleanIno(rel, digest string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(rel))
	h.Write([]byte{0})
	h.Write([]byte(digest))
	return h.Sum64()&^(1<<63) | 2
}

func (n *wnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	v, ok := n.w.stat(join(n.rel(), name))
	if !ok {
		return nil, syscall.ENOENT
	}
	return n.child(ctx, name, v, out), 0
}

func (n *wnode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	v, ok := n.w.stat(n.rel())
	if !ok {
		return syscall.ENOENT
	}
	if v.dirty {
		out.SetTimeout(0)
	}
	return n.w.fill(v, &out.Attr)
}

func (n *wnode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	rel := n.rel()
	n.w.mu.RLock()
	defer n.w.mu.RUnlock()
	if v, ok := n.w.statLocked(rel); !ok || v.Kind != Dir {
		return nil, syscall.ENOTDIR
	}
	return fs.NewListDirStream(n.w.listLocked(rel)), 0
}

func (n *wnode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	v, ok := n.w.stat(n.rel())
	if !ok {
		return nil, syscall.ENOENT
	}
	if v.Kind != Symlink {
		return nil, syscall.EINVAL
	}
	return []byte(v.Target), 0
}

func (n *wnode) Access(ctx context.Context, mask uint32) syscall.Errno {
	if _, ok := n.w.stat(n.rel()); !ok {
		return syscall.ENOENT
	}
	return 0
}

func (n *wnode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(n.w.upper, &st); err != nil {
		return errnoOf(err)
	}
	out.FromStatfsT(&st)
	return 0
}

func (n *wnode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	rel := n.rel()
	w := n.w
	w.opens.Add(1)
	if flags&writeFlags != 0 {
		if errno := n.lockNoPassthrough(ctx); errno != 0 {
			return nil, 0, errno
		}
		defer n.mu.Unlock()
		trunc := flags&syscall.O_TRUNC != 0
		upper, err := w.copyUp(ctx, rel, trunc)
		if err != nil {
			return nil, 0, errnoOf(err)
		}
		if trunc {
			if err := os.Truncate(upper, 0); err != nil {
				return nil, 0, errnoOf(err)
			}
		}
		return n.openUpperLocked(upper, flags, true)
	}
	v, f, release, err := w.openClean(ctx, rel)
	if err != nil {
		return nil, 0, errnoOf(err)
	}
	switch {
	case v.Kind == Dir:
		return nil, 0, syscall.EISDIR
	case v.dirty:
		if errno := n.lockNoPassthrough(ctx); errno != 0 {
			return nil, 0, errno
		}
		defer n.mu.Unlock()
		return n.openUpperLocked(v.upper, flags, false)
	case f == nil: // empty file without digest
		null, err := os.Open(os.DevNull)
		if err != nil {
			return nil, 0, errnoOf(err)
		}
		return &whandle{f: null, w: w}, fuse.FOPEN_DIRECT_IO, 0
	}
	h := &whandle{f: f, w: w, release: release}
	if !w.pt {
		return h, fuse.FOPEN_KEEP_CACHE, 0
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cachedOpen > 0 || w.wasWritten(rel) {
		// Paths written here before stay out of passthrough: their next
		// write must not wait for readers.
		n.cachedOpen++
		h.n = n
		return h, fuse.FOPEN_KEEP_CACHE, 0
	}
	n.ptOpen++
	h.n, h.pt = n, true
	w.ptOpens.Add(1)
	return &whandlePT{h}, 0, 0
}

// lockNoPassthrough locks n once no passthrough handle of it is open.
// The kernel requires every open of an inode in passthrough mode to use
// its backing file, which is the clean content; a write (or a read of
// the dirty content) therefore waits briefly for the clean readers to
// close and fails with ETXTBSY if they do not.
func (n *wnode) lockNoPassthrough(ctx context.Context) syscall.Errno {
	deadline := time.Now().Add(passthroughWait)
	n.mu.Lock()
	for n.ptOpen > 0 {
		n.mu.Unlock()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return syscall.ETXTBSY
		}
		time.Sleep(5 * time.Millisecond)
		n.mu.Lock()
	}
	return 0
}

// passthroughWait bounds the wait of lockNoPassthrough.
var passthroughWait = 2 * time.Second

// openUpperLocked opens the upper file of a dirty file through the page
// cache (n.mu held, no passthrough handle open).
func (n *wnode) openUpperLocked(upper string, flags uint32, writable bool) (fs.FileHandle, uint32, syscall.Errno) {
	f, err := os.OpenFile(upper, int(flags)&^(syscall.O_CREAT|syscall.O_EXCL|syscall.O_APPEND|syscall.O_TRUNC), 0)
	if err != nil {
		return nil, 0, errnoOf(err)
	}
	h := &whandle{f: f, w: n.w, writable: writable}
	if writable {
		n.w.markWritten(n.rel())
	}
	if n.w.pt {
		n.cachedOpen++
		h.n = n
	}
	return h, 0, 0
}

func (n *wnode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := join(n.rel(), name)
	w := n.w
	w.mu.Lock()
	if dir, ok := w.statLocked(n.rel()); !ok || dir.Kind != Dir {
		w.mu.Unlock()
		return nil, nil, 0, syscall.ENOENT
	}
	if v, ok := w.statLocked(rel); ok {
		if flags&syscall.O_EXCL != 0 {
			w.mu.Unlock()
			return nil, nil, 0, syscall.EEXIST
		}
		if v.Kind == Dir {
			w.mu.Unlock()
			return nil, nil, 0, syscall.EISDIR
		}
	}
	upper := w.newUpper()
	f, err := os.OpenFile(upper, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		w.mu.Unlock()
		return nil, nil, 0, errnoOf(err)
	}
	if old, ok := w.dirty[rel]; ok && old.Upper != "" {
		os.Remove(old.Upper)
	}
	w.setDirtyLocked(rel, &dirtyNode{Node: Node{Kind: File, Mode: mode & 0o7777}, Upper: upper})
	v, _ := w.statLocked(rel)
	w.mu.Unlock()
	ch := n.child(ctx, name, v, out)
	h := &whandle{f: f, w: w, writable: true}
	w.markWritten(rel)
	if w.pt {
		// A new path: its inode has no passthrough handles (the kernel
		// only creates names it does not know).
		cn := ch.Operations().(*wnode)
		cn.mu.Lock()
		cn.cachedOpen++
		h.n = cn
		cn.mu.Unlock()
	}
	return ch, h, 0, 0
}

// create records a new directory or symlink.
func (n *wnode) create(ctx context.Context, name string, nd Node, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := join(n.rel(), name)
	w := n.w
	w.mu.Lock()
	if _, ok := w.statLocked(rel); ok {
		w.mu.Unlock()
		return nil, syscall.EEXIST
	}
	w.setDirtyLocked(rel, &dirtyNode{Node: nd})
	v, _ := w.statLocked(rel)
	w.mu.Unlock()
	return n.child(ctx, name, v, out), 0
}

func (n *wnode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.create(ctx, name, Node{Kind: Dir, Mode: mode & 0o7777}, out)
}

func (n *wnode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.create(ctx, name, Node{Kind: Symlink, Mode: 0o777, Target: target, Size: int64(len(target))}, out)
}

func (n *wnode) Unlink(ctx context.Context, name string) syscall.Errno {
	rel := join(n.rel(), name)
	w := n.w
	w.mu.Lock()
	defer w.mu.Unlock()
	v, ok := w.statLocked(rel)
	if !ok {
		return syscall.ENOENT
	}
	if v.Kind == Dir {
		return syscall.EISDIR
	}
	w.removeLocked(rel)
	return 0
}

func (n *wnode) Rmdir(ctx context.Context, name string) syscall.Errno {
	rel := join(n.rel(), name)
	w := n.w
	w.mu.Lock()
	defer w.mu.Unlock()
	v, ok := w.statLocked(rel)
	if !ok {
		return syscall.ENOENT
	}
	if v.Kind != Dir {
		return syscall.ENOTDIR
	}
	if w.mounts[rel] {
		return syscall.EBUSY
	}
	if len(w.listLocked(rel)) > 0 {
		return syscall.ENOTEMPTY
	}
	w.removeLocked(rel)
	return 0
}

func (n *wnode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	src := join(n.rel(), name)
	np, ok := newParent.(*wnode)
	if !ok {
		return syscall.EXDEV
	}
	dst := join(np.rel(), newName)
	w := n.w
	v, ok := w.stat(src)
	if !ok {
		return syscall.ENOENT
	}
	if v.Kind == Dir {
		return syscall.EXDEV // callers fall back to copy and delete
	}
	if flags&^uint32(1) != 0 { // only RENAME_NOREPLACE
		return syscall.EINVAL
	}
	var upper string
	if v.Kind == File {
		var err error
		if upper, err = w.copyUp(ctx, src, false); err != nil {
			return errnoOf(err)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cur, ok := w.statLocked(src)
	if !ok {
		return syscall.ENOENT
	}
	if dv, exists := w.statLocked(dst); exists {
		if flags&1 != 0 {
			return syscall.EEXIST
		}
		if dv.Kind == Dir {
			return syscall.EISDIR
		}
		if old, ok := w.dirty[dst]; ok && old.Upper != "" && old.Upper != upper {
			os.Remove(old.Upper)
		}
	}
	nd := cur.Node
	nd.Path = dst
	if sd, ok := w.dirty[src]; ok {
		sd.Upper = "" // the upper moves to dst
	}
	w.removeLocked(src)
	w.setDirtyLocked(dst, &dirtyNode{Node: nd, Upper: upper})
	return 0
}

func (n *wnode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	rel := n.rel()
	w := n.w
	v, ok := w.stat(rel)
	if !ok {
		return syscall.ENOENT
	}
	size, hasSize := in.GetSize()
	mode, hasMode := in.GetMode()
	mtime, hasMtime := in.GetMTime()
	if v.Kind == File && (hasSize || hasMode || (hasMtime && v.dirty)) {
		upper, err := w.copyUp(ctx, rel, hasSize && size == 0)
		if err != nil {
			return errnoOf(err)
		}
		if hasSize {
			if err := os.Truncate(upper, int64(size)); err != nil {
				return errnoOf(err)
			}
		}
		if hasMtime {
			os.Chtimes(upper, mtime, mtime)
		}
	}
	if hasMode && v.Kind != Symlink {
		w.mu.Lock()
		if d, ok := w.dirty[rel]; ok {
			d.Mode = mode & 0o7777
		} else if v.Kind == Dir {
			nd := v.Node
			nd.Mode = mode & 0o7777
			w.setDirtyLocked(rel, &dirtyNode{Node: nd})
		}
		w.mu.Unlock()
	}
	nv, ok := w.stat(rel)
	if !ok {
		return syscall.ENOENT
	}
	out.SetTimeout(0)
	return w.fill(nv, &out.Attr)
}

// whandle is an open file: an upper file or the materialized content of
// a clean one.
type whandle struct {
	f        *os.File
	w        *WritableFS
	writable bool
	release  func()
	n        *wnode // counts the handle by mode (with passthrough)
	pt       bool
}

// whandlePT offers its clean file for kernel passthrough.
type whandlePT struct{ *whandle }

func (h *whandlePT) PassthroughFd() (int, bool) { return int(h.f.Fd()), true }

var (
	_ fs.FileReader   = (*whandle)(nil)
	_ fs.FileWriter   = (*whandle)(nil)
	_ fs.FileReleaser = (*whandle)(nil)
	_ fs.FileFsyncer  = (*whandle)(nil)
	_ fs.FileFlusher  = (*whandle)(nil)
	_ fs.FileLseeker  = (*whandle)(nil)
)

func (h *whandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.w.reads.Add(1)
	k, err := h.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, errnoOf(err)
	}
	return fuse.ReadResultData(dest[:k]), 0
}

func (h *whandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if !h.writable {
		return 0, syscall.EBADF
	}
	h.w.writes.Add(1)
	k, err := h.f.WriteAt(data, off)
	return uint32(k), errnoOf(err)
}

func (h *whandle) Release(ctx context.Context) syscall.Errno {
	if h.n != nil {
		h.n.mu.Lock()
		if h.pt {
			h.n.ptOpen--
		} else {
			h.n.cachedOpen--
		}
		h.n.mu.Unlock()
	}
	if h.release != nil {
		h.release()
	}
	return errnoOf(h.f.Close())
}

func (h *whandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	if !h.writable {
		return 0
	}
	return errnoOf(h.f.Sync())
}

func (h *whandle) Flush(ctx context.Context) syscall.Errno { return 0 }

func (h *whandle) Lseek(ctx context.Context, off uint64, whence uint32) (uint64, syscall.Errno) {
	k, err := h.f.Seek(int64(off), int(whence))
	return uint64(k), errnoOf(err)
}

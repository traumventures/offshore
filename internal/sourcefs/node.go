package sourcefs

import (
	"context"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/traumventures/offshore/internal/manifest"
)

type node struct {
	fs.Inode
	p *Projection
}

var (
	_ fs.NodeLookuper   = (*node)(nil)
	_ fs.NodeGetattrer  = (*node)(nil)
	_ fs.NodeReaddirer  = (*node)(nil)
	_ fs.NodeOpener     = (*node)(nil)
	_ fs.NodeReadlinker = (*node)(nil)
	_ fs.NodeCreater    = (*node)(nil)
	_ fs.NodeMkdirer    = (*node)(nil)
	_ fs.NodeUnlinker   = (*node)(nil)
	_ fs.NodeRmdirer    = (*node)(nil)
	_ fs.NodeRenamer    = (*node)(nil)
	_ fs.NodeSetattrer  = (*node)(nil)
	_ fs.NodeSymlinker  = (*node)(nil)
	_ fs.NodeStatfser   = (*node)(nil)
	_ fs.NodeAccesser   = (*node)(nil)
)

func (n *node) rel() string {
	return n.Path(n.p.rootNode.EmbeddedInode())
}

func errno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if e, ok := err.(syscall.Errno); ok {
		return e
	}
	if pe, ok := err.(*os.PathError); ok {
		if e, ok := pe.Err.(syscall.Errno); ok {
			return e
		}
	}
	return syscall.EIO
}

// fill sets attributes for a view.
func (p *Projection) fill(v view, out *fuse.Attr) syscall.Errno {
	out.Mode = kindMode(v.Kind) | (v.Mode & 0o7777)
	out.Nlink = 1
	if v.Kind == manifest.Dir {
		out.Nlink = 2
	}
	mt := time.Unix(0, v.MtimeNs)
	if v.MtimeNs == 0 {
		mt = time.Unix(1, 0)
	}
	out.Size = uint64(v.Size)
	if v.dirty && v.Kind == manifest.File && v.upper != "" {
		fi, err := os.Stat(v.upper)
		if err != nil {
			return errno(err)
		}
		out.Size = uint64(fi.Size())
		mt = fi.ModTime()
	}
	if v.Kind == manifest.Symlink {
		out.Size = uint64(len(v.Target))
	}
	out.SetTimes(&mt, &mt, &mt)
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
	return 0
}

func (n *node) child(ctx context.Context, name string, v view, out *fuse.EntryOut) *fs.Inode {
	n.p.fill(v, &out.Attr)
	if v.dirty {
		out.SetAttrTimeout(0)
	}
	if ch := n.GetChild(name); ch != nil && ch.StableAttr().Mode == kindMode(v.Kind) {
		return ch
	}
	return n.NewInode(ctx, &node{p: n.p}, fs.StableAttr{Mode: kindMode(v.Kind)})
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := join(n.rel(), name)
	v, ok := n.p.stat(rel)
	if !ok {
		return nil, syscall.ENOENT
	}
	return n.child(ctx, name, v, out), 0
}

func (n *node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	v, ok := n.p.stat(n.rel())
	if !ok {
		return syscall.ENOENT
	}
	if v.dirty {
		out.SetTimeout(0)
	}
	return n.p.fill(v, &out.Attr)
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	rel := n.rel()
	n.p.mu.RLock()
	defer n.p.mu.RUnlock()
	if v, ok := n.p.statLocked(rel); !ok || v.Kind != manifest.Dir {
		return nil, syscall.ENOTDIR
	}
	return fs.NewListDirStream(n.p.listLocked(rel)), 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	v, ok := n.p.stat(n.rel())
	if !ok {
		return nil, syscall.ENOENT
	}
	if v.Kind != manifest.Symlink {
		return nil, syscall.EINVAL
	}
	return []byte(v.Target), 0
}

func (n *node) Access(ctx context.Context, mask uint32) syscall.Errno {
	if _, ok := n.p.stat(n.rel()); !ok {
		return syscall.ENOENT
	}
	return 0
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(n.p.upperDir, &st); err != nil {
		return errno(err)
	}
	out.FromStatfsT(&st)
	return 0
}

const writeFlags = syscall.O_WRONLY | syscall.O_RDWR | syscall.O_TRUNC | syscall.O_APPEND

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	rel := n.rel()
	if flags&writeFlags != 0 {
		upper, err := n.p.copyUp(ctx, rel, flags&syscall.O_TRUNC != 0)
		if err != nil {
			return nil, 0, errno(err)
		}
		if flags&syscall.O_TRUNC != 0 {
			if err := os.Truncate(upper, 0); err != nil {
				return nil, 0, errno(err)
			}
		}
		f, err := os.OpenFile(upper, int(flags)&^(syscall.O_CREAT|syscall.O_EXCL|syscall.O_APPEND|syscall.O_TRUNC), 0)
		if err != nil {
			return nil, 0, errno(err)
		}
		return &handle{f: f, p: n.p, rel: rel, writable: true}, 0, 0
	}
	v, err := n.p.ensureBlob(ctx, rel)
	if err != nil {
		return nil, 0, errno(err)
	}
	if v.Kind == manifest.Dir {
		return nil, 0, syscall.EISDIR
	}
	src := v.upper
	if !v.dirty {
		if v.Hash == "" {
			src = "/dev/null"
		} else {
			src = n.p.store.Path(v.Hash)
		}
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, 0, errno(err)
	}
	var fl uint32
	if !v.dirty {
		fl = fuse.FOPEN_KEEP_CACHE
	}
	return &handle{f: f, p: n.p, rel: rel}, fl, 0
}

func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := join(n.rel(), name)
	p := n.p
	p.mu.Lock()
	if parent, ok := p.statLocked(n.rel()); !ok || parent.Kind != manifest.Dir {
		p.mu.Unlock()
		return nil, nil, 0, syscall.ENOENT
	}
	if v, ok := p.statLocked(rel); ok {
		if flags&syscall.O_EXCL != 0 {
			p.mu.Unlock()
			return nil, nil, 0, syscall.EEXIST
		}
		if v.Kind == manifest.Dir {
			p.mu.Unlock()
			return nil, nil, 0, syscall.EISDIR
		}
	}
	upper := p.newUpper()
	f, err := os.OpenFile(upper, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		p.mu.Unlock()
		return nil, nil, 0, errno(err)
	}
	if old, ok := p.dirty[rel]; ok && old.Upper != "" {
		os.Remove(old.Upper)
	}
	d := &dirty{Entry: manifest.Entry{Kind: manifest.File, Mode: mode & 0o7777}, Upper: upper}
	p.setDirtyLocked(rel, d)
	v, _ := p.statLocked(rel)
	p.mu.Unlock()
	ch := n.child(ctx, name, v, out)
	return ch, &handle{f: f, p: p, rel: rel, writable: true}, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := join(n.rel(), name)
	p := n.p
	p.mu.Lock()
	if _, ok := p.statLocked(rel); ok {
		p.mu.Unlock()
		return nil, syscall.EEXIST
	}
	p.setDirtyLocked(rel, &dirty{Entry: manifest.Entry{Kind: manifest.Dir, Mode: mode & 0o7777}})
	v, _ := p.statLocked(rel)
	p.mu.Unlock()
	return n.child(ctx, name, v, out), 0
}

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := join(n.rel(), name)
	p := n.p
	p.mu.Lock()
	if _, ok := p.statLocked(rel); ok {
		p.mu.Unlock()
		return nil, syscall.EEXIST
	}
	p.setDirtyLocked(rel, &dirty{Entry: manifest.Entry{Kind: manifest.Symlink, Mode: 0o777, Target: target, Size: int64(len(target))}})
	v, _ := p.statLocked(rel)
	p.mu.Unlock()
	return n.child(ctx, name, v, out), 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	rel := join(n.rel(), name)
	p := n.p
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.statLocked(rel)
	if !ok {
		return syscall.ENOENT
	}
	if v.Kind == manifest.Dir {
		return syscall.EISDIR
	}
	p.removeLocked(rel)
	return 0
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	rel := join(n.rel(), name)
	p := n.p
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.statLocked(rel)
	if !ok {
		return syscall.ENOENT
	}
	if v.Kind != manifest.Dir {
		return syscall.ENOTDIR
	}
	if p.synthetic[rel] {
		return syscall.EBUSY
	}
	if len(p.listLocked(rel)) > 0 {
		return syscall.ENOTEMPTY
	}
	p.removeLocked(rel)
	return 0
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	src := join(n.rel(), name)
	np, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	dst := join(np.rel(), newName)
	p := n.p
	v, ok := p.stat(src)
	if !ok {
		return syscall.ENOENT
	}
	if v.Kind == manifest.Dir {
		return syscall.EXDEV // callers fall back to copy + delete
	}
	if flags&^uint32(1) != 0 { // only RENAME_NOREPLACE is supported
		return syscall.EINVAL
	}
	var upper string
	if v.Kind == manifest.File {
		var err error
		if upper, err = p.copyUp(ctx, src, false); err != nil {
			return errno(err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.statLocked(src)
	if !ok {
		return syscall.ENOENT
	}
	if dv, exists := p.statLocked(dst); exists {
		if flags&1 != 0 {
			return syscall.EEXIST
		}
		if dv.Kind == manifest.Dir {
			return syscall.EISDIR
		}
		if old, ok := p.dirty[dst]; ok && old.Upper != "" && old.Upper != upper {
			os.Remove(old.Upper)
		}
	}
	e := cur.Entry
	e.Path = dst
	d := &dirty{Entry: e, Upper: upper}
	// Detach the upper from the source before removing it.
	if sd, ok := p.dirty[src]; ok {
		sd.Upper = ""
	}
	p.removeLocked(src)
	p.setDirtyLocked(dst, d)
	return 0
}

func (n *node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	rel := n.rel()
	p := n.p
	v, ok := p.stat(rel)
	if !ok {
		return syscall.ENOENT
	}
	size, hasSize := in.GetSize()
	mode, hasMode := in.GetMode()
	mtime, hasMtime := in.GetMTime()
	if v.Kind == manifest.File && (hasSize || hasMode || (hasMtime && v.dirty)) {
		upper, err := p.copyUp(ctx, rel, hasSize && size == 0)
		if err != nil {
			return errno(err)
		}
		if hasSize {
			if err := os.Truncate(upper, int64(size)); err != nil {
				return errno(err)
			}
		}
		if hasMtime {
			os.Chtimes(upper, mtime, mtime)
		}
	}
	if hasMode && v.Kind != manifest.Symlink {
		p.mu.Lock()
		if d, ok := p.dirty[rel]; ok {
			d.Mode = mode & 0o7777
		} else if v.Kind == manifest.Dir {
			e := v.Entry
			e.Mode = mode & 0o7777
			p.setDirtyLocked(rel, &dirty{Entry: e})
		}
		p.mu.Unlock()
	}
	nv, ok := p.stat(rel)
	if !ok {
		return syscall.ENOENT
	}
	out.SetTimeout(0)
	return p.fill(nv, &out.Attr)
}

// handle is an open file.
type handle struct {
	f        *os.File
	p        *Projection
	rel      string
	writable bool
}

var (
	_ fs.FileReader   = (*handle)(nil)
	_ fs.FileWriter   = (*handle)(nil)
	_ fs.FileReleaser = (*handle)(nil)
	_ fs.FileFsyncer  = (*handle)(nil)
	_ fs.FileFlusher  = (*handle)(nil)
	_ fs.FileLseeker  = (*handle)(nil)
)

func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, errno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *handle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if !h.writable {
		return 0, syscall.EBADF
	}
	n, err := h.f.WriteAt(data, off)
	return uint32(n), errno(err)
}

func (h *handle) Release(ctx context.Context) syscall.Errno {
	return errno(h.f.Close())
}

func (h *handle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	if !h.writable {
		return 0
	}
	return errno(h.f.Sync())
}

func (h *handle) Flush(ctx context.Context) syscall.Errno { return 0 }

func (h *handle) Lseek(ctx context.Context, off uint64, whence uint32) (uint64, syscall.Errno) {
	n, err := h.f.Seek(int64(off), int(whence))
	return uint64(n), errno(err)
}

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// roRoot serves a read-only Layer: the tree comes from a manifest, file
// content from blobTiers (fetched lazily on open), opened files are handed to
// the kernel via FUSE passthrough when enabled.
type roRoot struct {
	fs.Inode
	m           *Manifest
	blobs       *blobTiers
	passthrough bool
}

type roNode struct {
	fs.Inode
	e    *Entry
	ino  uint64
	root *roRoot

	mu        sync.Mutex
	lazyOpens int // open handles in cached (non-passthrough) io mode
}

var _ = (fs.NodeOnAdder)((*roRoot)(nil))

func typeBits(t byte) uint32 {
	switch t {
	case 'd':
		return syscall.S_IFDIR
	case 'l':
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

func (r *roRoot) OnAdd(ctx context.Context) {
	nodes := map[string]*fs.Inode{"": &r.Inode}
	for i := range r.m.Entries {
		e := &r.m.Entries[i]
		if e.Path == "" {
			continue
		}
		dir := path.Dir(e.Path)
		if dir == "." {
			dir = ""
		}
		parent := nodes[dir]
		if parent == nil {
			continue
		}
		ino := uint64(i + 2)
		child := parent.NewPersistentInode(ctx, &roNode{e: e, ino: ino, root: r},
			fs.StableAttr{Mode: typeBits(e.Type), Ino: ino})
		parent.AddChild(path.Base(e.Path), child, false)
		if e.Type == 'd' {
			nodes[e.Path] = child
		}
	}
}

func (r *roRoot) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o755
	out.Nlink = 2
	return 0
}

func (r *roRoot) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, fs.ENOATTR
}

func (n *roNode) fill(a *fuse.Attr) {
	e := n.e
	a.Ino = n.ino
	a.Mode = typeBits(e.Type) | e.Mode
	a.Size = uint64(e.Size)
	if e.Type == 'l' {
		a.Size = uint64(len(e.Target))
	}
	a.Blocks = (a.Size + 511) / 512
	a.Blksize = 4096
	a.Nlink = 1
	if e.Type == 'd' {
		a.Nlink = 2
	}
	sec, nsec := uint64(e.MtimeNs/1e9), uint32(e.MtimeNs%1e9)
	a.Mtime, a.Mtimensec = sec, nsec
	a.Ctime, a.Ctimensec = sec, nsec
	a.Atime, a.Atimensec = sec, nsec
}

func (n *roNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.fill(&out.Attr)
	return 0
}

func (n *roNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return []byte(n.e.Target), 0
}

func (n *roNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, fs.ENOATTR
}

func (n *roNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	return 0, 0
}

// Open never fetches: metadata-only opens (overlayfs copy-up reads file
// attributes through an open + ioctl) must not pull blobs. A blob that is
// already local is opened with passthrough; otherwise the handle fetches on
// its first Read. The kernel rejects passthrough opens of an inode that has
// cached-mode opens, so while lazy handles exist new opens stay lazy too.
func (n *roNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if p, ok := n.root.blobs.local(n.e.Digest); ok && n.lazyOpens == 0 {
		f, err := os.Open(p)
		if err != nil {
			return nil, 0, fs.ToErrno(err)
		}
		rf := &roFile{f: f, node: n}
		if n.root.passthrough {
			return &roFilePT{rf}, fuse.FOPEN_KEEP_CACHE, 0
		}
		return rf, fuse.FOPEN_KEEP_CACHE, 0
	}
	n.lazyOpens++
	return &roFile{node: n, lazy: true}, fuse.FOPEN_KEEP_CACHE, 0
}

type roFile struct {
	mu   sync.Mutex
	f    *os.File
	node *roNode
	lazy bool
}

func (f *roFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.mu.Lock()
	if f.f == nil {
		p, err := f.node.root.blobs.path(f.node.e.Digest, f.node.e.Size)
		if err == nil {
			f.f, err = os.Open(p)
		}
		if err != nil {
			f.mu.Unlock()
			return nil, fs.ToErrno(err)
		}
	}
	f.mu.Unlock()
	n, err := f.f.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fs.ToErrno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *roFile) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	f.node.fill(&out.Attr)
	return 0
}

func (f *roFile) Flush(ctx context.Context) syscall.Errno { return 0 }

func (f *roFile) Release(ctx context.Context) syscall.Errno {
	if f.lazy {
		f.node.mu.Lock()
		f.node.lazyOpens--
		f.node.mu.Unlock()
	}
	if f.f == nil {
		return 0
	}
	return fs.ToErrno(f.f.Close())
}

// roFilePT additionally offers its fd for kernel passthrough.
type roFilePT struct{ *roFile }

func (f *roFilePT) PassthroughFd() (int, bool) { return int(f.f.Fd()), true }

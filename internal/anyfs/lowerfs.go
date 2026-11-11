package anyfs

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// LowerOptions configure a LowerFS mount.
type LowerOptions struct {
	// FsName shows in /proc/self/mountinfo (fuse.<FsName>).
	FsName string
	// Timeout is the kernel's entry and attribute cache time (default 1h;
	// Invalidate updates changed entries).
	Timeout time.Duration
	// NoPassthrough disables kernel passthrough (it needs CAP_SYS_ADMIN
	// and kernel >= 6.9; without it reads go through the daemon).
	NoPassthrough bool
	// PrefetchMax: at mount, regular files up to this size are faulted in
	// in the background (0: none).
	PrefetchMax int64
	// AllowOther lets other users (and the kernel overlay on top) access
	// the mount.
	AllowOther bool
}

// LowerFS is a read-only FUSE file system over a Source: the lower layer
// of an overlay. Opens never fetch (overlayfs copy-up opens lower files
// to read their attributes); content is faulted in on the first read.
// Resident files are handed to the kernel with passthrough, so reads and
// mmaps of cached data never reach the daemon. Flush, xattrs and statx
// are answered with ENOSYS once, so the kernel stops asking.
type LowerFS struct {
	src   Source
	opt   LowerOptions
	srv   *fuse.Server
	root  *lnode
	mnt   string
	ptOK  bool
	ops   *opCounter
	cache sync.Map // rel -> *fs.Inode (looked up nodes, for invalidation)
}

type lnode struct {
	fs.Inode
	fsys *LowerFS
	rel  string

	mu        sync.Mutex
	lazyOpens int
}

var (
	_ fs.NodeLookuper   = (*lnode)(nil)
	_ fs.NodeGetattrer  = (*lnode)(nil)
	_ fs.NodeReaddirer  = (*lnode)(nil)
	_ fs.NodeReadlinker = (*lnode)(nil)
	_ fs.NodeOpener     = (*lnode)(nil)
	_ fs.NodeStatfser   = (*lnode)(nil)
)

// MountLower mounts src read-only at mnt.
func MountLower(mnt string, src Source, opt LowerOptions) (*LowerFS, error) {
	if opt.Timeout == 0 {
		opt.Timeout = time.Hour
	}
	if opt.FsName == "" {
		opt.FsName = "offshore-lower"
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return nil, err
	}
	l := &LowerFS{src: src, opt: opt, mnt: mnt}
	l.root = &lnode{fsys: l}
	to := opt.Timeout
	fopts := &fs.Options{EntryTimeout: &to, AttrTimeout: &to, NegativeTimeout: &to}
	fopts.MountOptions = fuse.MountOptions{
		AllowOther: opt.AllowOther, FsName: opt.FsName, Name: opt.FsName,
		DirectMount: true, MaxWrite: 1 << 20, MaxBackground: 64, EnableSymlinkCaching: true,
		Options: []string{"ro"},
	}
	if opt.NoPassthrough {
		fopts.DisabledCapabilities |= fuse.CAP_PASSTHROUGH
	}
	l.ops = &opCounter{RawFileSystem: &lowerRaw{RawFileSystem: fs.NewNodeFS(l.root, fopts)}}
	srv, err := fuse.NewServer(l.ops, mnt, &fopts.MountOptions)
	if err != nil {
		return nil, err
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		srv.Unmount()
		return nil, err
	}
	l.srv = srv
	l.ptOK = !opt.NoPassthrough && srv.KernelSettings().Flags64()&fuse.CAP_PASSTHROUGH != 0
	if opt.PrefetchMax > 0 {
		if ts, ok := src.(*TreeSource); ok {
			go ts.PrefetchSmall(context.Background(), opt.PrefetchMax, 16)
		}
	}
	return l, nil
}

// PrefetchSmall faults in every file up to max bytes, smallest first,
// and returns the number fetched.
func (s *TreeSource) PrefetchSmall(ctx context.Context, max int64, parallel int) int {
	var nodes []*Node
	t := s.Tree()
	for i := range t.Nodes() {
		n := &t.Nodes()[i]
		if n.Kind == File && n.Size <= max {
			if _, ok := s.files.Local(n); !ok {
				nodes = append(nodes, n)
			}
		}
	}
	return s.files.Prefetch(ctx, s.tier, nodes, parallel)
}

// Mountpoint returns the mount point.
func (l *LowerFS) Mountpoint() string { return l.mnt }

// Ops returns the histogram of FUSE requests that reached the daemon
// (count and service time per operation).
func (l *LowerFS) Ops() map[string]OpStat { return l.ops.Ops() }

// Passthrough reports whether the kernel accepted passthrough.
func (l *LowerFS) Passthrough() bool { return l.ptOK }

// Unmount unmounts (lazily if busy).
func (l *LowerFS) Unmount() error {
	err := l.srv.Unmount()
	if err != nil {
		syscall.Unmount(l.mnt, syscall.MNT_DETACH)
	}
	l.srv.Wait()
	return err
}

// Invalidate tells the kernel that the given paths changed in the source
// (after TreeSource.Update): cached entries and contents are dropped.
func (l *LowerFS) Invalidate(paths []string) {
	for _, p := range paths {
		dir, name := parent(p), path.Base(p)
		if v, ok := l.cache.Load(p); ok {
			v.(*fs.Inode).NotifyContent(0, 0)
		}
		var pi *fs.Inode
		if dir == "" {
			pi = &l.root.Inode
		} else if v, ok := l.cache.Load(dir); ok {
			pi = v.(*fs.Inode)
		}
		if pi != nil {
			pi.NotifyEntry(name)
		}
	}
}

func kindBits(k uint8) uint32 {
	switch k {
	case uint8(Dir):
		return syscall.S_IFDIR
	case uint8(Symlink):
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

// inoOf is a stable inode number per path, kind and content: changed
// content is a new inode (the kernel drops the old one's caches and
// passthrough backing file).
func inoOf(n *Node) uint64 {
	h := fnv.New64a()
	h.Write([]byte(n.Path))
	h.Write([]byte{0, byte(n.Kind)})
	h.Write([]byte(n.Digest))
	v := h.Sum64()
	if v < 2 || v == ^uint64(0) {
		v += 2
	}
	return v
}

func (l *LowerFS) attr(n *Node, a *fuse.Attr) {
	a.Mode = kindBits(uint8(n.Kind)) | (n.Mode & 0o7777)
	a.Size = uint64(n.Size)
	if n.Kind == Symlink {
		a.Size = uint64(len(n.Target))
	}
	a.Blocks = (a.Size + 511) / 512
	a.Blksize = 4096
	a.Nlink = 1
	if n.Kind == Dir {
		a.Nlink = 2
	}
	sec, nsec := uint64(n.MtimeNs/1e9), uint32(n.MtimeNs%1e9)
	a.Mtime, a.Mtimensec = sec, nsec
	a.Ctime, a.Ctimensec = sec, nsec
	a.Atime, a.Atimensec = sec, nsec
	if n.Path != "" {
		a.Ino = inoOf(n)
	}
}

func (n *lnode) node() (*Node, syscall.Errno) {
	nd, ok := n.fsys.src.Node(n.rel)
	if !ok {
		return nil, syscall.ENOENT
	}
	return nd, 0
}

func (n *lnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := name
	if n.rel != "" {
		rel = n.rel + "/" + name
	}
	nd, ok := n.fsys.src.Node(rel)
	if !ok {
		return nil, syscall.ENOENT
	}
	n.fsys.attr(nd, &out.Attr)
	ch := n.NewInode(ctx, &lnode{fsys: n.fsys, rel: rel}, fs.StableAttr{Mode: kindBits(uint8(nd.Kind)), Ino: inoOf(nd)})
	n.fsys.cache.Store(rel, ch)
	return ch, 0
}

func (n *lnode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	nd, e := n.node()
	if e != 0 {
		return e
	}
	n.fsys.attr(nd, &out.Attr)
	return 0
}

func (n *lnode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	kids, err := n.fsys.src.Children(n.rel)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	es := make([]fuse.DirEntry, 0, len(kids))
	for _, k := range kids {
		es = append(es, fuse.DirEntry{Name: path.Base(k.Path), Mode: kindBits(uint8(k.Kind)), Ino: inoOf(k)})
	}
	return fs.NewListDirStream(es), 0
}

func (n *lnode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	nd, e := n.node()
	if e != 0 {
		return nil, e
	}
	return []byte(nd.Target), 0
}

func (n *lnode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &st); err == nil {
		out.FromStatfsT(&st)
	}
	return 0
}

// Open never fetches. A resident file is opened for passthrough; other
// handles fault the content in on their first read. The kernel refuses
// passthrough opens of an inode with cached-mode opens, so while lazy
// handles exist new opens stay lazy too.
func (n *lnode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	nd, e := n.node()
	if e != 0 {
		return nil, 0, e
	}
	if nd.Kind != File {
		return nil, 0, syscall.EISDIR
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if p, ok := n.fsys.src.Local(nd); ok && n.lazyOpens == 0 {
		f, err := os.Open(p)
		if err == nil {
			h := &lfile{f: f, nd: nd, n: n}
			n.acquire(nd)
			if n.fsys.ptOK {
				return &lfilePT{h}, fuse.FOPEN_KEEP_CACHE, 0
			}
			return h, fuse.FOPEN_KEEP_CACHE, 0
		}
	}
	n.lazyOpens++
	return &lfile{nd: nd, n: n, lazy: true}, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *lnode) acquire(nd *Node) {
	if ts, ok := n.fsys.src.(*TreeSource); ok {
		ts.files.Acquire(nd)
	}
}

func (n *lnode) release(nd *Node) {
	if ts, ok := n.fsys.src.(*TreeSource); ok {
		ts.files.Release(nd)
	}
}

type lfile struct {
	mu   sync.Mutex
	f    *os.File
	nd   *Node
	n    *lnode
	lazy bool
}

var (
	_ fs.FileReader    = (*lfile)(nil)
	_ fs.FileReleaser  = (*lfile)(nil)
	_ fs.FileGetattrer = (*lfile)(nil)
)

func (h *lfile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	if h.f == nil {
		p, err := h.n.fsys.src.Fetch(ctx, h.nd)
		if err == nil {
			h.f, err = os.Open(p)
		}
		if err != nil {
			h.mu.Unlock()
			return nil, syscall.EIO
		}
		h.n.acquire(h.nd)
	}
	f := h.f
	h.mu.Unlock()
	k, err := f.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fs.ToErrno(err)
	}
	return fuse.ReadResultData(dest[:k]), 0
}

func (h *lfile) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	h.n.fsys.attr(h.nd, &out.Attr)
	return 0
}

func (h *lfile) Release(ctx context.Context) syscall.Errno {
	if h.lazy {
		h.n.mu.Lock()
		h.n.lazyOpens--
		h.n.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return 0
	}
	h.n.release(h.nd)
	err := h.f.Close()
	h.f = nil
	return fs.ToErrno(err)
}

// lfilePT offers its file for kernel passthrough.
type lfilePT struct{ *lfile }

func (h *lfilePT) PassthroughFd() (int, bool) { return int(h.f.Fd()), true }

// lowerRaw answers flush, xattrs and statx with ENOSYS without asking the
// node file system: the kernel then stops sending them for the mount
// (halves the requests reaching the daemon on a build).
type lowerRaw struct{ fuse.RawFileSystem }

func (lowerRaw) Flush(<-chan struct{}, *fuse.FlushIn) fuse.Status { return fuse.ENOSYS }
func (lowerRaw) GetXAttr(<-chan struct{}, *fuse.InHeader, string, []byte) (uint32, fuse.Status) {
	return 0, fuse.ENOSYS
}
func (lowerRaw) ListXAttr(<-chan struct{}, *fuse.InHeader, []byte) (uint32, fuse.Status) {
	return 0, fuse.ENOSYS
}
func (lowerRaw) Statx(<-chan struct{}, *fuse.StatxIn, *fuse.StatxOut) fuse.Status {
	return fuse.ENOSYS
}

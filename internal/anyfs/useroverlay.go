package anyfs

import (
	"context"
	"hash/fnv"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Merge decides which layer shows when both have a non directory at a
// path.
type MergePolicy int

const (
	// TopWins: the upper entry always shows (overlayfs semantics).
	TopWins MergePolicy = iota
	// NewestMtime: the entry with the newer mtime shows (both sides are
	// written independently, e.g. local and remote builds of target/).
	NewestMtime
)

// Whiteout markers in the upper directory of a UserOverlay.
const (
	WhiteoutPrefix = ".wh."
	OpaqueMarker   = ".wh..wh..opq"
)

// UserOverlayOptions configure a UserOverlay.
type UserOverlayOptions struct {
	FsName  string
	Merge   MergePolicy
	Timeout time.Duration // kernel attribute/entry cache (default 1s)
	// NoPassthrough disables passthrough for upper files.
	NoPassthrough bool
	AllowOther    bool
}

// UserOverlay is a writable union mounted with FUSE by an unprivileged
// process (the laptop): an upper directory over a read-only Source.
// Writes go to the upper directory (copy-up of whole files on first
// write), deletions of lower entries leave whiteout markers (.wh.NAME),
// a directory replacing a lower one is opaque (.wh..wh..opq).
type UserOverlay struct {
	upper string
	lower Source
	opt   UserOverlayOptions
	srv   *fuse.Server
	mnt   string
	mu    sync.Mutex // serializes namespace changes (copy-up, whiteouts)
}

// MountUserOverlay mounts the union of upper over lower at mnt.
func MountUserOverlay(mnt, upper string, lower Source, opt UserOverlayOptions) (*UserOverlay, error) {
	if opt.Timeout == 0 {
		opt.Timeout = time.Second
	}
	if opt.FsName == "" {
		opt.FsName = "offshore-overlay"
	}
	for _, d := range []string{mnt, upper} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	u := &UserOverlay{upper: upper, lower: lower, opt: opt, mnt: mnt}
	to := opt.Timeout
	fopts := &fs.Options{EntryTimeout: &to, AttrTimeout: &to}
	fopts.MountOptions = fuse.MountOptions{AllowOther: opt.AllowOther, FsName: opt.FsName, Name: opt.FsName, MaxWrite: 1 << 20}
	if opt.NoPassthrough {
		fopts.DisabledCapabilities |= fuse.CAP_PASSTHROUGH
	}
	srv, err := fs.Mount(mnt, &unode{u: u}, fopts)
	if err != nil {
		return nil, err
	}
	u.srv = srv
	return u, nil
}

// Unmount unmounts (lazily if busy).
func (u *UserOverlay) Unmount() error {
	err := u.srv.Unmount()
	if err != nil {
		syscall.Unmount(u.mnt, syscall.MNT_DETACH)
	}
	u.srv.Wait()
	return err
}

// Mountpoint returns the mount point.
func (u *UserOverlay) Mountpoint() string { return u.mnt }

// Upper returns the upper directory.
func (u *UserOverlay) Upper() string { return u.upper }

func (u *UserOverlay) up(rel string) string { return filepath.Join(u.upper, filepath.FromSlash(rel)) }

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// whited reports whether a whiteout or an opaque ancestor hides the
// lower entry at rel.
func (u *UserOverlay) whited(rel string) bool {
	if rel == "" {
		return false
	}
	dir, name := parent(rel), path.Base(rel)
	if _, err := os.Lstat(filepath.Join(u.up(dir), WhiteoutPrefix+name)); err == nil {
		return true
	}
	for d := dir; ; d = parent(d) {
		if _, err := os.Lstat(filepath.Join(u.up(d), OpaqueMarker)); err == nil {
			return true
		}
		if d == "" {
			return false
		}
	}
}

// entry is the resolved view of a path.
type entry struct {
	node  Node   // metadata as shown
	upper string // upper path ("" if the lower shows)
	lower *Node  // lower node (nil if none or hidden)
}

func (u *UserOverlay) resolve(rel string) (entry, bool) {
	var e entry
	var low *Node
	if !u.whited(rel) {
		if n, ok := u.lower.Node(rel); ok {
			low = n
		}
	}
	if rel != "" && strings.HasPrefix(path.Base(rel), WhiteoutPrefix) {
		return e, false
	}
	p := u.up(rel)
	fi, err := os.Lstat(p)
	if err == nil {
		up := nodeOf(rel, fi, p)
		if low != nil && u.opt.Merge == NewestMtime && up.Kind != Dir && low.Kind != Dir && low.MtimeNs > up.MtimeNs {
			return entry{node: *low, lower: low}, true
		}
		if low != nil && (low.Kind != Dir || up.Kind != Dir) {
			low = nil // replaced
		}
		return entry{node: up, upper: p, lower: low}, true
	}
	if low == nil {
		return e, false
	}
	return entry{node: *low, lower: low}, true
}

// ensureUpperDir creates the upper directory for rel and its parents
// with the shown modes.
func (u *UserOverlay) ensureUpperDir(rel string) error {
	if rel == "" {
		return os.MkdirAll(u.upper, 0o755)
	}
	p := u.up(rel)
	if fi, err := os.Lstat(p); err == nil {
		if fi.IsDir() {
			return nil
		}
		return syscall.ENOTDIR
	}
	if err := u.ensureUpperDir(parent(rel)); err != nil {
		return err
	}
	mode := os.FileMode(0o755)
	var mt time.Time
	if n, ok := u.lower.Node(rel); ok && n.Kind == Dir {
		mode = os.FileMode(n.Mode & 0o777)
		mt = time.Unix(0, n.MtimeNs)
	}
	if err := os.Mkdir(p, mode|0o700); err != nil && !os.IsExist(err) {
		return err
	}
	if !mt.IsZero() {
		os.Chtimes(p, mt, mt)
	}
	return nil
}

// copyUp makes the shown entry at rel an upper entry (whole content,
// mode and mtime) and returns its path.
func (u *UserOverlay) copyUp(ctx context.Context, rel string) (string, error) {
	e, ok := u.resolve(rel)
	if !ok {
		return "", syscall.ENOENT
	}
	if e.upper != "" {
		return e.upper, nil
	}
	n := e.lower
	if err := u.ensureUpperDir(parent(rel)); err != nil {
		return "", err
	}
	p := u.up(rel)
	switch n.Kind {
	case Dir:
		if err := u.ensureUpperDir(rel); err != nil {
			return "", err
		}
	case Symlink:
		os.Remove(p)
		if err := os.Symlink(n.Target, p); err != nil {
			return "", err
		}
	case File:
		src, err := u.lower.Fetch(ctx, n)
		if err != nil {
			return "", syscall.EIO
		}
		tmp := p + ".offshore-copyup"
		if err := cloneFile(src, tmp); err != nil {
			os.Remove(tmp)
			return "", err
		}
		os.Chmod(tmp, os.FileMode(n.Mode&0o7777))
		mt := time.Unix(0, n.MtimeNs)
		os.Chtimes(tmp, mt, mt)
		if err := os.Rename(tmp, p); err != nil {
			return "", err
		}
	}
	os.Remove(filepath.Join(u.up(parent(rel)), WhiteoutPrefix+path.Base(rel)))
	return p, nil
}

// whiteout hides the lower entry at rel (if any).
func (u *UserOverlay) whiteout(rel string) error {
	if _, ok := u.lower.Node(rel); !ok || u.whited(rel) {
		return nil
	}
	if err := u.ensureUpperDir(parent(rel)); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(u.up(parent(rel)), WhiteoutPrefix+path.Base(rel)), nil, 0o644)
}

// unwhite removes a whiteout of name in dir before a new entry is made;
// a new directory over a hidden lower one becomes opaque.
func (u *UserOverlay) unwhite(rel string, dir bool) {
	wh := filepath.Join(u.up(parent(rel)), WhiteoutPrefix+path.Base(rel))
	if _, err := os.Lstat(wh); err == nil {
		os.Remove(wh)
		if dir {
			os.WriteFile(filepath.Join(u.up(rel), OpaqueMarker), nil, 0o644)
		}
	}
}

type unode struct {
	fs.Inode
	u *UserOverlay
}

// rel is the node's current path (renames move inodes).
func (n *unode) rel() string { return n.Path(nil) }

var (
	_ fs.NodeLookuper   = (*unode)(nil)
	_ fs.NodeGetattrer  = (*unode)(nil)
	_ fs.NodeSetattrer  = (*unode)(nil)
	_ fs.NodeReaddirer  = (*unode)(nil)
	_ fs.NodeReadlinker = (*unode)(nil)
	_ fs.NodeOpener     = (*unode)(nil)
	_ fs.NodeCreater    = (*unode)(nil)
	_ fs.NodeMkdirer    = (*unode)(nil)
	_ fs.NodeSymlinker  = (*unode)(nil)
	_ fs.NodeLinker     = (*unode)(nil)
	_ fs.NodeUnlinker   = (*unode)(nil)
	_ fs.NodeRmdirer    = (*unode)(nil)
	_ fs.NodeRenamer    = (*unode)(nil)
	_ fs.NodeStatfser   = (*unode)(nil)
)

func uino(rel string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(rel))
	v := h.Sum64()
	if v < 2 || v == ^uint64(0) {
		v += 2
	}
	return v
}

func (n *unode) attr(e entry, a *fuse.Attr) { n.attrAt(n.rel(), e, a) }

func (n *unode) attrAt(rel string, e entry, a *fuse.Attr) {
	if e.upper != "" {
		var st syscall.Stat_t
		if syscall.Lstat(e.upper, &st) == nil {
			a.FromStat(&st)
			a.Ino = uino(rel)
			return
		}
	}
	(&LowerFS{}).attr(&e.node, a)
	a.Ino = uino(rel)
}

func (n *unode) child(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := join(n.rel(), name)
	e, ok := n.u.resolve(rel)
	if !ok {
		return nil, syscall.ENOENT
	}
	c := &unode{u: n.u}
	c.attrAt(rel, e, &out.Attr)
	return n.NewInode(ctx, c, fs.StableAttr{Mode: kindBits(uint8(e.node.Kind)), Ino: uino(rel)}), 0
}

func (n *unode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.child(ctx, name, out)
}

func (n *unode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	var h *ufile
	switch v := fh.(type) {
	case *ufile:
		h = v
	case *ufilePT:
		h = v.ufile
	}
	if h != nil && h.f != nil {
		var st syscall.Stat_t
		if err := syscall.Fstat(int(h.f.Fd()), &st); err == nil {
			out.FromStat(&st)
			out.Ino = uino(n.rel())
			return 0
		}
	}
	e, ok := n.u.resolve(n.rel())
	if !ok {
		return syscall.ENOENT
	}
	n.attr(e, &out.Attr)
	return 0
}

func (n *unode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.u.mu.Lock()
	p, err := n.u.copyUp(ctx, n.rel())
	n.u.mu.Unlock()
	if err != nil {
		return fs.ToErrno(err)
	}
	if m, ok := in.GetMode(); ok {
		if err := syscall.Chmod(p, m&0o7777); err != nil {
			return fs.ToErrno(err)
		}
	}
	if sz, ok := in.GetSize(); ok {
		if err := syscall.Truncate(p, int64(sz)); err != nil {
			return fs.ToErrno(err)
		}
	}
	at, aok := in.GetATime()
	mt, mok := in.GetMTime()
	if aok || mok {
		var st syscall.Stat_t
		syscall.Lstat(p, &st)
		if !aok {
			at = time.Unix(st.Atim.Unix())
		}
		if !mok {
			mt = time.Unix(st.Mtim.Unix())
		}
		if err := os.Chtimes(p, at, mt); err != nil {
			return fs.ToErrno(err)
		}
	}
	return n.Getattr(ctx, nil, out)
}

func (n *unode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	return n.u.readdir(n.rel()), 0
}

func (u *UserOverlay) readdir(dir string) fs.DirStream {
	seen := map[string]bool{}
	var es []fuse.DirEntry
	if des, err := os.ReadDir(u.up(dir)); err == nil {
		for _, de := range des {
			name := de.Name()
			if strings.HasPrefix(name, WhiteoutPrefix) || strings.HasSuffix(name, ".offshore-copyup") {
				continue
			}
			if e, ok := u.resolve(join(dir, name)); ok {
				seen[name] = true
				es = append(es, fuse.DirEntry{Name: name, Mode: kindBits(uint8(e.node.Kind)), Ino: uino(join(dir, name))})
			}
		}
	}
	if kids, err := u.lower.Children(dir); err == nil {
		for _, k := range kids {
			name := path.Base(k.Path)
			if seen[name] || u.whited(k.Path) {
				continue
			}
			es = append(es, fuse.DirEntry{Name: name, Mode: kindBits(uint8(k.Kind)), Ino: uino(k.Path)})
		}
	}
	return fs.NewListDirStream(es)
}

func (n *unode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	e, ok := n.u.resolve(n.rel())
	if !ok {
		return nil, syscall.ENOENT
	}
	if e.upper != "" {
		t, err := os.Readlink(e.upper)
		return []byte(t), fs.ToErrno(err)
	}
	return []byte(e.node.Target), 0
}

func (n *unode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(n.u.upper, &st); err != nil {
		return fs.ToErrno(err)
	}
	out.FromStatfsT(&st)
	return 0
}

func (n *unode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	e, ok := n.u.resolve(n.rel())
	if !ok {
		return nil, 0, syscall.ENOENT
	}
	p := e.upper
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY || flags&syscall.O_TRUNC != 0 {
		n.u.mu.Lock()
		var err error
		p, err = n.u.copyUp(ctx, n.rel())
		n.u.mu.Unlock()
		if err != nil {
			return nil, 0, fs.ToErrno(err)
		}
	} else if p == "" {
		var err error
		if p, err = n.u.lower.Fetch(ctx, e.lower); err != nil {
			return nil, 0, syscall.EIO
		}
	}
	f, err := os.OpenFile(p, int(flags)&^(syscall.O_CREAT|syscall.O_EXCL), 0)
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	return n.u.handle(f), 0, 0
}

func (u *UserOverlay) handle(f *os.File) fs.FileHandle {
	h := &ufile{f: f}
	if u.opt.NoPassthrough {
		return h
	}
	return &ufilePT{h}
}

func (n *unode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := join(n.rel(), name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	if err := n.u.ensureUpperDir(n.rel()); err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	if e, ok := n.u.resolve(rel); ok && e.upper == "" && flags&syscall.O_EXCL != 0 {
		return nil, nil, 0, syscall.EEXIST
	}
	n.u.unwhite(rel, false)
	f, err := os.OpenFile(n.u.up(rel), int(flags)|os.O_CREATE, os.FileMode(mode&0o7777))
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	ch, errno := n.child(ctx, name, out)
	if errno != 0 {
		f.Close()
		return nil, nil, 0, errno
	}
	return ch, n.u.handle(f), 0, 0
}

func (n *unode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := join(n.rel(), name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	if _, ok := n.u.resolve(rel); ok {
		return nil, syscall.EEXIST
	}
	if err := n.u.ensureUpperDir(n.rel()); err != nil {
		return nil, fs.ToErrno(err)
	}
	if err := os.Mkdir(n.u.up(rel), os.FileMode(mode&0o7777)); err != nil {
		return nil, fs.ToErrno(err)
	}
	n.u.unwhite(rel, true)
	return n.child(ctx, name, out)
}

func (n *unode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := join(n.rel(), name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	if _, ok := n.u.resolve(rel); ok {
		return nil, syscall.EEXIST
	}
	if err := n.u.ensureUpperDir(n.rel()); err != nil {
		return nil, fs.ToErrno(err)
	}
	n.u.unwhite(rel, false)
	if err := os.Symlink(target, n.u.up(rel)); err != nil {
		return nil, fs.ToErrno(err)
	}
	return n.child(ctx, name, out)
}

func (n *unode) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	t, ok := target.(*unode)
	if !ok {
		return nil, syscall.EXDEV
	}
	rel := join(n.rel(), name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	if _, ok := n.u.resolve(rel); ok {
		return nil, syscall.EEXIST
	}
	src, err := n.u.copyUp(ctx, t.rel())
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	if err := n.u.ensureUpperDir(n.rel()); err != nil {
		return nil, fs.ToErrno(err)
	}
	n.u.unwhite(rel, false)
	if err := os.Link(src, n.u.up(rel)); err != nil {
		return nil, fs.ToErrno(err)
	}
	return n.child(ctx, name, out)
}

func (n *unode) remove(rel string, dir bool) syscall.Errno {
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	e, ok := n.u.resolve(rel)
	if !ok {
		return syscall.ENOENT
	}
	if dir {
		if e.node.Kind != Dir {
			return syscall.ENOTDIR
		}
		if ds := n.u.readdir(rel); ds.HasNext() {
			return syscall.ENOTEMPTY
		}
	} else if e.node.Kind == Dir {
		return syscall.EISDIR
	}
	if e.upper != "" {
		if dir {
			// Drop whiteouts and markers left inside.
			des, _ := os.ReadDir(e.upper)
			for _, de := range des {
				os.Remove(filepath.Join(e.upper, de.Name()))
			}
		}
		if err := os.Remove(e.upper); err != nil {
			return fs.ToErrno(err)
		}
	}
	if err := n.u.whiteout(rel); err != nil {
		return fs.ToErrno(err)
	}
	return 0
}

func (n *unode) Unlink(ctx context.Context, name string) syscall.Errno {
	return n.remove(join(n.rel(), name), false)
}

func (n *unode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return n.remove(join(n.rel(), name), true)
}

// Rename moves upper entries; files and symlinks from the lower layer
// are copied up first. Directories that exist in the lower layer cannot
// be renamed (EXDEV: callers such as mv fall back to copying).
func (n *unode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	np, ok := newParent.(*unode)
	if !ok || flags != 0 {
		return syscall.EXDEV
	}
	from, to := join(n.rel(), name), join(np.rel(), newName)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	e, ok := n.u.resolve(from)
	if !ok {
		return syscall.ENOENT
	}
	if e.node.Kind == Dir {
		if _, low := n.u.lower.Node(from); low && !n.u.whited(from) {
			return syscall.EXDEV
		}
	}
	src, err := n.u.copyUp(ctx, from)
	if err != nil {
		return fs.ToErrno(err)
	}
	if t, ok := n.u.resolve(to); ok && t.node.Kind == Dir && e.node.Kind == Dir {
		if t.upper == "" {
			return syscall.EXDEV
		}
	}
	if err := n.u.ensureUpperDir(np.rel()); err != nil {
		return fs.ToErrno(err)
	}
	n.u.unwhite(to, e.node.Kind == Dir)
	if err := os.Rename(src, n.u.up(to)); err != nil {
		return fs.ToErrno(err)
	}
	if err := n.u.whiteout(from); err != nil {
		return fs.ToErrno(err)
	}
	return 0
}

// ufile is an open upper (or fetched lower) file.
type ufile struct {
	mu sync.Mutex
	f  *os.File
}

var (
	_ fs.FileReader    = (*ufile)(nil)
	_ fs.FileWriter    = (*ufile)(nil)
	_ fs.FileFsyncer   = (*ufile)(nil)
	_ fs.FileReleaser  = (*ufile)(nil)
	_ fs.FileFlusher   = (*ufile)(nil)
	_ fs.FileAllocater = (*ufile)(nil)
)

func (h *ufile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	k, err := h.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, fs.ToErrno(err)
	}
	return fuse.ReadResultData(dest[:k]), 0
}

func (h *ufile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	k, err := h.f.WriteAt(data, off)
	return uint32(k), fs.ToErrno(err)
}

func (h *ufile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return fs.ToErrno(h.f.Sync())
}

func (h *ufile) Flush(ctx context.Context) syscall.Errno { return 0 }

func (h *ufile) Allocate(ctx context.Context, off, size uint64, mode uint32) syscall.Errno {
	return fs.ToErrno(syscall.Fallocate(int(h.f.Fd()), mode, int64(off), int64(size)))
}

func (h *ufile) Release(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return 0
	}
	err := h.f.Close()
	h.f = nil
	return fs.ToErrno(err)
}

type ufilePT struct{ *ufile }

func (h *ufilePT) PassthroughFd() (int, bool) { return int(h.f.Fd()), true }

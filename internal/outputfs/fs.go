package outputfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
	"golang.org/x/sys/unix"
)

// FSType is the file system type of output mounts in /proc/self/mountinfo.
const FSType = "fuse.offshore-outputs"

// Marker names in the upper directory. Names starting with WhiteoutPrefix
// are never visible in the mount and cannot be created through it.
const (
	WhiteoutPrefix = ".wh."
	internalPrefix = ".wh..wh." // opaque marker, temporary files
	opaqueName     = ".wh..wh..opq"
)

// FS is one mounted build directory (e.g. <repo>/target): a writable union
// of two layers, implemented in user space with go-fuse.
//
//   - lower: the build directory of the latest remote build, listed per
//     directory and read per file on first open (Tree).
//   - upper: a local directory next to the client state (never inside the
//     repository) that receives every write: cargo's locks and
//     fingerprints, local rebuilds, copied up files.
//
// The local directory the mount covers is hidden while mounted and left
// untouched.
//
// When both layers have a non-directory entry at a path, the newer one
// (mtime) wins, ties go to upper: a copied up file keeps the remote mtime
// and stays visible until a newer remote build replaces it, a local rebuild
// wins over older remote outputs. Directories present in both are merged.
// Deleting or replacing a remote entry writes a whiteout marker (.wh.NAME)
// whose mtime is the deletion time; it hides remote entries up to that
// mtime, so the next remote build that produces the path again makes it
// visible again. An opaque marker (.wh..wh..opq) in a directory that
// replaced a deleted remote directory hides that directory's older remote
// children.
//
// Kernel overlayfs is not used: it cannot be mounted by an unprivileged
// user outside a user namespace (and a mount inside one is invisible to
// the user's shell), and a FUSE lower layer below overlayfs gives no
// control over lazy reads and remote generations.
type FS struct {
	tree       *Tree
	repo       string // repository root
	build      string // build directory relative to repo ("target")
	mountpoint string
	upper      string // upper layer directory
	server     *fuse.Server
	root       *node
	log        *slog.Logger
	uid, gid   uint32

	mu sync.Mutex // serialises changes of the upper layer
}

// Mount mounts the build directory build of repo at its local path with
// the upper layer in upper (created if missing, kept after Unmount).
func Mount(tree *Tree, repo, build, upper string, log *slog.Logger) (*FS, error) {
	build = cleanDir(build)
	if build == "" {
		return nil, errors.New("outputs: empty build directory")
	}
	if upper == "" {
		return nil, errors.New("outputs: no upper directory")
	}
	mp := filepath.Join(repo, filepath.FromSlash(build))
	if up, err := filepath.Abs(upper); err != nil || strings.HasPrefix(up+"/", mp+"/") {
		return nil, fmt.Errorf("outputs: upper directory %s must not be inside %s", upper, mp)
	}
	if IsMounted(mp) {
		// Left over by a daemon that did not shut down cleanly.
		ForceUnmount(mp)
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(upper, 0o755); err != nil {
		return nil, err
	}
	f := &FS{tree: tree, repo: repo, build: build, mountpoint: mp, upper: upper, log: log,
		uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
	f.root = &node{f: f}
	// Negative entries are not cached: a remote build can create any
	// name, and only names the kernel knows can be invalidated.
	entry, attr, neg := 2*time.Second, 2*time.Second, time.Duration(0)
	opts := &gofs.Options{
		EntryTimeout: &entry, AttrTimeout: &attr, NegativeTimeout: &neg,
		UID: f.uid, GID: f.gid,
		MountOptions: fuse.MountOptions{
			FsName:       "offshore",
			Name:         "offshore-outputs",
			MaxReadAhead: 1 << 20,
			// O_TRUNC arrives with open: truncating a remote file
			// (cargo rewriting an output) needs no download.
			ExtraCapabilities: fuse.CAP_ATOMIC_O_TRUNC,
		},
	}
	srv, err := gofs.Mount(mp, f.root, opts)
	if err != nil {
		return nil, fmt.Errorf("mount %s: %w", mp, err)
	}
	f.server = srv
	tree.OnInvalidate(f.invalidate)
	return f, nil
}

// Mountpoint returns the local path of the mount.
func (f *FS) Mountpoint() string { return f.mountpoint }

// Upper returns the upper layer directory.
func (f *FS) Upper() string { return f.upper }

// Unmount unmounts, lazily if the mount is busy (e.g. a proc macro library
// is still mapped by rust-analyzer). The upper layer is kept.
func (f *FS) Unmount() error {
	if f.server == nil {
		return nil
	}
	err := f.server.Unmount()
	if err != nil {
		if ForceUnmount(f.mountpoint) == nil {
			err = nil
		}
	}
	if err == nil {
		f.server.Wait()
		f.server = nil
	}
	return err
}

// ForceUnmount detaches a mount lazily.
func ForceUnmount(mp string) error {
	for _, bin := range []string{"fusermount3", "fusermount"} {
		if p, err := exec.LookPath(bin); err == nil {
			return exec.Command(p, "-uz", mp).Run()
		}
	}
	return syscall.Unmount(mp, syscall.MNT_DETACH)
}

// invalidate drops kernel caches of everything the kernel knows.
func (f *FS) invalidate() {
	if f.server == nil {
		return
	}
	var walk func(n *gofs.Inode)
	walk = func(n *gofs.Inode) {
		n.NotifyContent(0, 0)
		for name, ch := range n.Children() {
			n.NotifyEntry(name)
			walk(ch)
		}
	}
	walk(f.root.EmbeddedInode())
}

// rel returns the repository relative path of a path inside the mount.
func (f *FS) rel(inner string) string {
	if inner == "" {
		return f.build
	}
	return f.build + "/" + inner
}

func (f *FS) upperPath(inner string) string {
	if inner == "" {
		return f.upper
	}
	return filepath.Join(f.upper, filepath.FromSlash(inner))
}

func splitInner(inner string) (dir, name string) {
	dir, name = path.Split(inner)
	return strings.TrimSuffix(dir, "/"), name
}

// layers is what a path refers to.
type layers struct {
	up      os.FileInfo     // upper entry, nil unless it is visible
	low     *proto.OutEntry // lower entry, nil unless it is visible
	lowRaw  *proto.OutEntry // lower entry not hidden by markers (may be shadowed by upper)
	dormant bool            // an upper non-directory exists but is shadowed by low
}

func (l layers) exists() bool { return l.up != nil || l.low != nil }

func (l layers) isDir() bool {
	if l.up != nil {
		return l.up.IsDir()
	}
	return l.low != nil && l.low.Kind == manifest.Dir
}

func (l layers) mode() uint32 {
	if l.up != nil {
		switch {
		case l.up.IsDir():
			return syscall.S_IFDIR
		case l.up.Mode()&os.ModeSymlink != 0:
			return syscall.S_IFLNK
		}
		return syscall.S_IFREG
	}
	return kindMode(l.low.Kind)
}

// markerTime returns the mtime of a marker file, -1 if absent.
func markerTime(p string) int64 {
	fi, err := os.Lstat(p)
	if err != nil {
		return -1
	}
	return fi.ModTime().UnixNano()
}

// combine decides visibility from the raw upper and lower entries of one
// name and the markers of its directory (-1: absent).
func combine(up os.FileInfo, low *proto.OutEntry, whiteout, opaque int64) layers {
	if low != nil && (whiteout >= low.MtimeNs || opaque >= low.MtimeNs) {
		low = nil
	}
	l := layers{up: up, low: low, lowRaw: low}
	if up != nil && low != nil && !(up.IsDir() && low.Kind == manifest.Dir) {
		if up.ModTime().UnixNano() >= low.MtimeNs {
			l.low = nil
		} else {
			l.up, l.dormant = nil, true
		}
	}
	return l
}

// lowerStat returns the lower entry of inner, nil if absent or the remote
// build cannot be reached.
func (f *FS) lowerStat(ctx context.Context, inner string) (*proto.OutEntry, syscall.Errno) {
	e, err := f.tree.Stat(ctx, f.rel(inner))
	if err == nil {
		return &e, 0
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrUnavailable) {
		return nil, 0
	}
	return nil, syscall.EIO
}

// resolve returns the layers of inner (path relative to the mount root).
func (f *FS) resolve(ctx context.Context, inner string) (layers, syscall.Errno) {
	if inner == "" {
		up, err := os.Stat(f.upper)
		if err != nil {
			return layers{}, syscall.EIO
		}
		low, errno := f.lowerStat(ctx, "")
		if errno != 0 {
			return layers{}, errno
		}
		if low != nil && low.Kind != manifest.Dir {
			low = nil
		}
		return layers{up: up, low: low, lowRaw: low}, 0
	}
	dir, name := splitInner(inner)
	parent, errno := f.resolve(ctx, dir)
	if errno != 0 {
		return layers{}, errno
	}
	if !parent.isDir() {
		return layers{}, syscall.ENOTDIR
	}
	var up os.FileInfo
	wh, opq := int64(-1), int64(-1)
	if parent.up != nil {
		up, _ = os.Lstat(f.upperPath(inner))
		wh = markerTime(filepath.Join(f.upperPath(dir), WhiteoutPrefix+name))
		opq = markerTime(filepath.Join(f.upperPath(dir), opaqueName))
	}
	var low *proto.OutEntry
	if parent.low != nil && parent.low.Kind == manifest.Dir {
		if low, errno = f.lowerStat(ctx, inner); errno != 0 {
			return layers{}, errno
		}
	}
	return combine(up, low, wh, opq), 0
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

func (f *FS) fill(l layers, out *fuse.Attr) {
	if l.up != nil {
		out.FromStat(l.up.Sys().(*syscall.Stat_t))
		out.Ino = 0
	} else {
		e := l.low
		out.Mode = kindMode(e.Kind) | (e.Mode & 0o7777)
		out.Size = uint64(e.Size)
		if e.Kind == manifest.Symlink {
			out.Size = uint64(len(e.Target))
		}
		mt := time.Unix(0, e.MtimeNs)
		out.SetTimes(&mt, &mt, &mt)
		out.Owner = fuse.Owner{Uid: f.uid, Gid: f.gid}
		out.Nlink = 1
		if e.Kind == manifest.Dir {
			out.Nlink = 2
		}
	}
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
}

// ensureUpperDir creates the upper directory of inner (which must resolve
// to a directory) and its parents. Callers hold f.mu.
func (f *FS) ensureUpperDir(ctx context.Context, inner string) syscall.Errno {
	if inner == "" {
		return 0
	}
	l, errno := f.resolve(ctx, inner)
	if errno != 0 {
		return errno
	}
	if !l.isDir() {
		return syscall.ENOTDIR
	}
	if l.up != nil {
		return 0
	}
	dir, _ := splitInner(inner)
	if errno := f.ensureUpperDir(ctx, dir); errno != 0 {
		return errno
	}
	p := f.upperPath(inner)
	if l.dormant {
		os.Remove(p)
	}
	mode := os.FileMode(0o755)
	if l.low != nil && l.low.Mode&0o777 != 0 {
		mode = os.FileMode(l.low.Mode & 0o777)
	}
	if err := os.Mkdir(p, mode|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return gofs.ToErrno(err)
	}
	return 0
}

// writeMarker creates or touches a marker file with an mtime of at least
// now and at least min (remote clocks may be ahead).
func writeMarker(p string, min int64) syscall.Errno {
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		return gofs.ToErrno(err)
	}
	t := time.Now()
	if t.UnixNano() < min {
		t = time.Unix(0, min)
	}
	return gofs.ToErrno(os.Chtimes(p, t, t))
}

func (f *FS) whiteoutPath(inner string) string {
	dir, name := splitInner(inner)
	return filepath.Join(f.upperPath(dir), WhiteoutPrefix+name)
}

// hideLower hides whatever the lower layer has at inner, now and in the
// current remote build, and returns that entry (nil: none). The upper
// parent directory must exist. Callers hold f.mu.
func (f *FS) hideLower(ctx context.Context, inner string) (*proto.OutEntry, syscall.Errno) {
	low, errno := f.lowerStat(ctx, inner)
	if errno != 0 {
		return nil, errno
	}
	if low == nil {
		os.Remove(f.whiteoutPath(inner))
		return nil, 0
	}
	return low, writeMarker(f.whiteoutPath(inner), low.MtimeNs)
}

// prepareCreate readies the upper layer for a new entry at inner: parent
// directory, no dormant upper entry, and the lower entry (if any) hidden,
// which it returns. Callers hold f.mu.
func (f *FS) prepareCreate(ctx context.Context, inner string) (*proto.OutEntry, syscall.Errno) {
	dir, _ := splitInner(inner)
	if errno := f.ensureUpperDir(ctx, dir); errno != 0 {
		return nil, errno
	}
	l, errno := f.resolve(ctx, inner)
	if errno != 0 {
		return nil, errno
	}
	if l.exists() {
		return nil, syscall.EEXIST
	}
	if l.dormant {
		os.RemoveAll(f.upperPath(inner))
	}
	return f.hideLower(ctx, inner)
}

// copyUp makes inner exist in the upper layer with the visible content
// (empty if truncate). Callers hold f.mu.
func (f *FS) copyUp(ctx context.Context, inner string, truncate bool) syscall.Errno {
	l, errno := f.resolve(ctx, inner)
	if errno != 0 {
		return errno
	}
	if l.up != nil {
		return 0
	}
	if l.low == nil {
		return syscall.ENOENT
	}
	if l.low.Kind == manifest.Dir {
		return f.ensureUpperDir(ctx, inner)
	}
	dir, name := splitInner(inner)
	if errno := f.ensureUpperDir(ctx, dir); errno != 0 {
		return errno
	}
	dst := f.upperPath(inner)
	tmp := filepath.Join(f.upperPath(dir), internalPrefix+"tmp."+name)
	os.Remove(tmp)
	e := *l.low
	switch e.Kind {
	case manifest.Symlink:
		if err := os.Symlink(e.Target, tmp); err != nil {
			return gofs.ToErrno(err)
		}
	default:
		src := ""
		if !truncate {
			p, err := f.tree.Content(ctx, f.rel(inner), e)
			if err != nil {
				f.log.Warn("outputs: cannot copy up remote output", "path", filepath.Join(f.mountpoint, inner), "err", err)
				return syscall.EIO
			}
			src = p
		}
		if err := copyFile(src, tmp, os.FileMode(e.Mode&0o7777)); err != nil {
			os.Remove(tmp)
			return gofs.ToErrno(err)
		}
	}
	// The copy keeps the remote mtime: it stays visible until a newer
	// remote build replaces the file (and cargo still sees it fresh).
	ts, _ := unix.TimeToTimespec(time.Unix(0, e.MtimeNs))
	unix.UtimesNanoAt(unix.AT_FDCWD, tmp, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
	if l.dormant {
		os.RemoveAll(dst)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return gofs.ToErrno(err)
	}
	os.Remove(f.whiteoutPath(inner))
	return 0
}

// copyFile copies src (empty file if "") to dst, as a reflink if the file
// system supports it.
func copyFile(src, dst string, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o200)
	if err != nil {
		return err
	}
	if src != "" {
		in, err := os.Open(src)
		if err != nil {
			out.Close()
			return err
		}
		if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) != nil {
			_, err = io.Copy(out, in)
		}
		in.Close()
		if err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

type node struct {
	gofs.Inode
	f *FS
}

var (
	_ gofs.NodeLookuper   = (*node)(nil)
	_ gofs.NodeGetattrer  = (*node)(nil)
	_ gofs.NodeSetattrer  = (*node)(nil)
	_ gofs.NodeReaddirer  = (*node)(nil)
	_ gofs.NodeOpener     = (*node)(nil)
	_ gofs.NodeCreater    = (*node)(nil)
	_ gofs.NodeMkdirer    = (*node)(nil)
	_ gofs.NodeUnlinker   = (*node)(nil)
	_ gofs.NodeRmdirer    = (*node)(nil)
	_ gofs.NodeRenamer    = (*node)(nil)
	_ gofs.NodeLinker     = (*node)(nil)
	_ gofs.NodeSymlinker  = (*node)(nil)
	_ gofs.NodeReadlinker = (*node)(nil)
	_ gofs.NodeStatfser   = (*node)(nil)
)

func (n *node) inner() string { return n.Path(n.f.root.EmbeddedInode()) }

func (n *node) child(name string) string { return path.Join(n.inner(), name) }

func reserved(name string) bool { return strings.HasPrefix(name, WhiteoutPrefix) }

func (n *node) newChild(ctx context.Context, name string, l layers, out *fuse.EntryOut) *gofs.Inode {
	n.f.fill(l, &out.Attr)
	if ch := n.GetChild(name); ch != nil && ch.StableAttr().Mode == l.mode() {
		return ch
	}
	return n.NewInode(ctx, &node{f: n.f}, gofs.StableAttr{Mode: l.mode()})
}

// upperChild returns the layers of a just created upper entry.
func upperChild(p string) (layers, syscall.Errno) {
	fi, err := os.Lstat(p)
	if err != nil {
		return layers{}, gofs.ToErrno(err)
	}
	return layers{up: fi}, 0
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.ENOENT
	}
	l, errno := n.f.resolve(ctx, n.child(name))
	if errno != 0 {
		return nil, errno
	}
	if !l.exists() {
		return nil, syscall.ENOENT
	}
	return n.newChild(ctx, name, l, out), 0
}

func (n *node) Getattr(ctx context.Context, fh gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if lf, ok := fh.(*gofs.LoopbackFile); ok {
		errno := lf.Getattr(ctx, out)
		out.Ino = 0
		return errno
	}
	l, errno := n.f.resolve(ctx, n.inner())
	if errno != 0 {
		return errno
	}
	if !l.exists() {
		return syscall.ENOENT
	}
	n.f.fill(l, &out.Attr)
	return 0
}

func (n *node) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if lf, ok := fh.(*gofs.LoopbackFile); ok { // an open upper file
		errno := lf.Setattr(ctx, in, out)
		out.Ino = 0
		return errno
	}
	inner := n.inner()
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	sz, truncate := in.GetSize()
	if errno := n.f.copyUp(ctx, inner, truncate && sz == 0); errno != 0 {
		return errno
	}
	p := n.f.upperPath(inner)
	if m, ok := in.GetMode(); ok {
		if err := syscall.Chmod(p, m); err != nil {
			return gofs.ToErrno(err)
		}
	}
	if truncate {
		if err := syscall.Truncate(p, int64(sz)); err != nil {
			return gofs.ToErrno(err)
		}
	}
	mtime, mok := in.GetMTime()
	atime, aok := in.GetATime()
	if mok || aok {
		ta := unix.Timespec{Nsec: unix.UTIME_OMIT}
		tm := unix.Timespec{Nsec: unix.UTIME_OMIT}
		if aok {
			ta, _ = unix.TimeToTimespec(atime)
		}
		if mok {
			tm, _ = unix.TimeToTimespec(mtime)
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, []unix.Timespec{ta, tm}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return gofs.ToErrno(err)
		}
	}
	l, errno := upperChild(p)
	if errno != 0 {
		return errno
	}
	n.f.fill(l, &out.Attr)
	return 0
}

// readdir returns the visible entries of inner.
func (f *FS) readdir(ctx context.Context, inner string) (map[string]layers, syscall.Errno) {
	l, errno := f.resolve(ctx, inner)
	if errno != 0 {
		return nil, errno
	}
	if !l.isDir() {
		return nil, syscall.ENOTDIR
	}
	ups := map[string]os.FileInfo{}
	wh := map[string]int64{}
	opq := int64(-1)
	if l.up != nil {
		des, err := os.ReadDir(f.upperPath(inner))
		if err != nil {
			return nil, gofs.ToErrno(err)
		}
		for _, de := range des {
			fi, err := de.Info()
			if err != nil {
				continue
			}
			name := de.Name()
			switch {
			case name == opaqueName:
				opq = fi.ModTime().UnixNano()
			case strings.HasPrefix(name, internalPrefix):
			case reserved(name):
				wh[strings.TrimPrefix(name, WhiteoutPrefix)] = fi.ModTime().UnixNano()
			default:
				ups[name] = fi
			}
		}
	}
	var lows map[string]proto.OutEntry
	if l.low != nil && l.low.Kind == manifest.Dir {
		m, err := f.tree.List(ctx, f.rel(inner))
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrUnavailable) {
			return nil, syscall.EIO
		}
		lows = m
	}
	out := map[string]layers{}
	add := func(name string) {
		if _, done := out[name]; done {
			return
		}
		var low *proto.OutEntry
		if e, ok := lows[name]; ok {
			low = &e
		}
		w, ok := wh[name]
		if !ok {
			w = -1
		}
		if c := combine(ups[name], low, w, opq); c.exists() {
			out[name] = c
		}
	}
	for name := range ups {
		add(name)
	}
	for name := range lows {
		if !reserved(name) {
			add(name)
		}
	}
	return out, 0
}

func (n *node) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	m, errno := n.f.readdir(ctx, n.inner())
	if errno != 0 {
		return nil, errno
	}
	list := make([]fuse.DirEntry, 0, len(m))
	for name, l := range m {
		list = append(list, fuse.DirEntry{Name: name, Mode: l.mode()})
	}
	return gofs.NewListDirStream(list), 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	inner := n.inner()
	l, errno := n.f.resolve(ctx, inner)
	if errno != 0 {
		return nil, errno
	}
	switch {
	case l.up != nil:
		t, err := os.Readlink(n.f.upperPath(inner))
		if err != nil {
			return nil, syscall.EINVAL
		}
		return []byte(t), 0
	case l.low != nil && l.low.Kind == manifest.Symlink:
		return []byte(l.low.Target), 0
	}
	return nil, syscall.EINVAL
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(n.f.upper, &st); err != nil {
		return syscall.EIO
	}
	out.FromStatfsT(&st)
	return 0
}

const writeFlags = syscall.O_WRONLY | syscall.O_RDWR | syscall.O_TRUNC | syscall.O_APPEND

// openFlags are the flags passed to open(2) for upper files.
func openFlags(flags uint32) int {
	return int(flags) &^ (syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOCTTY)
}

func (f *FS) openUpper(inner string, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	fd, err := syscall.Open(f.upperPath(inner), openFlags(flags)|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, gofs.ToErrno(err)
	}
	return gofs.NewLoopbackFile(fd), 0, 0
}

func (n *node) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	inner := n.inner()
	if flags&writeFlags != 0 {
		n.f.mu.Lock()
		errno := n.f.copyUp(ctx, inner, flags&syscall.O_TRUNC != 0)
		n.f.mu.Unlock()
		if errno != 0 {
			return nil, 0, errno
		}
		return n.f.openUpper(inner, flags)
	}
	for attempt := 0; ; attempt++ {
		l, errno := n.f.resolve(ctx, inner)
		if errno != 0 {
			return nil, 0, errno
		}
		if l.up != nil {
			return n.f.openUpper(inner, flags)
		}
		if l.low == nil {
			return nil, 0, syscall.ENOENT
		}
		if l.low.Kind == manifest.Dir {
			return nil, 0, syscall.EISDIR
		}
		p, err := n.f.tree.Content(ctx, n.f.rel(inner), *l.low)
		if errors.Is(err, ErrStale) && attempt == 0 {
			continue // listed before a rebuild: list again
		}
		if err != nil {
			n.f.log.Warn("outputs: cannot read remote output", "path", filepath.Join(n.f.mountpoint, inner), "err", err)
			return nil, 0, syscall.EIO
		}
		fh, err := os.Open(p)
		if err != nil {
			return nil, 0, syscall.EIO
		}
		return &handle{f: fh}, 0, 0
	}
}

func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	if reserved(name) {
		return nil, nil, 0, syscall.EINVAL
	}
	inner := n.child(name)
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	if _, errno := n.f.prepareCreate(ctx, inner); errno != 0 {
		return nil, nil, 0, errno
	}
	p := n.f.upperPath(inner)
	fd, err := syscall.Open(p, openFlags(flags)|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, mode)
	if err != nil {
		return nil, nil, 0, gofs.ToErrno(err)
	}
	l, errno := upperChild(p)
	if errno != 0 {
		syscall.Close(fd)
		return nil, nil, 0, errno
	}
	return n.newChild(ctx, name, l, out), gofs.NewLoopbackFile(fd), 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.EINVAL
	}
	inner := n.child(name)
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	hidden, errno := n.f.prepareCreate(ctx, inner)
	if errno != 0 {
		return nil, errno
	}
	p := n.f.upperPath(inner)
	if err := os.Mkdir(p, os.FileMode(mode&0o777)); err != nil {
		return nil, gofs.ToErrno(err)
	}
	if hidden != nil {
		// Older remote children of the replaced entry stay hidden even
		// when a newer remote build makes the directory visible again.
		if errno := writeMarker(filepath.Join(p, opaqueName), hidden.MtimeNs); errno != 0 {
			return nil, errno
		}
	}
	l, errno := upperChild(p)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, name, l, out), 0
}

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.EINVAL
	}
	inner := n.child(name)
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	if _, errno := n.f.prepareCreate(ctx, inner); errno != 0 {
		return nil, errno
	}
	p := n.f.upperPath(inner)
	if err := os.Symlink(target, p); err != nil {
		return nil, gofs.ToErrno(err)
	}
	l, errno := upperChild(p)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, name, l, out), 0
}

func (n *node) Link(ctx context.Context, target gofs.InodeEmbedder, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.EINVAL
	}
	tn, ok := target.(*node)
	if !ok {
		return nil, syscall.EXDEV
	}
	src, dst := tn.inner(), n.child(name)
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	sl, errno := n.f.resolve(ctx, src)
	if errno != 0 {
		return nil, errno
	}
	if sl.up == nil && sl.low != nil && sl.low.Kind == manifest.File {
		// cargo uplifts outputs (deps/x-HASH -> x) by unlinking and hard
		// linking even when they are fresh. When the remote build has the
		// same link, dropping the whiteout restores it without a download.
		if dl, errno := n.f.resolve(ctx, dst); errno == 0 && !dl.exists() && !dl.dormant {
			if de, errno := n.f.lowerStat(ctx, dst); errno == 0 && de != nil && de.Kind == manifest.File &&
				de.Size == sl.low.Size && de.MtimeNs == sl.low.MtimeNs {
				os.Remove(n.f.whiteoutPath(dst))
				if dl, errno := n.f.resolve(ctx, dst); errno == 0 && dl.low != nil {
					return n.newChild(ctx, name, dl, out), 0
				}
			}
		}
	}
	if errno := n.f.copyUp(ctx, src, false); errno != 0 {
		return nil, errno
	}
	if _, errno := n.f.prepareCreate(ctx, dst); errno != 0 {
		return nil, errno
	}
	p := n.f.upperPath(dst)
	if err := os.Link(n.f.upperPath(src), p); err != nil {
		return nil, gofs.ToErrno(err)
	}
	l, errno := upperChild(p)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, name, l, out), 0
}

// remove deletes inner from the merged view. Callers hold f.mu.
func (f *FS) remove(ctx context.Context, inner string, dir bool) syscall.Errno {
	l, errno := f.resolve(ctx, inner)
	if errno != 0 {
		return errno
	}
	if !l.exists() {
		return syscall.ENOENT
	}
	if dir != l.isDir() {
		if dir {
			return syscall.ENOTDIR
		}
		return syscall.EISDIR
	}
	if dir {
		m, errno := f.readdir(ctx, inner)
		if errno != 0 {
			return errno
		}
		if len(m) > 0 {
			return syscall.ENOTEMPTY
		}
	}
	if l.up != nil || l.dormant {
		var err error
		if dir {
			err = os.RemoveAll(f.upperPath(inner)) // only markers are left
		} else {
			err = os.Remove(f.upperPath(inner))
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return gofs.ToErrno(err)
		}
	}
	if l.lowRaw != nil {
		parent, _ := splitInner(inner)
		if errno := f.ensureUpperDir(ctx, parent); errno != 0 {
			return errno
		}
		return writeMarker(f.whiteoutPath(inner), l.lowRaw.MtimeNs)
	}
	return 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	return n.f.remove(ctx, n.child(name), false)
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	return n.f.remove(ctx, n.child(name), true)
}

func (n *node) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if reserved(newName) {
		return syscall.EINVAL
	}
	np, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	if flags&^unix.RENAME_NOREPLACE != 0 {
		return syscall.ENOTSUP
	}
	src, dst := n.child(name), np.child(newName)
	if src == dst {
		return 0
	}
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	sl, errno := n.f.resolve(ctx, src)
	if errno != 0 {
		return errno
	}
	if !sl.exists() {
		return syscall.ENOENT
	}
	if sl.isDir() && sl.lowRaw != nil {
		// Like overlayfs without redirect_dir: callers (mv, rustc's
		// incremental session finalization) fall back to copying.
		return syscall.EXDEV
	}
	dl, errno := n.f.resolve(ctx, dst)
	if errno != 0 {
		return errno
	}
	if dl.exists() {
		if flags&unix.RENAME_NOREPLACE != 0 {
			return syscall.EEXIST
		}
		if dl.isDir() != sl.isDir() {
			if sl.isDir() {
				return syscall.ENOTDIR
			}
			return syscall.EISDIR
		}
		if errno := n.f.remove(ctx, dst, dl.isDir()); errno != 0 {
			return errno
		}
	}
	if errno := n.f.copyUp(ctx, src, false); errno != 0 {
		return errno
	}
	if _, errno := n.f.prepareCreate(ctx, dst); errno != 0 {
		return errno
	}
	if err := os.Rename(n.f.upperPath(src), n.f.upperPath(dst)); err != nil {
		return gofs.ToErrno(err)
	}
	if sl.lowRaw != nil {
		if errno := writeMarker(n.f.whiteoutPath(src), sl.lowRaw.MtimeNs); errno != 0 {
			return errno
		}
	}
	return 0
}

type handle struct{ f *os.File }

var (
	_ gofs.FileReader   = (*handle)(nil)
	_ gofs.FileReleaser = (*handle)(nil)
)

func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *handle) Release(ctx context.Context) syscall.Errno {
	h.f.Close()
	return 0
}

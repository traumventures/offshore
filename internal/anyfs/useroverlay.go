package anyfs

import (
	"context"
	"errors"
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
	"golang.org/x/sys/unix"
)

// MergePolicy decides which layer shows when both have a non directory
// at a path.
type MergePolicy int

const (
	// TopWins: the upper entry always shows and markers hide the lower
	// entry for good (overlayfs semantics).
	TopWins MergePolicy = iota
	// NewestMtime: the entry with the newer mtime shows, ties go to
	// upper (both sides are written independently, e.g. local and remote
	// builds of target/). Markers hide lower entries up to their own
	// mtime, so a newer lower entry shows again.
	NewestMtime
)

// Marker names in the upper directory of a UserOverlay. Names starting
// with WhiteoutPrefix are never visible in the mount and cannot be
// created through it.
const (
	WhiteoutPrefix = ".wh."
	OpaqueMarker   = ".wh..wh..opq"
	internalPrefix = ".wh..wh." // opaque marker, temporary files
)

// UserOverlayOptions configure a UserOverlay.
type UserOverlayOptions struct {
	// FsName is the mount source shown in mountinfo; Name the subtype
	// (file system type "fuse."+Name). Name defaults to FsName.
	FsName string
	Name   string
	Merge  MergePolicy
	// Timeout is the kernel attribute/entry cache (default 1s). Negative
	// entries are never cached: a lower layer can gain any name, and only
	// names the kernel knows can be invalidated.
	Timeout time.Duration
	// NoPassthrough disables kernel passthrough of read-only opens. Write
	// opens never use passthrough (go-fuse shares one backing file per
	// inode, a write must not reach the lower layer's file).
	NoPassthrough bool
	AllowOther    bool
	MaxReadAhead  int
	Log           *slog.Logger
}

// UserOverlay is a writable union mounted with FUSE by an unprivileged
// process (the laptop): an upper directory over a read-only Source.
//
// Writes go to the upper directory (copy-up of whole files on first
// write; a truncating open copies nothing). Deleting or replacing a lower
// entry writes a whiteout marker (.wh.NAME) whose mtime is the deletion
// time, at least the lower entry's mtime; a directory replacing a deleted
// lower directory gets an opaque marker (.wh..wh..opq) hiding the older
// lower children. Directories present in both layers are merged; for
// other entries the MergePolicy decides. A copy-up keeps the lower mtime,
// so under NewestMtime it stays visible until a newer lower entry
// replaces it (and cargo still finds it fresh).
//
// Kernel overlayfs is not used here: it cannot be mounted by an
// unprivileged user outside a user namespace (and a mount inside one is
// invisible to the user's shell), and a FUSE lower layer below overlayfs
// gives no control over lazy reads and lower generations.
type UserOverlay struct {
	upper    string
	lower    Source
	opt      UserOverlayOptions
	srv      *fuse.Server
	mnt      string
	root     *unode
	log      *slog.Logger
	uid, gid uint32
	mu       sync.Mutex // serializes changes of the upper layer
}

// MountUserOverlay mounts the union of upper (created if missing, kept
// after Unmount) over lower at mnt.
func MountUserOverlay(mnt, upper string, lower Source, opt UserOverlayOptions) (*UserOverlay, error) {
	if opt.Timeout == 0 {
		opt.Timeout = time.Second
	}
	if opt.FsName == "" {
		opt.FsName = "offshore-overlay"
	}
	if opt.Name == "" {
		opt.Name = opt.FsName
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, d := range []string{mnt, upper} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	u := &UserOverlay{upper: upper, lower: lower, opt: opt, mnt: mnt, log: opt.Log,
		uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
	u.root = &unode{u: u}
	to, neg := opt.Timeout, time.Duration(0)
	fopts := &gofs.Options{EntryTimeout: &to, AttrTimeout: &to, NegativeTimeout: &neg, UID: u.uid, GID: u.gid}
	fopts.MountOptions = fuse.MountOptions{
		AllowOther: opt.AllowOther, FsName: opt.FsName, Name: opt.Name, MaxWrite: 1 << 20,
		MaxReadAhead: opt.MaxReadAhead,
		// mount(2) where permitted (root in a machine container without
		// fusermount), fusermount otherwise.
		DirectMount: true,
		// O_TRUNC arrives with open: truncating a lower file (cargo
		// rewriting an output) needs no fetch.
		ExtraCapabilities: fuse.CAP_ATOMIC_O_TRUNC,
	}
	if opt.NoPassthrough {
		fopts.DisabledCapabilities |= fuse.CAP_PASSTHROUGH
	}
	srv, err := gofs.Mount(mnt, u.root, fopts)
	if err != nil {
		return nil, err
	}
	u.srv = srv
	return u, nil
}

// Unmount unmounts, lazily if the mount is busy (e.g. a proc macro
// library is still mapped by rust-analyzer). The upper layer is kept.
func (u *UserOverlay) Unmount() error {
	if u.srv == nil {
		return nil
	}
	err := u.srv.Unmount()
	if err != nil && ForceUnmount(u.mnt) == nil {
		err = nil
	}
	if err == nil {
		u.srv.Wait()
		u.srv = nil
	}
	return err
}

// ForceUnmount detaches a FUSE mount lazily.
func ForceUnmount(mp string) error {
	for _, bin := range []string{"fusermount3", "fusermount"} {
		if p, err := exec.LookPath(bin); err == nil {
			return exec.Command(p, "-uz", mp).Run()
		}
	}
	return syscall.Unmount(mp, syscall.MNT_DETACH)
}

// Invalidate drops the kernel caches (entries, attributes, contents) of
// everything the kernel knows: call it when the lower layer changed (a
// new generation of a remote listing).
func (u *UserOverlay) Invalidate() {
	if u.srv == nil {
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
	walk(u.root.EmbeddedInode())
}

// Mountpoint returns the mount point.
func (u *UserOverlay) Mountpoint() string { return u.mnt }

// Upper returns the upper directory.
func (u *UserOverlay) Upper() string { return u.upper }

func (u *UserOverlay) upperPath(rel string) string {
	if rel == "" {
		return u.upper
	}
	return filepath.Join(u.upper, filepath.FromSlash(rel))
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func splitRel(rel string) (dir, name string) {
	return parent(rel), path.Base(rel)
}

func reserved(name string) bool { return strings.HasPrefix(name, WhiteoutPrefix) }

// layers is what a path refers to.
type layers struct {
	up      os.FileInfo // upper entry, nil unless it is visible
	low     *Node       // lower entry, nil unless it is visible
	lowRaw  *Node       // lower entry not hidden by markers (may be shadowed by upper)
	dormant bool        // an upper non-directory exists but is shadowed by low
}

func (l layers) exists() bool { return l.up != nil || l.low != nil }

func (l layers) isDir() bool {
	if l.up != nil {
		return l.up.IsDir()
	}
	return l.low != nil && l.low.Kind == Dir
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
	return kindBits(uint8(l.low.Kind))
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
func (u *UserOverlay) combine(up os.FileInfo, low *Node, whiteout, opaque int64) layers {
	hides := func(marker int64) bool {
		return marker >= 0 && (u.opt.Merge == TopWins || marker >= low.MtimeNs)
	}
	if low != nil && (hides(whiteout) || hides(opaque)) {
		low = nil
	}
	l := layers{up: up, low: low, lowRaw: low}
	if up != nil && low != nil && !(up.IsDir() && low.Kind == Dir) {
		if u.opt.Merge == TopWins || up.ModTime().UnixNano() >= low.MtimeNs {
			l.low = nil
		} else {
			l.up, l.dormant = nil, true
		}
	}
	return l
}

// lowerStat returns the lower entry at rel, nil if absent or unreachable
// and not cached.
func (u *UserOverlay) lowerStat(ctx context.Context, rel string) (*Node, syscall.Errno) {
	ss, ok := u.lower.(StatSource)
	if !ok {
		if n, ok := u.lower.Node(rel); ok {
			return n, 0
		}
		return nil, 0
	}
	n, err := ss.Stat(ctx, rel)
	if err == nil {
		return n, 0
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrUnavailable) {
		return nil, 0
	}
	return nil, syscall.EIO
}

// resolve returns the layers of rel (relative to the mount root).
func (u *UserOverlay) resolve(ctx context.Context, rel string) (layers, syscall.Errno) {
	if rel == "" {
		up, err := os.Stat(u.upper)
		if err != nil {
			return layers{}, syscall.EIO
		}
		low, errno := u.lowerStat(ctx, "")
		if errno != 0 {
			return layers{}, errno
		}
		if low != nil && low.Kind != Dir {
			low = nil
		}
		return layers{up: up, low: low, lowRaw: low}, 0
	}
	dir, name := splitRel(rel)
	par, errno := u.resolve(ctx, dir)
	if errno != 0 {
		return layers{}, errno
	}
	if !par.isDir() {
		return layers{}, syscall.ENOTDIR
	}
	var up os.FileInfo
	wh, opq := int64(-1), int64(-1)
	if par.up != nil {
		up, _ = os.Lstat(u.upperPath(rel))
		wh = markerTime(filepath.Join(u.upperPath(dir), WhiteoutPrefix+name))
		opq = markerTime(filepath.Join(u.upperPath(dir), OpaqueMarker))
	}
	var low *Node
	if par.low != nil && par.low.Kind == Dir {
		if low, errno = u.lowerStat(ctx, rel); errno != 0 {
			return layers{}, errno
		}
	}
	return u.combine(up, low, wh, opq), 0
}

func (u *UserOverlay) fill(l layers, out *fuse.Attr) {
	if l.up != nil {
		out.FromStat(l.up.Sys().(*syscall.Stat_t))
		out.Ino = 0
	} else {
		e := l.low
		out.Mode = kindBits(uint8(e.Kind)) | (e.Mode & 0o7777)
		out.Size = uint64(e.Size)
		if e.Kind == Symlink {
			out.Size = uint64(len(e.Target))
		}
		mt := time.Unix(0, e.MtimeNs)
		out.SetTimes(&mt, &mt, &mt)
		out.Owner = fuse.Owner{Uid: u.uid, Gid: u.gid}
		out.Nlink = 1
		if e.Kind == Dir {
			out.Nlink = 2
		}
	}
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
}

// ensureUpperDir creates the upper directory of rel (which must resolve
// to a directory) and its parents. Callers hold u.mu.
func (u *UserOverlay) ensureUpperDir(ctx context.Context, rel string) syscall.Errno {
	if rel == "" {
		return 0
	}
	l, errno := u.resolve(ctx, rel)
	if errno != 0 {
		return errno
	}
	if !l.isDir() {
		return syscall.ENOTDIR
	}
	if l.up != nil {
		return 0
	}
	if errno := u.ensureUpperDir(ctx, parent(rel)); errno != 0 {
		return errno
	}
	p := u.upperPath(rel)
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

func (u *UserOverlay) whiteoutPath(rel string) string {
	dir, name := splitRel(rel)
	return filepath.Join(u.upperPath(dir), WhiteoutPrefix+name)
}

// hideLower hides whatever the lower layer has at rel, now and in the
// current lower generation, and returns that entry (nil: none). The upper
// parent directory must exist. Callers hold u.mu.
func (u *UserOverlay) hideLower(ctx context.Context, rel string) (*Node, syscall.Errno) {
	low, errno := u.lowerStat(ctx, rel)
	if errno != 0 {
		return nil, errno
	}
	if low == nil {
		os.Remove(u.whiteoutPath(rel))
		return nil, 0
	}
	return low, writeMarker(u.whiteoutPath(rel), low.MtimeNs)
}

// prepareCreate readies the upper layer for a new entry at rel: parent
// directory, no dormant upper entry, and the lower entry (if any) hidden,
// which it returns. Callers hold u.mu.
func (u *UserOverlay) prepareCreate(ctx context.Context, rel string) (*Node, syscall.Errno) {
	if errno := u.ensureUpperDir(ctx, parent(rel)); errno != 0 {
		return nil, errno
	}
	l, errno := u.resolve(ctx, rel)
	if errno != 0 {
		return nil, errno
	}
	if l.exists() {
		return nil, syscall.EEXIST
	}
	if l.dormant {
		os.RemoveAll(u.upperPath(rel))
	}
	return u.hideLower(ctx, rel)
}

// copyUp makes rel exist in the upper layer with the visible content
// (empty if truncate). Callers hold u.mu.
func (u *UserOverlay) copyUp(ctx context.Context, rel string, truncate bool) syscall.Errno {
	l, errno := u.resolve(ctx, rel)
	if errno != 0 {
		return errno
	}
	if l.up != nil {
		return 0
	}
	if l.low == nil {
		return syscall.ENOENT
	}
	if l.low.Kind == Dir {
		return u.ensureUpperDir(ctx, rel)
	}
	dir, name := splitRel(rel)
	if errno := u.ensureUpperDir(ctx, dir); errno != 0 {
		return errno
	}
	dst := u.upperPath(rel)
	tmp := filepath.Join(u.upperPath(dir), internalPrefix+"tmp."+name)
	os.Remove(tmp)
	e := l.low
	switch e.Kind {
	case Symlink:
		if err := os.Symlink(e.Target, tmp); err != nil {
			return gofs.ToErrno(err)
		}
	default:
		src := ""
		if !truncate {
			p, err := u.lower.Fetch(ctx, e)
			if err != nil {
				u.log.Warn("overlay: cannot copy up lower file", "path", filepath.Join(u.mnt, rel), "err", err)
				return syscall.EIO
			}
			src = p
		}
		if err := copyFile(src, tmp, os.FileMode(e.Mode&0o7777)); err != nil {
			os.Remove(tmp)
			return gofs.ToErrno(err)
		}
	}
	// The copy keeps the lower mtime.
	ts, _ := unix.TimeToTimespec(time.Unix(0, e.MtimeNs))
	unix.UtimesNanoAt(unix.AT_FDCWD, tmp, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
	if l.dormant {
		os.RemoveAll(dst)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return gofs.ToErrno(err)
	}
	os.Remove(u.whiteoutPath(rel))
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

// readdir returns the visible entries of rel.
func (u *UserOverlay) readdir(ctx context.Context, rel string) (map[string]layers, syscall.Errno) {
	l, errno := u.resolve(ctx, rel)
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
		des, err := os.ReadDir(u.upperPath(rel))
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
			case name == OpaqueMarker:
				opq = fi.ModTime().UnixNano()
			case strings.HasPrefix(name, internalPrefix):
			case reserved(name):
				wh[strings.TrimPrefix(name, WhiteoutPrefix)] = fi.ModTime().UnixNano()
			default:
				ups[name] = fi
			}
		}
	}
	lows := map[string]*Node{}
	if l.low != nil && l.low.Kind == Dir {
		kids, err := u.lower.Children(rel)
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrUnavailable) && !errors.Is(err, syscall.ENOTDIR) {
			return nil, syscall.EIO
		}
		for _, k := range kids {
			lows[path.Base(k.Path)] = k
		}
	}
	out := map[string]layers{}
	add := func(name string) {
		if _, done := out[name]; done {
			return
		}
		w, ok := wh[name]
		if !ok {
			w = -1
		}
		if c := u.combine(ups[name], lows[name], w, opq); c.exists() {
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

// remove deletes rel from the merged view. Callers hold u.mu.
func (u *UserOverlay) remove(ctx context.Context, rel string, dir bool) syscall.Errno {
	l, errno := u.resolve(ctx, rel)
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
		m, errno := u.readdir(ctx, rel)
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
			err = os.RemoveAll(u.upperPath(rel)) // only markers are left
		} else {
			err = os.Remove(u.upperPath(rel))
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return gofs.ToErrno(err)
		}
	}
	if l.lowRaw != nil {
		if errno := u.ensureUpperDir(ctx, parent(rel)); errno != 0 {
			return errno
		}
		return writeMarker(u.whiteoutPath(rel), l.lowRaw.MtimeNs)
	}
	return 0
}

type unode struct {
	gofs.Inode
	u *UserOverlay
}

var (
	_ gofs.NodeLookuper   = (*unode)(nil)
	_ gofs.NodeGetattrer  = (*unode)(nil)
	_ gofs.NodeSetattrer  = (*unode)(nil)
	_ gofs.NodeReaddirer  = (*unode)(nil)
	_ gofs.NodeOpener     = (*unode)(nil)
	_ gofs.NodeCreater    = (*unode)(nil)
	_ gofs.NodeMkdirer    = (*unode)(nil)
	_ gofs.NodeUnlinker   = (*unode)(nil)
	_ gofs.NodeRmdirer    = (*unode)(nil)
	_ gofs.NodeRenamer    = (*unode)(nil)
	_ gofs.NodeLinker     = (*unode)(nil)
	_ gofs.NodeSymlinker  = (*unode)(nil)
	_ gofs.NodeReadlinker = (*unode)(nil)
	_ gofs.NodeStatfser   = (*unode)(nil)
)

// rel is the node's current path (renames move inodes).
func (n *unode) rel() string { return n.Path(n.u.root.EmbeddedInode()) }

func (n *unode) child(name string) string { return join(n.rel(), name) }

func (n *unode) newChild(ctx context.Context, name string, l layers, out *fuse.EntryOut) *gofs.Inode {
	n.u.fill(l, &out.Attr)
	if ch := n.GetChild(name); ch != nil && ch.StableAttr().Mode == l.mode() {
		return ch
	}
	return n.NewInode(ctx, &unode{u: n.u}, gofs.StableAttr{Mode: l.mode()})
}

// upperChild returns the layers of a just created upper entry.
func upperChild(p string) (layers, syscall.Errno) {
	fi, err := os.Lstat(p)
	if err != nil {
		return layers{}, gofs.ToErrno(err)
	}
	return layers{up: fi}, 0
}

func (n *unode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.ENOENT
	}
	l, errno := n.u.resolve(ctx, n.child(name))
	if errno != 0 {
		return nil, errno
	}
	if !l.exists() {
		return nil, syscall.ENOENT
	}
	return n.newChild(ctx, name, l, out), 0
}

func (n *unode) Getattr(ctx context.Context, fh gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if uf, ok := fh.(*upperFile); ok {
		errno := uf.Getattr(ctx, out)
		out.Ino = 0
		return errno
	}
	l, errno := n.u.resolve(ctx, n.rel())
	if errno != 0 {
		return errno
	}
	if !l.exists() {
		return syscall.ENOENT
	}
	n.u.fill(l, &out.Attr)
	return 0
}

func (n *unode) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if uf, ok := fh.(*upperFile); ok { // an open upper file
		errno := uf.Setattr(ctx, in, out)
		out.Ino = 0
		return errno
	}
	rel := n.rel()
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	sz, truncate := in.GetSize()
	if errno := n.u.copyUp(ctx, rel, truncate && sz == 0); errno != 0 {
		return errno
	}
	p := n.u.upperPath(rel)
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
	n.u.fill(l, &out.Attr)
	return 0
}

func (n *unode) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	m, errno := n.u.readdir(ctx, n.rel())
	if errno != 0 {
		return nil, errno
	}
	list := make([]fuse.DirEntry, 0, len(m))
	for name, l := range m {
		list = append(list, fuse.DirEntry{Name: name, Mode: l.mode()})
	}
	return gofs.NewListDirStream(list), 0
}

func (n *unode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	rel := n.rel()
	l, errno := n.u.resolve(ctx, rel)
	if errno != 0 {
		return nil, errno
	}
	switch {
	case l.up != nil:
		t, err := os.Readlink(n.u.upperPath(rel))
		if err != nil {
			return nil, syscall.EINVAL
		}
		return []byte(t), 0
	case l.low != nil && l.low.Kind == Symlink:
		return []byte(l.low.Target), 0
	}
	return nil, syscall.EINVAL
}

func (n *unode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(n.u.upper, &st); err != nil {
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

func (u *UserOverlay) openUpper(rel string, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	fd, err := syscall.Open(u.upperPath(rel), openFlags(flags)|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, gofs.ToErrno(err)
	}
	return u.handle(fd, flags&writeFlags != 0), 0, 0
}

// handle wraps an open upper file. Write opens never use passthrough.
func (u *UserOverlay) handle(fd int, write bool) gofs.FileHandle {
	lf := gofs.NewLoopbackFile(fd).(*gofs.LoopbackFile)
	if write || u.opt.NoPassthrough {
		return &upperFile{lf}
	}
	return &upperFilePT{upperFile{lf}}
}

func (n *unode) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	rel := n.rel()
	if flags&writeFlags != 0 {
		n.u.mu.Lock()
		errno := n.u.copyUp(ctx, rel, flags&syscall.O_TRUNC != 0)
		n.u.mu.Unlock()
		if errno != 0 {
			return nil, 0, errno
		}
		return n.u.openUpper(rel, flags)
	}
	for attempt := 0; ; attempt++ {
		l, errno := n.u.resolve(ctx, rel)
		if errno != 0 {
			return nil, 0, errno
		}
		if l.up != nil {
			return n.u.openUpper(rel, flags)
		}
		if l.low == nil {
			return nil, 0, syscall.ENOENT
		}
		if l.low.Kind == Dir {
			return nil, 0, syscall.EISDIR
		}
		p, err := n.u.lower.Fetch(ctx, l.low)
		if errors.Is(err, ErrStale) && attempt == 0 {
			continue // listed before a rebuild: list again
		}
		if err != nil {
			n.u.log.Warn("overlay: cannot read lower file", "path", filepath.Join(n.u.mnt, rel), "err", err)
			return nil, 0, syscall.EIO
		}
		fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, 0, syscall.EIO
		}
		lf := gofs.NewLoopbackFile(fd).(*gofs.LoopbackFile)
		if n.u.opt.NoPassthrough {
			return &lowerFile{lf}, 0, 0
		}
		return &lowerFilePT{lowerFile{lf}}, 0, 0
	}
}

func (n *unode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	if reserved(name) {
		return nil, nil, 0, syscall.EINVAL
	}
	rel := n.child(name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	if _, errno := n.u.prepareCreate(ctx, rel); errno != 0 {
		return nil, nil, 0, errno
	}
	p := n.u.upperPath(rel)
	fd, err := syscall.Open(p, openFlags(flags)|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, mode)
	if err != nil {
		return nil, nil, 0, gofs.ToErrno(err)
	}
	l, errno := upperChild(p)
	if errno != 0 {
		syscall.Close(fd)
		return nil, nil, 0, errno
	}
	return n.newChild(ctx, name, l, out), n.u.handle(fd, true), 0, 0
}

func (n *unode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.EINVAL
	}
	rel := n.child(name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	hidden, errno := n.u.prepareCreate(ctx, rel)
	if errno != 0 {
		return nil, errno
	}
	p := n.u.upperPath(rel)
	if err := os.Mkdir(p, os.FileMode(mode&0o777)); err != nil {
		return nil, gofs.ToErrno(err)
	}
	if hidden != nil {
		// Older lower children of the replaced entry stay hidden even
		// when a newer lower generation makes the directory visible again.
		if errno := writeMarker(filepath.Join(p, OpaqueMarker), hidden.MtimeNs); errno != 0 {
			return nil, errno
		}
	}
	l, errno := upperChild(p)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, name, l, out), 0
}

func (n *unode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.EINVAL
	}
	rel := n.child(name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	if _, errno := n.u.prepareCreate(ctx, rel); errno != 0 {
		return nil, errno
	}
	p := n.u.upperPath(rel)
	if err := os.Symlink(target, p); err != nil {
		return nil, gofs.ToErrno(err)
	}
	l, errno := upperChild(p)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, name, l, out), 0
}

func (n *unode) Link(ctx context.Context, target gofs.InodeEmbedder, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if reserved(name) {
		return nil, syscall.EINVAL
	}
	tn, ok := target.(*unode)
	if !ok {
		return nil, syscall.EXDEV
	}
	src, dst := tn.rel(), n.child(name)
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	sl, errno := n.u.resolve(ctx, src)
	if errno != 0 {
		return nil, errno
	}
	if sl.up == nil && sl.low != nil && sl.low.Kind == File {
		// cargo uplifts outputs (deps/x-HASH -> x) by unlinking and hard
		// linking even when they are fresh. When the lower layer has the
		// same link, dropping the whiteout restores it without a fetch.
		if dl, errno := n.u.resolve(ctx, dst); errno == 0 && !dl.exists() && !dl.dormant {
			if de, errno := n.u.lowerStat(ctx, dst); errno == 0 && de != nil && de.Kind == File &&
				de.Size == sl.low.Size && de.MtimeNs == sl.low.MtimeNs {
				os.Remove(n.u.whiteoutPath(dst))
				if dl, errno := n.u.resolve(ctx, dst); errno == 0 && dl.low != nil {
					return n.newChild(ctx, name, dl, out), 0
				}
			}
		}
	}
	if errno := n.u.copyUp(ctx, src, false); errno != 0 {
		return nil, errno
	}
	if _, errno := n.u.prepareCreate(ctx, dst); errno != 0 {
		return nil, errno
	}
	p := n.u.upperPath(dst)
	if err := os.Link(n.u.upperPath(src), p); err != nil {
		return nil, gofs.ToErrno(err)
	}
	l, errno := upperChild(p)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, name, l, out), 0
}

func (n *unode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	return n.u.remove(ctx, n.child(name), false)
}

func (n *unode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	return n.u.remove(ctx, n.child(name), true)
}

// Rename moves upper entries; lower files and symlinks are copied up
// first. Directories with lower content cannot be renamed (EXDEV, like
// overlayfs without redirect_dir: callers such as mv and rustc's
// incremental session finalization fall back to copying).
func (n *unode) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if reserved(newName) {
		return syscall.EINVAL
	}
	np, ok := newParent.(*unode)
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
	n.u.mu.Lock()
	defer n.u.mu.Unlock()
	sl, errno := n.u.resolve(ctx, src)
	if errno != 0 {
		return errno
	}
	if !sl.exists() {
		return syscall.ENOENT
	}
	if sl.isDir() && sl.lowRaw != nil {
		return syscall.EXDEV
	}
	dl, errno := n.u.resolve(ctx, dst)
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
		if errno := n.u.remove(ctx, dst, dl.isDir()); errno != 0 {
			return errno
		}
	}
	if errno := n.u.copyUp(ctx, src, false); errno != 0 {
		return errno
	}
	if _, errno := n.u.prepareCreate(ctx, dst); errno != 0 {
		return errno
	}
	if err := os.Rename(n.u.upperPath(src), n.u.upperPath(dst)); err != nil {
		return gofs.ToErrno(err)
	}
	if sl.lowRaw != nil {
		if errno := writeMarker(n.u.whiteoutPath(src), sl.lowRaw.MtimeNs); errno != 0 {
			return errno
		}
	}
	return 0
}

// upperFile is an open upper file. Only upper files serve getattr and
// setattr through their handle.
type upperFile struct{ *gofs.LoopbackFile }

// PassthroughFd hides the loopback file's passthrough.
func (h *upperFile) PassthroughFd() (int, bool) { return -1, false }

// upperFilePT is a read-only upper file served by kernel passthrough.
type upperFilePT struct{ upperFile }

func (h *upperFilePT) PassthroughFd() (int, bool) { return h.LoopbackFile.PassthroughFd() }

// lowerFile is a read-only open file of fetched lower content (e.g. a
// file of the content cache): attributes always come from the node.
type lowerFile struct{ *gofs.LoopbackFile }

func (h *lowerFile) PassthroughFd() (int, bool) { return -1, false }

type lowerFilePT struct{ lowerFile }

func (h *lowerFilePT) PassthroughFd() (int, bool) { return h.LoopbackFile.PassthroughFd() }

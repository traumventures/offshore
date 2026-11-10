package outputs

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
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
)

// FSType is the file system type of output mounts in /proc/self/mountinfo.
const FSType = "fuse.cloudsourced-outputs"

// FS is one mounted build directory (e.g. <repo>/target). It overlays the
// remote build slot over the local directory it is mounted on: remote
// entries win, local entries that do not exist remotely stay visible (for
// example outputs of earlier local builds). The mount is read-only, so a
// locally running cargo can never write into it.
type FS struct {
	tree       *Tree
	repo       string // repository root
	build      string // build directory relative to repo ("target")
	mountpoint string
	lowerFd    int
	lower      string // /proc/self/fd/N: the directory below the mount
	server     *fuse.Server
	root       *node
	log        *slog.Logger
	uid, gid   uint32
}

// Mount mounts the build directory build of repo at its local path. An
// existing local directory stays readable below the mount and is
// untouched after Unmount; a missing one is created.
func Mount(tree *Tree, repo, build string, log *slog.Logger) (*FS, error) {
	build = cleanDir(build)
	if build == "" {
		return nil, errors.New("outputs: empty build directory")
	}
	mp := filepath.Join(repo, filepath.FromSlash(build))
	if IsMounted(mp) {
		// Left over by a daemon that did not shut down cleanly.
		ForceUnmount(mp)
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(mp, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", mp, err)
	}
	f := &FS{tree: tree, repo: repo, build: build, mountpoint: mp, lowerFd: fd,
		lower: fmt.Sprintf("/proc/self/fd/%d", fd), log: log, uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
	f.root = &node{f: f}
	entry, attr, neg := 2*time.Second, 2*time.Second, time.Second
	opts := &gofs.Options{
		EntryTimeout: &entry, AttrTimeout: &attr, NegativeTimeout: &neg,
		UID: f.uid, GID: f.gid,
		MountOptions: fuse.MountOptions{
			FsName:       "cloudsourced",
			Name:         "cloudsourced-outputs",
			Options:      []string{"ro"},
			MaxReadAhead: 1 << 20,
		},
	}
	srv, err := gofs.Mount(mp, f.root, opts)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("mount %s: %w", mp, err)
	}
	f.server = srv
	tree.OnInvalidate(f.invalidate)
	return f, nil
}

// Mountpoint returns the local path of the mount.
func (f *FS) Mountpoint() string { return f.mountpoint }

// Unmount unmounts, lazily if the mount is busy (e.g. a proc macro library
// is still mapped by rust-analyzer).
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
		syscall.Close(f.lowerFd)
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

func (f *FS) lowerPath(inner string) string {
	if inner == "" {
		return f.lower
	}
	return f.lower + "/" + inner
}

// resolved is what a path inside the mount refers to.
type resolved struct {
	remote bool
	e      proto.OutEntry
	lower  os.FileInfo
}

func (f *FS) resolve(ctx context.Context, inner string) (resolved, syscall.Errno) {
	if inner != "" {
		e, err := f.tree.Stat(ctx, f.rel(inner))
		if err == nil {
			return resolved{remote: true, e: e}, 0
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrUnavailable) {
			return resolved{}, syscall.EIO
		}
	}
	stat := os.Lstat
	if inner == "" {
		stat = os.Stat // the mount root: follow /proc/self/fd/N
	}
	fi, err := stat(f.lowerPath(inner))
	if err != nil {
		return resolved{}, syscall.ENOENT
	}
	return resolved{lower: fi}, 0
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

func (r resolved) mode() uint32 {
	if r.remote {
		return kindMode(r.e.Kind)
	}
	switch {
	case r.lower.IsDir():
		return syscall.S_IFDIR
	case r.lower.Mode()&os.ModeSymlink != 0:
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

func (f *FS) fill(r resolved, out *fuse.Attr) {
	if r.remote {
		out.Mode = kindMode(r.e.Kind) | (r.e.Mode & 0o7777)
		out.Size = uint64(r.e.Size)
		if r.e.Kind == manifest.Symlink {
			out.Size = uint64(len(r.e.Target))
		}
		mt := time.Unix(0, r.e.MtimeNs)
		out.SetTimes(&mt, &mt, &mt)
		out.Owner = fuse.Owner{Uid: f.uid, Gid: f.gid}
	} else {
		st := r.lower.Sys().(*syscall.Stat_t)
		out.FromStat(st)
		out.Ino = 0
	}
	out.Nlink = 1
	if out.Mode&syscall.S_IFMT == syscall.S_IFDIR {
		out.Nlink = 2
	}
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
}

type node struct {
	gofs.Inode
	f *FS
}

var (
	_ gofs.NodeLookuper   = (*node)(nil)
	_ gofs.NodeGetattrer  = (*node)(nil)
	_ gofs.NodeReaddirer  = (*node)(nil)
	_ gofs.NodeOpener     = (*node)(nil)
	_ gofs.NodeReadlinker = (*node)(nil)
	_ gofs.NodeStatfser   = (*node)(nil)
)

func (n *node) inner() string { return n.Path(n.f.root.EmbeddedInode()) }

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	inner := path.Join(n.inner(), name)
	r, errno := n.f.resolve(ctx, inner)
	if errno != 0 {
		return nil, errno
	}
	n.f.fill(r, &out.Attr)
	if ch := n.GetChild(name); ch != nil && ch.StableAttr().Mode == r.mode() {
		return ch, 0
	}
	return n.NewInode(ctx, &node{f: n.f}, gofs.StableAttr{Mode: r.mode()}), 0
}

func (n *node) Getattr(ctx context.Context, fh gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	r, errno := n.f.resolve(ctx, n.inner())
	if errno != 0 {
		return errno
	}
	n.f.fill(r, &out.Attr)
	return 0
}

func (n *node) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	inner := n.inner()
	seen := map[string]bool{}
	var list []fuse.DirEntry
	if remote, err := n.f.tree.List(ctx, n.f.rel(inner)); err == nil {
		for name, e := range remote {
			seen[name] = true
			list = append(list, fuse.DirEntry{Name: name, Mode: kindMode(e.Kind)})
		}
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrUnavailable) {
		return nil, syscall.EIO
	}
	if des, err := os.ReadDir(n.f.lowerPath(inner)); err == nil {
		for _, de := range des {
			if seen[de.Name()] {
				continue
			}
			m := uint32(syscall.S_IFREG)
			switch {
			case de.IsDir():
				m = syscall.S_IFDIR
			case de.Type()&os.ModeSymlink != 0:
				m = syscall.S_IFLNK
			}
			list = append(list, fuse.DirEntry{Name: de.Name(), Mode: m})
		}
	}
	return gofs.NewListDirStream(list), 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	inner := n.inner()
	r, errno := n.f.resolve(ctx, inner)
	if errno != 0 {
		return nil, errno
	}
	if r.remote {
		if r.e.Kind != manifest.Symlink {
			return nil, syscall.EINVAL
		}
		return []byte(r.e.Target), 0
	}
	t, err := os.Readlink(n.f.lowerPath(inner))
	if err != nil {
		return nil, syscall.EINVAL
	}
	return []byte(t), 0
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(n.f.lowerFd, &st); err != nil {
		return syscall.EIO
	}
	out.FromStatfsT(&st)
	return 0
}

const writeFlags = syscall.O_WRONLY | syscall.O_RDWR | syscall.O_TRUNC | syscall.O_APPEND

func (n *node) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	if flags&writeFlags != 0 {
		return nil, 0, syscall.EROFS
	}
	inner := n.inner()
	for attempt := 0; ; attempt++ {
		r, errno := n.f.resolve(ctx, inner)
		if errno != 0 {
			return nil, 0, errno
		}
		src := n.f.lowerPath(inner)
		if r.remote {
			if r.e.Kind == manifest.Dir {
				return nil, 0, syscall.EISDIR
			}
			p, err := n.f.tree.Content(ctx, n.f.rel(inner), r.e)
			if errors.Is(err, ErrStale) && attempt == 0 {
				continue // listed before a rebuild: list again
			}
			if err != nil {
				n.f.log.Warn("outputs: cannot read remote output", "path", filepath.Join(n.f.mountpoint, inner), "err", err)
				return nil, 0, syscall.EIO
			}
			src = p
		}
		fh, err := os.Open(src)
		if err != nil {
			return nil, 0, syscall.EIO
		}
		return &handle{f: fh}, 0, 0
	}
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

package anyfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Slot mounts without FUSE in the warm path.
//
// A FUSE lower under a kernel overlay costs a daemon round trip for
// every lookup the kernel has not cached and for every open (passthrough
// attaches the backing file at open), which under load made incremental
// builds 2-3x slower than on a local file system. Instead the overlay
// stacks three lower layers, none of which needs the daemon for resident
// content:
//
//	meta   a local directory with the tree's metadata: directories,
//	       symlinks, empty files, and every other file as a sparse
//	       metacopy file (mode, size, mtime) whose absolute
//	       trusted.overlay.redirect names its content by digest
//	files  data-only layer: the materialized files (Files, by digest)
//	lazy   data-only layer: a LowerFS in digest mode (DigestSource) that
//	       faults in content that is not (or no longer) resident
//
// overlayfs looks a metacopy file's data up in the data-only layers in
// order, so resident content is a local file and only misses reach FUSE.

// digestRel is the path of content digest d in a data-only layer (the
// layout of Files).
func digestRel(d string) string { return d[:2] + "/" + d }

const (
	xattrMetacopy = "trusted.overlay.metacopy"
	xattrRedirect = "trusted.overlay.redirect"
)

// WriteMetaLayer writes the metadata of t into dir (created; it must not
// exist) as the metadata lower layer of a slot overlay. Writing the
// xattrs needs CAP_SYS_ADMIN.
func WriteMetaLayer(dir string, t *Tree) error {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	nodes := t.Nodes()
	// Directories first (sorted by path: parents before children).
	for i := range nodes {
		n := &nodes[i]
		if n.Kind == Dir {
			if err := os.Mkdir(filepath.Join(dir, n.Path), 0o755); err != nil {
				return err
			}
		}
	}
	work := make(chan *Node, 256)
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		first error
	)
	for range max(4, runtime.NumCPU()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				if err := writeMetaNode(dir, n); err != nil {
					errMu.Lock()
					if first == nil {
						first = err
					}
					errMu.Unlock()
				}
			}
		}()
	}
	for i := range nodes {
		if nodes[i].Kind != Dir {
			work <- &nodes[i]
		}
	}
	close(work)
	wg.Wait()
	if first != nil {
		return first
	}
	// Directory modes and mtimes last, deepest first (creating children
	// changes the parent's mtime).
	var dirs []*Node
	for i := range nodes {
		if nodes[i].Kind == Dir {
			dirs = append(dirs, &nodes[i])
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i].Path, "/") > strings.Count(dirs[j].Path, "/") })
	for _, n := range dirs {
		p := filepath.Join(dir, n.Path)
		if err := os.Chmod(p, os.FileMode(n.Mode&0o7777)); err != nil {
			return err
		}
		if err := setMtime(p, n.MtimeNs, 0); err != nil {
			return err
		}
	}
	return nil
}

func writeMetaNode(dir string, n *Node) error {
	p := filepath.Join(dir, n.Path)
	switch n.Kind {
	case Symlink:
		if err := os.Symlink(n.Target, p); err != nil {
			return err
		}
		return setMtime(p, n.MtimeNs, unix.AT_SYMLINK_NOFOLLOW)
	case File:
	default:
		return fmt.Errorf("anyfs: meta layer: %s: unsupported kind %d", n.Path, n.Kind)
	}
	fd, err := unix.Open(p, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, n.Mode&0o7777)
	if err != nil {
		return &os.PathError{Op: "create", Path: p, Err: err}
	}
	defer unix.Close(fd)
	if d := digestOf(n); n.Size > 0 && d != "" {
		if err := unix.Ftruncate(fd, n.Size); err != nil {
			return &os.PathError{Op: "truncate", Path: p, Err: err}
		}
		if err := unix.Fsetxattr(fd, xattrMetacopy, nil, 0); err != nil {
			return &os.PathError{Op: "setxattr", Path: p, Err: err}
		}
		if err := unix.Fsetxattr(fd, xattrRedirect, []byte("/"+digestRel(d)), 0); err != nil {
			return &os.PathError{Op: "setxattr", Path: p, Err: err}
		}
	}
	// The create mode is masked by the umask.
	if err := unix.Fchmod(fd, n.Mode&0o7777); err != nil {
		return &os.PathError{Op: "chmod", Path: p, Err: err}
	}
	ts := unix.NsecToTimespec(n.MtimeNs)
	return unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), []unix.Timespec{ts, ts}, 0)
}

func setMtime(p string, ns int64, flags int) error {
	ts := unix.NsecToTimespec(ns)
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, []unix.Timespec{ts, ts}, flags); err != nil {
		return &os.PathError{Op: "utimes", Path: p, Err: err}
	}
	return nil
}

// DigestSource serves the content of a TreeSource by digest
// ("ab/abcd...", the layout of Files) instead of by path: the lazy
// data-only layer of a slot overlay. Metadata other than the size comes
// from the metadata layer.
type DigestSource struct {
	*TreeSource
	mu    sync.Mutex
	tree  *Tree
	index map[string]*Node
}

// NewDigestSource returns a digest view of s.
func NewDigestSource(s *TreeSource) *DigestSource { return &DigestSource{TreeSource: s} }

func (s *DigestSource) lookup(d string) (*Node, bool) {
	t := s.Tree()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tree != t {
		s.index = map[string]*Node{}
		ns := t.Nodes()
		for i := range ns {
			if n := &ns[i]; n.Kind == File && n.Size > 0 && n.Digest != "" {
				s.index[n.Digest] = n
			}
		}
		s.tree = t
	}
	n, ok := s.index[d]
	return n, ok
}

// Node implements Source.
func (s *DigestSource) Node(rel string) (*Node, bool) {
	if rel == "" {
		return &rootNode, true
	}
	pre, d, ok := strings.Cut(rel, "/")
	if len(pre) != 2 {
		return nil, false
	}
	if !ok {
		return &Node{Path: rel, Kind: Dir, Mode: 0o755}, true
	}
	n, found := s.lookup(d)
	if !found || d[:2] != pre {
		return nil, false
	}
	c := *n
	c.Path = rel
	return &c, true
}

// Children implements Source: the digest view is not listable (the
// overlay only looks names up).
func (s *DigestSource) Children(dir string) ([]*Node, error) {
	if _, ok := s.Node(dir); !ok {
		return nil, syscall.ENOENT
	}
	return nil, nil
}

// SlotOptions configure MountSlot.
type SlotOptions struct {
	// Lower configures the lazy FUSE layer (FsName, PrefetchMax, ...).
	Lower LowerOptions
}

// Slot is a mounted slot overlay (MountSlot).
type Slot struct {
	Target string
	Meta   string
	Lazy   *LowerFS
}

// MountSlot mounts a kernel overlay of a build slot at target: upper and
// work over the metadata layer written to meta (replaced if present)
// and the data-only layers of src's materialized files and a digest-mode
// LowerFS at lazy. It needs CAP_SYS_ADMIN.
func MountSlot(ctx context.Context, target, upper, work, meta, lazy string, src *TreeSource, opt SlotOptions) (*Slot, error) {
	if src.files == nil {
		return nil, errors.New("anyfs: slot source without files")
	}
	if err := os.RemoveAll(meta); err != nil {
		return nil, err
	}
	if err := WriteMetaLayer(meta, src.Tree()); err != nil {
		os.RemoveAll(meta)
		return nil, fmt.Errorf("meta layer: %w", err)
	}
	l, err := MountLower(lazy, NewDigestSource(src), opt.Lower)
	if err != nil {
		os.RemoveAll(meta)
		return nil, err
	}
	if err := MountKernelOverlayData(target, upper, work, []string{meta}, []string{src.files.Dir(), lazy}); err != nil {
		l.Unmount()
		os.RemoveAll(meta)
		return nil, err
	}
	return &Slot{Target: target, Meta: meta, Lazy: l}, nil
}

// Unmount unmounts the overlay and the lazy layer (lazily if busy). The
// metadata layer stays (a detached overlay may still use it); the next
// MountSlot replaces it.
func (s *Slot) Unmount() error {
	err := Unmount(s.Target)
	if e := s.Lazy.Unmount(); err == nil {
		err = e
	}
	return err
}

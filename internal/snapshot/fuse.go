package snapshot

import (
	"context"
	"os"
	"path"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// The FUSE fill layer: a loopback file system over the local directory
// that fills a pending file before it is opened, truncated or renamed. It
// is mounted while a restore is incomplete; afterwards the directory is
// used directly.

type fillNode struct {
	fs.LoopbackNode
	f *Filler
}

var (
	_ fs.NodeOpener    = (*fillNode)(nil)
	_ fs.NodeSetattrer = (*fillNode)(nil)
	_ fs.NodeUnlinker  = (*fillNode)(nil)
	_ fs.NodeRenamer   = (*fillNode)(nil)
	_ fs.NodeCreater   = (*fillNode)(nil)
)

func (n *fillNode) rel() string { return n.Path(nil) }

func (n *fillNode) child(name string) string { return path.Join(n.Path(nil), name) }

func (n *fillNode) fill(ctx context.Context, rel string) syscall.Errno {
	if err := n.f.Fill(ctx, rel); err != nil {
		return syscall.EIO
	}
	return 0
}

func (n *fillNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_TRUNC != 0 {
		n.f.Forget(n.rel()) // the content is replaced
	} else if e := n.fill(ctx, n.rel()); e != 0 {
		return nil, 0, e
	}
	return n.LoopbackNode.Open(ctx, flags)
}

func (n *fillNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if sz, ok := in.GetSize(); ok && sz == 0 {
		n.f.Forget(n.rel())
	} else if e := n.fill(ctx, n.rel()); e != 0 {
		return e
	}
	return n.LoopbackNode.Setattr(ctx, fh, in, out)
}

func (n *fillNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.f.Forget(n.child(name))
	return n.LoopbackNode.Unlink(ctx, name)
}

func (n *fillNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if e := n.fill(ctx, n.child(name)); e != 0 {
		return e
	}
	if p, ok := newParent.(*fillNode); ok {
		n.f.Forget(p.child(newName))
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}

func (n *fillNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := n.child(name)
	if flags&syscall.O_TRUNC != 0 {
		n.f.Forget(rel)
	} else if e := n.fill(ctx, rel); e != 0 {
		return nil, nil, 0, e
	}
	return n.LoopbackNode.Create(ctx, name, flags, mode, out)
}

// Mount mounts the fill layer of f at mountpoint (over f's directory) and
// returns the function that unmounts it.
func Mount(mountpoint string, f *Filler) (func() error, error) {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(f.dir, &st); err != nil {
		return nil, err
	}
	root := &fs.LoopbackRoot{Path: f.dir, Dev: uint64(st.Dev)}
	root.NewNode = func(r *fs.LoopbackRoot, _ *fs.Inode, _ string, _ *syscall.Stat_t) fs.InodeEmbedder {
		return &fillNode{LoopbackNode: fs.LoopbackNode{RootData: r}, f: f}
	}
	rootNode := root.NewNode(root, nil, "", &st)
	root.RootNode = rootNode
	sec := time.Second
	srv, err := fs.Mount(mountpoint, rootNode, &fs.Options{
		EntryTimeout: &sec, AttrTimeout: &sec,
		MountOptions: fuse.MountOptions{AllowOther: true, DirectMount: true, FsName: "cloudsourced-fill", Name: "cloudsourced-fill", MaxWrite: 1 << 20},
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		err := srv.Unmount()
		srv.Wait()
		return err
	}, nil
}

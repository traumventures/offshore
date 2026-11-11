package anyfs

import (
	"sync/atomic"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// OpStat counts the requests of one FUSE operation that reached the
// daemon (everything the kernel did not answer from its caches or by
// passthrough) and the time spent serving them.
type OpStat struct {
	Count   int64   `json:"count"`
	Seconds float64 `json:"seconds"`
}

const (
	opLookup = iota
	opForget
	opGetattr
	opReadlink
	opAccess
	opGetxattr
	opListxattr
	opOpen
	opRead
	opLseek
	opRelease
	opFlush
	opFsync
	opOpendir
	opReaddir
	opReaddirplus
	opReleasedir
	opStatfs
	opStatx
	opIoctl
	numOps
)

var opNames = [numOps]string{"lookup", "forget", "getattr", "readlink", "access", "getxattr", "listxattr", "open", "read",
	"lseek", "release", "flush", "fsync", "opendir", "readdir", "readdirplus", "releasedir", "statfs", "statx", "ioctl"}

// opCounter wraps a read-only RawFileSystem and keeps an operation
// histogram (count and service time per op type).
type opCounter struct {
	fuse.RawFileSystem
	n  [numOps]atomic.Int64
	ns [numOps]atomic.Int64
}

func (c *opCounter) done(op int, t time.Time) {
	c.n[op].Add(1)
	c.ns[op].Add(int64(time.Since(t)))
}

// Ops returns the histogram: op name -> count and seconds (ops never
// seen are omitted).
func (c *opCounter) Ops() map[string]OpStat {
	out := map[string]OpStat{}
	for i := range numOps {
		if k := c.n[i].Load(); k > 0 {
			out[opNames[i]] = OpStat{Count: k, Seconds: float64(c.ns[i].Load()) / 1e9}
		}
	}
	return out
}

func (c *opCounter) Lookup(x <-chan struct{}, h *fuse.InHeader, name string, o *fuse.EntryOut) fuse.Status {
	defer c.done(opLookup, time.Now())
	return c.RawFileSystem.Lookup(x, h, name, o)
}
func (c *opCounter) Forget(nodeid, nlookup uint64) {
	defer c.done(opForget, time.Now())
	c.RawFileSystem.Forget(nodeid, nlookup)
}
func (c *opCounter) GetAttr(x <-chan struct{}, i *fuse.GetAttrIn, o *fuse.AttrOut) fuse.Status {
	defer c.done(opGetattr, time.Now())
	return c.RawFileSystem.GetAttr(x, i, o)
}
func (c *opCounter) Readlink(x <-chan struct{}, h *fuse.InHeader) ([]byte, fuse.Status) {
	defer c.done(opReadlink, time.Now())
	return c.RawFileSystem.Readlink(x, h)
}
func (c *opCounter) Access(x <-chan struct{}, i *fuse.AccessIn) fuse.Status {
	defer c.done(opAccess, time.Now())
	return c.RawFileSystem.Access(x, i)
}
func (c *opCounter) GetXAttr(x <-chan struct{}, h *fuse.InHeader, a string, d []byte) (uint32, fuse.Status) {
	defer c.done(opGetxattr, time.Now())
	return c.RawFileSystem.GetXAttr(x, h, a, d)
}
func (c *opCounter) ListXAttr(x <-chan struct{}, h *fuse.InHeader, d []byte) (uint32, fuse.Status) {
	defer c.done(opListxattr, time.Now())
	return c.RawFileSystem.ListXAttr(x, h, d)
}
func (c *opCounter) Open(x <-chan struct{}, i *fuse.OpenIn, o *fuse.OpenOut) fuse.Status {
	defer c.done(opOpen, time.Now())
	return c.RawFileSystem.Open(x, i, o)
}
func (c *opCounter) Read(x <-chan struct{}, i *fuse.ReadIn, b []byte) (fuse.ReadResult, fuse.Status) {
	defer c.done(opRead, time.Now())
	return c.RawFileSystem.Read(x, i, b)
}
func (c *opCounter) Lseek(x <-chan struct{}, i *fuse.LseekIn, o *fuse.LseekOut) fuse.Status {
	defer c.done(opLseek, time.Now())
	return c.RawFileSystem.Lseek(x, i, o)
}
func (c *opCounter) Release(x <-chan struct{}, i *fuse.ReleaseIn) {
	defer c.done(opRelease, time.Now())
	c.RawFileSystem.Release(x, i)
}
func (c *opCounter) Flush(x <-chan struct{}, i *fuse.FlushIn) fuse.Status {
	defer c.done(opFlush, time.Now())
	return c.RawFileSystem.Flush(x, i)
}
func (c *opCounter) Fsync(x <-chan struct{}, i *fuse.FsyncIn) fuse.Status {
	defer c.done(opFsync, time.Now())
	return c.RawFileSystem.Fsync(x, i)
}
func (c *opCounter) OpenDir(x <-chan struct{}, i *fuse.OpenIn, o *fuse.OpenOut) fuse.Status {
	defer c.done(opOpendir, time.Now())
	return c.RawFileSystem.OpenDir(x, i, o)
}
func (c *opCounter) ReadDir(x <-chan struct{}, i *fuse.ReadIn, o *fuse.DirEntryList) fuse.Status {
	defer c.done(opReaddir, time.Now())
	return c.RawFileSystem.ReadDir(x, i, o)
}
func (c *opCounter) ReadDirPlus(x <-chan struct{}, i *fuse.ReadIn, o *fuse.DirEntryList) fuse.Status {
	defer c.done(opReaddirplus, time.Now())
	return c.RawFileSystem.ReadDirPlus(x, i, o)
}
func (c *opCounter) ReleaseDir(i *fuse.ReleaseIn) {
	defer c.done(opReleasedir, time.Now())
	c.RawFileSystem.ReleaseDir(i)
}
func (c *opCounter) StatFs(x <-chan struct{}, h *fuse.InHeader, o *fuse.StatfsOut) fuse.Status {
	defer c.done(opStatfs, time.Now())
	return c.RawFileSystem.StatFs(x, h, o)
}
func (c *opCounter) Statx(x <-chan struct{}, i *fuse.StatxIn, o *fuse.StatxOut) fuse.Status {
	defer c.done(opStatx, time.Now())
	return c.RawFileSystem.Statx(x, i, o)
}
func (c *opCounter) Ioctl(x <-chan struct{}, i *fuse.IoctlIn, ib []byte, o *fuse.IoctlOut, ob []byte) fuse.Status {
	defer c.done(opIoctl, time.Now())
	return c.RawFileSystem.Ioctl(x, i, ib, o, ob)
}

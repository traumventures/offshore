package anyfs

import (
	"container/list"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/zeebo/blake3"
	"golang.org/x/sys/unix"
)

// Files materializes the contents of tree nodes as whole local files,
// keyed by content digest (identical files share one), assembled from
// the pages of a tier on first use. These read-only files back the FUSE
// lower: opened files are handed to the kernel (passthrough). They are
// resident data too: Reclaim drops the least recently used ones that
// are not open; a later read faults them in again.
type Files struct {
	dir string
	obs Observer

	mu       sync.Mutex
	files    map[string]*mfile
	lru      *list.List
	resident int64
	limit    int64 // reclaim to below this after a fault (0: unbounded)
	inflight map[string]*fetch
	faults   int64
	faultB   int64
	rate     rate
	// pins counts holders per digest: pinned contents are never
	// reclaimed (e.g. outputs not yet committed anywhere else).
	pins map[string]int

	pmu    sync.Mutex
	pages  map[Key][]byte // last pages assembled (packed small files share them)
	porder []Key
}

type mfile struct {
	digest string
	size   int64
	open   int
	// backed files can be faulted in again after reclaim (their pages
	// are in a lower tier); others are the only copy and never reclaimed.
	backed bool
	elem   *list.Element
}

// OpenFiles opens the materialized file cache in dir (files found in dir
// are resident and unbacked until SetBacked).
func OpenFiles(dir string, obs Observer) (*Files, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f := &Files{dir: dir, obs: obs, files: map[string]*mfile{}, lru: list.New(),
		inflight: map[string]*fetch{}, pages: map[Key][]byte{}, pins: map[string]int{}}
	type found struct {
		d   string
		sz  int64
		mod time.Time
	}
	var fs_ []found
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !Key(d.Name()).Valid() {
			os.Remove(p)
			return nil
		}
		if fi, err := d.Info(); err == nil {
			if fi.Size() == 0 && d.Name() != emptyDigest {
				// Emptied by a crash before its data reached the disk
				// (renamed into place without fsync): fetched again.
				os.Remove(p)
				return nil
			}
			fs_ = append(fs_, found{d.Name(), fi.Size(), fi.ModTime()})
		}
		return nil
	})
	sort.Slice(fs_, func(i, j int) bool { return fs_[i].mod.Before(fs_[j].mod) })
	for _, x := range fs_ {
		m := &mfile{digest: x.d, size: x.sz}
		m.elem = f.lru.PushBack(m)
		f.files[x.d] = m
		f.resident += x.sz
	}
	return f, nil
}

// SetBacked marks a resident file as backed by a lower tier (it may be
// reclaimed and faulted in again).
func (f *Files) SetBacked(digest string) {
	f.mu.Lock()
	if m := f.files[digest]; m != nil {
		m.backed = true
	}
	f.mu.Unlock()
}

// Dir returns the directory of the materialized files (by digest,
// "ab/abcd...": the files data-only layer of a slot overlay).
func (f *Files) Dir() string { return f.dir }

// MarkAllBacked marks every resident file as backed: a cache whose
// contents can always be faulted in again (the source projection, where
// the laptop holds every file of the tree).
func (f *Files) MarkAllBacked() {
	f.mu.Lock()
	for _, m := range f.files {
		m.backed = true
	}
	f.mu.Unlock()
}

// SetLimit caps the resident bytes: after each fault the least recently
// used backed files that are not open are reclaimed down to 90% of max
// (0: unbounded).
func (f *Files) SetLimit(max int64) {
	f.mu.Lock()
	f.limit = max
	f.mu.Unlock()
	f.reclaimToLimit("")
}

// Limit returns the resident byte cap (0: unbounded).
func (f *Files) Limit() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limit
}

// Pin keeps a node's content resident (never reclaimed) until a matching
// Unpin; pins are counted per digest and may precede the content.
func (f *Files) Pin(n *Node) {
	d := digestOf(n)
	if d == "" {
		return
	}
	f.mu.Lock()
	f.pins[d]++
	f.mu.Unlock()
}

// Unpin ends a Pin.
func (f *Files) Unpin(n *Node) {
	d := digestOf(n)
	f.mu.Lock()
	if f.pins[d] <= 1 {
		delete(f.pins, d)
	} else {
		f.pins[d]--
	}
	f.mu.Unlock()
	f.reclaimToLimit("")
}

// Pinned returns the number of pinned digests and the resident bytes of
// their contents.
func (f *Files) Pinned() (n int, bytes int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for d := range f.pins {
		n++
		if m := f.files[d]; m != nil {
			bytes += m.size
		}
	}
	return n, bytes
}

// reclaimToLimit reclaims down to 90% of the limit, keeping the content
// of keep (just faulted in for a caller about to open it).
func (f *Files) reclaimToLimit(keep string) {
	f.mu.Lock()
	limit, res := f.limit, f.resident
	f.mu.Unlock()
	if limit > 0 && res > limit {
		f.reclaim(limit/10*9, keep)
	}
}

func (f *Files) path(digest string) string {
	return filepath.Join(f.dir, digest[:2], digest)
}

// emptyDigest is the BLAKE3 of no bytes.
var emptyDigest = string(KeyOf(nil))

func digestOf(n *Node) string {
	if n.Digest == "" && n.Size == 0 {
		return emptyDigest
	}
	return n.Digest
}

// Local returns the path of a node's materialized content if resident.
func (f *Files) Local(n *Node) (string, bool) {
	d := digestOf(n)
	if d == "" {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.files[d]
	if m == nil {
		return "", false
	}
	f.lru.MoveToBack(m.elem)
	return f.path(d), true
}

// Acquire marks a materialized file open (not reclaimed until Release).
func (f *Files) Acquire(n *Node) {
	f.mu.Lock()
	if m := f.files[digestOf(n)]; m != nil {
		m.open++
	}
	f.mu.Unlock()
}

// Open opens a node's resident content and marks it open (not reclaimed
// until Release); false if it is not resident (e.g. reclaimed since the
// fault).
func (f *Files) Open(n *Node) (*os.File, bool) {
	d := digestOf(n)
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.files[d]
	if m == nil {
		return nil, false
	}
	fh, err := os.Open(f.path(d))
	if err != nil {
		return nil, false
	}
	m.open++
	f.lru.MoveToBack(m.elem)
	return fh, true
}

// Release ends an Acquire.
func (f *Files) Release(n *Node) {
	f.mu.Lock()
	if m := f.files[digestOf(n)]; m != nil && m.open > 0 {
		m.open--
	}
	f.mu.Unlock()
}

// Ensure materializes a node's content from the pages of tier (a page
// fault if it is not resident) and returns its path. Concurrent calls
// share one fetch.
func (f *Files) Ensure(ctx context.Context, tier Tier, n *Node) (string, error) {
	return f.fault(ctx, n, func(d string) error {
		if tier == nil {
			return fmt.Errorf("fault %s: %w", n.Path, ErrNotFound)
		}
		return f.assemble(ctx, tier, n, d)
	})
}

// FileFetcher writes the whole content of a node to w: a source that
// serves files by content digest rather than pages (the laptop over
// QUIC for the source projection).
type FileFetcher func(ctx context.Context, n *Node, w io.Writer) error

// ErrRetryFetch, wrapped in a FileFetcher's error, asks EnsureWith to
// discard what the fetcher wrote and call it again (the transfer broke
// midway and the source is available again).
var ErrRetryFetch = errors.New("retry fetch")

// maxFetchRestarts bounds the restarts after ErrRetryFetch.
const maxFetchRestarts = 3

// EnsureWith materializes a node's content with fetch (a page fault of
// the whole file) and returns its path. The content is verified against
// the node's digest; concurrent calls share one fetch.
//
// A full cache disk must not fail the read of a source file (in
// production cargo reported "failed to load manifest for workspace
// member" with ENOSPC, a misleading errno for a read): the fault reclaims
// the unopened contents of this cache once and retries; only then the
// read fails, with EIO.
func (f *Files) EnsureWith(ctx context.Context, n *Node, fetch FileFetcher) (string, error) {
	p, err := f.ensureWith(ctx, n, fetch)
	if err != nil && errors.Is(err, syscall.ENOSPC) && ctx.Err() == nil {
		f.Reclaim(0)
		p, err = f.ensureWith(ctx, n, fetch)
	}
	if err != nil && errors.Is(err, syscall.ENOSPC) {
		err = fmt.Errorf("%w (cache disk full: %v)", syscall.EIO, err)
	}
	return p, err
}

func (f *Files) ensureWith(ctx context.Context, n *Node, fetch FileFetcher) (string, error) {
	return f.fault(ctx, n, func(d string) error {
		p := f.path(d)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		tmp := fmt.Sprintf("%s.%d.tmp", p, time.Now().UnixNano())
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o444)
		if err != nil {
			return err
		}
		var cw *countWriter
		for restart := 0; ; restart++ {
			h := blake3.New()
			cw = &countWriter{w: io.MultiWriter(out, h)}
			err = fetch(ctx, n, cw)
			if err != nil && errors.Is(err, ErrRetryFetch) && restart < maxFetchRestarts && ctx.Err() == nil {
				if err = out.Truncate(0); err == nil {
					_, err = out.Seek(0, io.SeekStart)
				}
				if err == nil {
					continue
				}
			}
			if err == nil {
				if got := hex.EncodeToString(h.Sum(nil)); got != d {
					err = fmt.Errorf("content of %s changed: want %s, got %s", n.Path, Key(d).Short(), Key(got).Short())
				}
			}
			break
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("fault %s: %w", n.Path, err)
		}
		return f.add(tmp, d, cw.n, true)
	})
}

// fault runs fill (which must leave the content at f.path(d)) unless the
// node is resident, sharing concurrent faults of the same digest.
func (f *Files) fault(ctx context.Context, n *Node, fill func(d string) error) (string, error) {
	if p, ok := f.Local(n); ok {
		return p, nil
	}
	d := digestOf(n)
	if d == "" {
		return "", fmt.Errorf("anyfs: %s has no content digest", n.Path)
	}
	f.mu.Lock()
	if fl := f.inflight[d]; fl != nil {
		f.mu.Unlock()
		select {
		case <-fl.done:
			if fl.err != nil {
				return "", fl.err
			}
			return f.path(d), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	fl := &fetch{done: make(chan struct{})}
	f.inflight[d] = fl
	f.mu.Unlock()
	begin := time.Now()
	fl.err = fill(d)
	f.mu.Lock()
	delete(f.inflight, d)
	if fl.err == nil {
		f.faults++
		f.faultB += n.Size
	}
	f.mu.Unlock()
	close(fl.done)
	if fl.err != nil {
		return "", fl.err
	}
	f.rate.add(time.Now(), 1)
	f.obs.emit(Event{Type: EvPageFault, Tier: "files", Path: n.Path, Pages: int64(len(n.Pages)), Bytes: n.Size, Seconds: time.Since(begin).Seconds()})
	f.reclaimToLimit(d)
	return f.path(d), nil
}

func (f *Files) assemble(ctx context.Context, tier Tier, n *Node, d string) error {
	p := f.path(d)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", p, time.Now().UnixNano())
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o444)
	if err != nil {
		return err
	}
	var off int64
	for _, r := range n.Pages {
		b, err := f.page(ctx, tier, r.Key)
		if err == nil && r.Off+r.Len > int64(len(b)) {
			err = fmt.Errorf("page %s too short for %s", r.Key.Short(), n.Path)
		}
		if err == nil {
			_, err = out.WriteAt(b[r.Off:r.Off+r.Len], off)
		}
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return fmt.Errorf("fault %s: %w", n.Path, err)
		}
		off += r.Len
	}
	if off != n.Size {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("fault %s: pages hold %d of %d bytes", n.Path, off, n.Size)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return f.add(tmp, d, n.Size, true)
}

// add renames a complete temporary file into place.
func (f *Files) add(tmp, d string, size int64, backed bool) error {
	if err := os.Rename(tmp, f.path(d)); err != nil {
		os.Remove(tmp)
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files[d] == nil {
		m := &mfile{digest: d, size: size, backed: backed}
		m.elem = f.lru.PushBack(m)
		f.files[d] = m
		f.resident += size
	} else if backed {
		f.files[d].backed = true
	}
	return nil
}

// page returns a page, keeping the last few in memory: the small files
// of one pack are usually read together.
func (f *Files) page(ctx context.Context, tier Tier, k Key) ([]byte, error) {
	f.pmu.Lock()
	if b, ok := f.pages[k]; ok {
		f.pmu.Unlock()
		return b, nil
	}
	f.pmu.Unlock()
	b, err := tier.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	f.pmu.Lock()
	if _, ok := f.pages[k]; !ok {
		f.pages[k] = b
		f.porder = append(f.porder, k)
		for len(f.porder) > 4 {
			delete(f.pages, f.porder[0])
			f.porder = f.porder[1:]
		}
	}
	f.pmu.Unlock()
	return b, nil
}

// Adopt makes the content of a local file (e.g. just committed from an
// upper directory) resident for node n without fetching it: a reflink
// clone where the file system supports it, a copy otherwise (never a
// hardlink: the source may be written in place later). With move the
// source is renamed instead (it must not be used afterwards).
func (f *Files) Adopt(n *Node, src string, backed, move bool) error {
	d := digestOf(n)
	if d == "" {
		return nil
	}
	if _, ok := f.Local(n); ok {
		return nil
	}
	p := f.path(d)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", p, time.Now().UnixNano())
	if move {
		if err := os.Rename(src, tmp); err != nil {
			move = false
		}
	}
	if !move {
		if err := cloneFile(src, tmp); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() != n.Size {
		os.Remove(tmp)
		return fmt.Errorf("adopt %s: changed while copying", n.Path)
	}
	os.Chmod(tmp, 0o444)
	return f.add(tmp, d, n.Size, backed)
}

// cloneFile copies src to dst with a reflink if possible.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) != nil {
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}

// Prefetch materializes the given nodes with parallel workers (e.g. all
// small files at mount: cargo reads hundreds of fingerprints serially)
// and returns the number fetched.
func (f *Files) Prefetch(ctx context.Context, tier Tier, nodes []*Node, parallel int) int {
	if parallel <= 0 {
		parallel = 16
	}
	ch := make(chan *Node)
	var wg sync.WaitGroup
	var mu sync.Mutex
	n := 0
	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for nd := range ch {
				if _, ok := f.Local(nd); ok {
					continue
				}
				if _, err := f.Ensure(ctx, tier, nd); err == nil {
					mu.Lock()
					n++
					mu.Unlock()
				}
			}
		}()
	}
	for _, nd := range nodes {
		if ctx.Err() != nil {
			break
		}
		ch <- nd
	}
	close(ch)
	wg.Wait()
	return n
}

// Reclaim drops least recently used backed materialized files that are
// not open until at most target bytes are resident; it returns the bytes
// freed.
func (f *Files) Reclaim(target int64) int64 { return f.reclaim(target, "") }

func (f *Files) reclaim(target int64, keep string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var freed, n int64
	for e := f.lru.Front(); e != nil && f.resident > target; {
		next := e.Next()
		m := e.Value.(*mfile)
		if m.open == 0 && m.backed && m.digest != keep && f.pins[m.digest] == 0 {
			os.Remove(f.path(m.digest))
			f.lru.Remove(e)
			delete(f.files, m.digest)
			f.resident -= m.size
			freed += m.size
			n++
		}
		e = next
	}
	if freed > 0 {
		f.obs.emit(Event{Type: EvReclaim, Tier: "files", Pages: n, Bytes: freed})
	}
	return freed
}

// Drop removes a resident file unless it is open (its content is no
// longer referenced).
func (f *Files) Drop(digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.files[digest]; m != nil && m.open == 0 {
		os.Remove(f.path(digest))
		f.lru.Remove(m.elem)
		delete(f.files, digest)
		f.resident -= m.size
	}
}

// DropUnreferenced removes the resident files keep rejects (content no
// tree references any more) unless they are open, pinned or the only copy
// (unbacked), and returns their bytes.
func (f *Files) DropUnreferenced(keep func(digest string) bool) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var freed int64
	for d, m := range f.files {
		if m.open > 0 || !m.backed || f.pins[d] > 0 || keep(d) {
			continue
		}
		os.Remove(f.path(d))
		f.lru.Remove(m.elem)
		delete(f.files, d)
		f.resident -= m.size
		freed += m.size
	}
	return freed
}

// Cold marks the materialized content of nodes as least recently used,
// in order (the last node becomes the coldest), so that Reclaim drops it
// first: the caller's reclaim order (e.g. by access class) ahead of plain
// recency. Nodes without resident content are ignored.
func (f *Files) Cold(nodes []*Node) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range nodes {
		if m := f.files[digestOf(n)]; m != nil {
			f.lru.MoveToFront(m.elem)
		}
	}
}

// Digests returns the digests of the resident files.
func (f *Files) Digests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.files))
	for d := range f.files {
		out = append(out, d)
	}
	return out
}

// Stats reports the resident bytes of materialized files and the file
// faults so far.
func (f *Files) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Stats{ResidentBytes: f.resident, Pages: int64(len(f.files)), Faults: f.faults, PageInBytes: f.faultB, FaultRate: f.rate.get(time.Now())}
}

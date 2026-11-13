package anyfs

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/zeebo/blake3"
	"golang.org/x/sys/unix"
)

// FileID identifies a file's content without reading it: the inode and
// the size and timestamps of its last change. Every write changes the
// ctime, which user space cannot set back, so an unchanged FileID means
// unchanged content (within the file system's timestamp granularity,
// which Hasher accounts for by hashing only settled files).
type FileID struct {
	Dev, Ino       uint64
	Size           int64
	MtimeNs, Ctime int64
}

// FileIDOf returns a file's identity (ok false: no inode information).
func FileIDOf(fi fs.FileInfo) (FileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() {
		return FileID{}, false
	}
	return FileID{Dev: uint64(st.Dev), Ino: st.Ino, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(),
		Ctime: st.Ctim.Sec*1e9 + st.Ctim.Nsec}, true
}

// HasherOptions configure a Hasher.
type HasherOptions struct {
	// Sink receives the pages of hashed files (nil: digests only).
	Sink PageSink
	// RangeSink, if set, receives the pages instead of Sink, with their
	// range in the hashed file (a tier can clone it instead of writing
	// the bytes again).
	RangeSink RangeSink
	// Skip excludes files from background hashing (e.g. ephemeral access
	// classes: deleted before any commit, hashing them is wasted IO).
	Skip func(rel string) bool
	// MinSize: smaller files are left to the commit (default 64 KiB;
	// the bytes are in the large files).
	MinSize int64
	// Workers bounds files hashed at once (default DefaultHashWorkers).
	// They run at the lowest CPU and IO priority (nice 19, idle IO
	// class), so they use cores the session leaves idle.
	Workers int
	// BytesPerSec bounds the read rate (0: unbounded) while the
	// machine's IO is under pressure (Pressure at least PressureLimit),
	// so hashing in the background does not starve the session's own IO
	// on a slow disk; without pressure (outputs read from the page cache,
	// a fast disk) hashing runs at full speed.
	BytesPerSec float64
	// Pressure returns the share of time (percent) tasks stalled on IO
	// recently (ok false: unknown, the rate always applies). Default:
	// /proc/pressure/io "some" avg10.
	Pressure func() (float64, bool)
	// PressureLimit is the IO pressure (percent) from which BytesPerSec
	// applies (default 10).
	PressureLimit float64
	// Settle: a file is hashed once its last change is this old (default
	// 1s); a write while it is read changes its ctime and the result is
	// dropped.
	Settle time.Duration
	// MaxEntries bounds the digests kept (default 200000).
	MaxEntries int
	// OnHashed is called with each file hashed (e.g. to give its pages a
	// writeback hint).
	OnHashed func(n *Node)
	Now      func() time.Time
}

// HasherStats count a Hasher's work.
type HasherStats struct {
	Queued, Hashed, HashedBytes int64
	Stale                       int64 // results dropped: written while read, or changed before the commit
	Hits, HitBytes              int64 // lookups answered
	Pending                     int   // files waiting
}

// Hasher hashes files in the background as they are written (e.g. on
// fanotify close_write) and remembers digest and pages per path with the
// file's identity; a commit looks them up (Lookup) instead of reading
// the files again. Pages go to the sink when hashed, so they can be
// written back to the store during the session.
type Hasher struct {
	opt HasherOptions

	mu      sync.Mutex
	queue   map[string]*hitem // rel -> pending
	entries map[string]*hentry
	wake    chan struct{}
	stop    chan struct{}
	wg      sync.WaitGroup
	st      HasherStats
	tokens  float64
	last    time.Time
	seq     int64
	// pressured caches the IO pressure check (once a second).
	pressured   bool
	pressuredAt time.Time
}

type hitem struct {
	rel, path string
	due       time.Time
}

type hentry struct {
	id     FileID
	digest string
	pages  []PageRef
	held   bool // looked up by a commit not finished yet: never evicted
	seq    int64
}

// NewHasher starts a hasher; Close stops it.
func NewHasher(opt HasherOptions) *Hasher {
	if opt.MinSize <= 0 {
		opt.MinSize = 64 << 10
	}
	if opt.Workers <= 0 {
		opt.Workers = DefaultHashWorkers()
	}
	if opt.Pressure == nil {
		opt.Pressure = IOPressure
	}
	if opt.PressureLimit <= 0 {
		opt.PressureLimit = 10
	}
	if opt.Settle <= 0 {
		opt.Settle = time.Second
	}
	if opt.MaxEntries <= 0 {
		opt.MaxEntries = 200000
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	h := &Hasher{opt: opt, queue: map[string]*hitem{}, entries: map[string]*hentry{}, wake: make(chan struct{}, 1), stop: make(chan struct{})}
	for range opt.Workers {
		h.wg.Add(1)
		go h.loop()
	}
	return h
}

// DefaultHashWorkers is the default number of files a Hasher hashes at
// once: a quarter of the cores (2..8).
func DefaultHashWorkers() int {
	return min(max(runtime.NumCPU()/4, 2), 8)
}

// IOPressure returns the machine's IO pressure: the share of the last 10
// seconds some task stalled on IO (/proc/pressure/io, percent).
func IOPressure() (float64, bool) {
	b, err := os.ReadFile("/proc/pressure/io")
	if err != nil {
		return 0, false
	}
	var avg10 float64
	if _, err := fmt.Sscanf(string(b), "some avg10=%f", &avg10); err != nil {
		return 0, false
	}
	return avg10, true
}

// lowPriority lowers the calling goroutine's thread to nice 19 and the
// idle IO class; it stays locked to the thread, which exits with the
// goroutine (the priority never leaks to other goroutines).
func lowPriority() {
	runtime.LockOSThread()
	tid := unix.Gettid()
	unix.Setpriority(unix.PRIO_PROCESS, tid, 19)
	const ioprioWhoProcess, ioprioClassIdle, ioprioClassShift = 1, 3, 13
	unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), ioprioClassIdle<<ioprioClassShift)
}

// Close stops the workers (files being hashed are finished).
func (h *Hasher) Close() {
	select {
	case <-h.stop:
		return
	default:
	}
	close(h.stop)
	h.wg.Wait()
}

// Queue asks for the file at path (rel in the tree) to be hashed once it
// settled; queuing it again (another write) postpones it.
func (h *Hasher) Queue(rel, path string) {
	if h.opt.Skip != nil && h.opt.Skip(rel) {
		return
	}
	h.mu.Lock()
	h.queue[rel] = &hitem{rel: rel, path: path, due: h.opt.Now().Add(h.opt.Settle)}
	h.st.Queued++
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Forget drops queued files (a commit reads them itself: hashing them
// again in the background would read them twice).
func (h *Hasher) Forget(rels []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range rels {
		delete(h.queue, r)
	}
}

// next takes the queued file due first (ok false: none due; wait is how
// long until the next one is).
func (h *Hasher) next() (hitem, bool, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.opt.Now()
	var best *hitem
	for _, it := range h.queue {
		if best == nil || it.due.Before(best.due) {
			best = it
		}
	}
	if best == nil {
		return hitem{}, false, time.Hour
	}
	if d := best.due.Sub(now); d > 0 {
		return hitem{}, false, d
	}
	delete(h.queue, best.rel)
	return *best, true, 0
}

func (h *Hasher) loop() {
	defer h.wg.Done()
	lowPriority()
	for {
		it, ok, wait := h.next()
		if !ok {
			t := time.NewTimer(min(wait, time.Second))
			select {
			case <-h.stop:
				t.Stop()
				return
			case <-h.wake:
			case <-t.C:
			}
			t.Stop()
			continue
		}
		select {
		case <-h.stop:
			return
		default:
		}
		h.hash(it)
	}
}

// hash reads one settled file and records its digest if it did not
// change while it was read.
func (h *Hasher) hash(it hitem) {
	fi, err := os.Lstat(it.path)
	if err != nil {
		return
	}
	id, ok := FileIDOf(fi)
	if !ok || id.Size < h.opt.MinSize {
		return
	}
	h.mu.Lock()
	if e := h.entries[it.rel]; e != nil && e.id == id {
		h.mu.Unlock()
		return // hashed already
	}
	h.mu.Unlock()
	now := h.opt.Now()
	if settled := time.Unix(0, id.Ctime).Add(h.opt.Settle); settled.After(now) {
		h.requeue(it, settled) // still being written
		return
	}
	f, err := os.Open(it.path)
	if err != nil {
		return
	}
	n := &Node{Path: it.rel, Kind: File, Size: id.Size}
	var r io.Reader = f
	if h.opt.BytesPerSec > 0 {
		r = &limitedReader{r: f, h: h}
	}
	switch {
	case h.opt.RangeSink != nil:
		err = NewPacker(h.opt.Sink).Serial().WithRanges(h.opt.RangeSink).AddLargeFile(n, f, id.Ctime, r)
	case h.opt.Sink != nil:
		err = NewPacker(h.opt.Sink).Serial().AddLarge(n, r)
	default:
		err = hashReader(n, r)
	}
	f.Close()
	if err != nil {
		return
	}
	fi2, err := os.Lstat(it.path)
	id2, ok2 := FileID{}, false
	if err == nil {
		id2, ok2 = FileIDOf(fi2)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !ok2 || id2 != id || n.Size != id.Size {
		h.st.Stale++ // written meanwhile: its pages are discarded unreferenced
		return
	}
	h.seq++
	if e := h.entries[it.rel]; e != nil && e.held {
		return // a commit uses the old one
	}
	h.entries[it.rel] = &hentry{id: id, digest: n.Digest, pages: n.Pages, seq: h.seq}
	h.st.Hashed++
	h.st.HashedBytes += id.Size
	h.evictLocked()
	if h.opt.OnHashed != nil {
		h.opt.OnHashed(n)
	}
}

func (h *Hasher) requeue(it hitem, due time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.queue[it.rel]; !ok {
		it.due = due
		h.queue[it.rel] = &it
	}
}

// evictLocked drops the oldest unheld entries beyond MaxEntries.
func (h *Hasher) evictLocked() {
	over := len(h.entries) - h.opt.MaxEntries
	if over <= 0 {
		return
	}
	type ent struct {
		rel string
		seq int64
	}
	var es []ent
	for rel, e := range h.entries {
		if !e.held {
			es = append(es, ent{rel, e.seq})
		}
	}
	sort.Slice(es, func(i, j int) bool { return es[i].seq < es[j].seq })
	for i := 0; i < over && i < len(es); i++ {
		delete(h.entries, es[i].rel)
	}
}

// Lookup sets n's digest and pages if the file at path (n's path in the
// tree) still has the identity it had when it was hashed; the entry is
// held until Release (it is never evicted while a commit uses its
// pages).
func (h *Hasher) Lookup(n *Node, path string) bool {
	h.mu.Lock()
	e := h.entries[n.Path]
	h.mu.Unlock()
	if e == nil {
		return false
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	id, ok := FileIDOf(fi)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries[n.Path] != e {
		return false
	}
	if !ok || id != e.id || n.Size != id.Size || n.MtimeNs != id.MtimeNs {
		delete(h.entries, n.Path)
		h.st.Stale++
		return false
	}
	n.Digest, n.Pages = e.digest, e.pages
	e.held = true
	h.st.Hits++
	h.st.HitBytes += id.Size
	return true
}

// Release ends the hold of the paths' entries; committed (the commit
// referencing their pages is durable) drops them: unchanged files are
// not read by later commits anyway.
func (h *Hasher) Release(paths []string, committed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range paths {
		if e := h.entries[p]; e != nil && e.held {
			if committed {
				delete(h.entries, p)
			} else {
				e.held = false
			}
		}
	}
}

// Keys returns the pages of the remembered digests (dirty pages a
// commit must not discard yet).
func (h *Hasher) Keys() []Key {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Key
	for _, e := range h.entries {
		for _, p := range e.pages {
			out = append(out, p.Key)
		}
	}
	return out
}

// Stats returns the hasher's counters.
func (h *Hasher) Stats() HasherStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.st
	st.Pending = len(h.queue)
	return st
}

// Drain waits until no queued file is due (files written within the
// last Settle stay queued) or ctx ends.
func (h *Hasher) Drain(ctx context.Context) {
	for {
		h.mu.Lock()
		due := false
		now := h.opt.Now()
		for _, it := range h.queue {
			if !it.due.After(now) {
				due = true
				break
			}
		}
		h.mu.Unlock()
		if !due {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// limitedReader paces reads to the hasher's byte rate (a token bucket
// shared by its workers, one second of burst).
type limitedReader struct {
	r io.Reader
	h *Hasher
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if len(p) > 1<<20 {
		p = p[:1<<20]
	}
	if l.h.underPressure() {
		l.h.take(len(p))
	}
	return l.r.Read(p)
}

// underPressure reports whether the rate limit applies (IO pressure at
// least the limit, or unknown), checked at most once a second.
func (h *Hasher) underPressure() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if now.Sub(h.pressuredAt) < time.Second {
		return h.pressured
	}
	p, ok := h.opt.Pressure()
	h.pressured, h.pressuredAt = !ok || p >= h.opt.PressureLimit, now
	if !h.pressured {
		h.last = time.Time{} // a fresh bucket when pressure returns
	}
	return h.pressured
}

func (h *Hasher) take(n int) {
	rate := h.opt.BytesPerSec
	for {
		h.mu.Lock()
		now := time.Now()
		if h.last.IsZero() {
			h.tokens = rate
		} else {
			h.tokens = min(rate, h.tokens+now.Sub(h.last).Seconds()*rate)
		}
		h.last = now
		if h.tokens >= float64(n) || h.tokens >= rate {
			h.tokens -= float64(n)
			h.mu.Unlock()
			return
		}
		wait := time.Duration((float64(n) - h.tokens) / rate * float64(time.Second))
		h.mu.Unlock()
		time.Sleep(wait)
	}
}

// hashReader sets n's digest and size from r (no pages).
func hashReader(n *Node, r io.Reader) error {
	h := blake3.New()
	sz, err := io.Copy(h, r)
	if err != nil {
		return err
	}
	n.Size, n.Digest = sz, hex.EncodeToString(h.Sum(nil))
	return nil
}

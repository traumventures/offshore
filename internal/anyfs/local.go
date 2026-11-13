package anyfs

import (
	"bufio"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/traumventures/offshore/internal/hostfs"
	"github.com/traumventures/offshore/internal/medium"
)

// LocalOptions configure a Local tier.
type LocalOptions struct {
	Dir      string
	Capacity int64 // bytes (0: unbounded, never reclaims on Put)
	// Medium is the profile of the disk (default: medium "local").
	Medium medium.Profile
	// Window is the working set window (default 10 minutes).
	Window time.Duration
	// Pool is a machine-wide directory of page files that several local
	// tiers (repositories) share by hardlink: a page put into one tier
	// that another already holds takes no extra disk (dedupe of content
	// addressed pages). Empty: no sharing.
	Pool     string
	Observer Observer
	Now      func() time.Time
}

// Local is the resident tier: pages as files on a local disk (or a
// mounted volume), replaced with ARC: pages touched once (T1) are
// reclaimed before pages touched again (T2), so a clean build streaming
// through new pages does not evict the hot set; ghost entries of
// reclaimed pages adapt the balance and detect refaults (thrashing).
type Local struct {
	opt LocalOptions
	dir string

	mu      sync.Mutex
	pages   map[Key]*lpage
	t1, t2  *list.List // resident, LRU at the front
	b1, b2  *list.List // ghosts (reclaimed), LRU at the front
	ghosts  map[Key]*ghost
	p       int64 // ARC target bytes of T1
	t1Bytes int64
	st      Stats
	thrash  bool
	journal *os.File
	hints   *os.File // hint journal (hints.journal)
	hintN   int      // lines appended since the last rewrite
	demote  func(Key, []byte)
	faults  rate
	noClone atomic.Bool // PutRange found clones unsupported
}

type lpage struct {
	key    Key
	size   int64
	state  State
	pinned bool
	shared bool // linked from the pool (another tier held it)
	cloned bool // a clone (reflink) of a file range
	hint   PageHint
	last   time.Time
	inT2   bool
	elem   *list.Element
}

// PageHint is the access-class policy of a resident page (set by the
// owner after a commit; persisted in the tier's hint journal, so it
// survives a restart).
type PageHint struct {
	// Expire is the page's dirty expiry (0: the link's DirtyExpire,
	// negative: written back at the next pass).
	Expire time.Duration
	// NoWriteback: never written back by the periodic writeback or dirty
	// pressure, only by an explicit flush.
	NoWriteback bool
	// LocalOnly: never demoted to a lower tier on reclaim.
	LocalOnly bool
}

type ghost struct {
	size int64
	last time.Time
	inB2 bool
	elem *list.Element
}

// OpenLocal opens (or creates) a local tier. Pages found on disk are
// resident; those the dirty journal lists are dirty and those the hint
// journal lists get their hint back.
func OpenLocal(opt LocalOptions) (*Local, error) {
	if opt.Window == 0 {
		opt.Window = 10 * time.Minute
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Medium.Medium == "" {
		opt.Medium = medium.Profile{Medium: medium.Local, Reach: medium.ReachMachine, Durability: medium.ReachMachine}
	}
	l := &Local{opt: opt, dir: filepath.Join(opt.Dir, "pages"), pages: map[Key]*lpage{},
		t1: list.New(), t2: list.New(), b1: list.New(), b2: list.New(), ghosts: map[Key]*ghost{}}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return nil, err
	}
	dirty := l.readJournal()
	hints := l.readHints()
	now := opt.Now()
	err := filepath.WalkDir(l.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		k := Key(d.Name())
		if !k.Valid() {
			os.Remove(p) // temporary files of an interrupted write
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if !pageIntact(p, k, fi.Size(), dirty[k]) {
			// Torn by a crash before its data reached the disk (page
			// files are renamed into place without fsync): a clean page
			// is fetched again, a dirty one is lost, not published wrong.
			os.Remove(p)
			delete(dirty, k)
			return nil
		}
		pg := &lpage{key: k, size: fi.Size(), last: now}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && opt.Pool != "" && st.Nlink > 2 {
			pg.shared = true
			l.st.SharedBytes += pg.size
		}
		if dirty[k] {
			pg.state = Dirty
			l.st.DirtyBytes += pg.size
		}
		pg.hint = hints[k]
		l.insert(pg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := l.rewriteJournal(); err != nil {
		return nil, err
	}
	if err := l.rewriteHints(); err != nil {
		return nil, err
	}
	return l, nil
}

// pageIntact checks a page file found at open: an empty file must be the
// empty page; a dirty page (not yet written back, so not fetchable again)
// is verified against its key.
func pageIntact(path string, k Key, size int64, dirty bool) bool {
	if size == 0 {
		return k == KeyOf(nil)
	}
	if !dirty {
		return true
	}
	b, err := os.ReadFile(path)
	return err == nil && KeyOf(b) == k
}

func (l *Local) path(k Key) string { return filepath.Join(l.dir, string(k[:2]), string(k)) }

func (l *Local) poolPath(k Key) string {
	return filepath.Join(l.opt.Pool, string(k[:2]), string(k))
}

// removeFile deletes a page's file and its pool entry once no other tier
// links it.
func (l *Local) removeFile(k Key) {
	os.Remove(l.path(k))
	if l.opt.Pool == "" {
		return
	}
	pp := l.poolPath(k)
	if fi, err := os.Lstat(pp); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink <= 1 {
			os.Remove(pp)
		}
	}
}

// Hint sets the access-class policy of a resident page (persisted).
func (l *Local) Hint(k Key, h PageHint) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pg := l.pages[k]
	if pg == nil || pg.hint == h {
		return
	}
	pg.hint = h
	if l.hints == nil {
		return
	}
	fmt.Fprintln(l.hints, formatHint(k, h))
	if l.hintN++; l.hintN > 2*len(l.pages)+1024 {
		l.hints.Close()
		l.hints = nil
		l.rewriteHints() // compact (best effort: hints are policy, not data)
	}
}

func (l *Local) hintsPath() string { return filepath.Join(l.opt.Dir, "hints.journal") }

// formatHint is a hint journal line: key, expiry (ns), flags (n:
// NoWriteback, l: LocalOnly, -: none).
func formatHint(k Key, h PageHint) string {
	flags := ""
	if h.NoWriteback {
		flags += "n"
	}
	if h.LocalOnly {
		flags += "l"
	}
	if flags == "" {
		flags = "-"
	}
	return fmt.Sprintf("%s %d %s", k, int64(h.Expire), flags)
}

// readHints replays the hint journal (the last line of a key wins).
func (l *Local) readHints() map[Key]PageHint {
	out := map[Key]PageHint{}
	f, err := os.Open(l.hintsPath())
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ks, flags string
		var exp int64
		if n, _ := fmt.Sscanf(sc.Text(), "%s %d %s", &ks, &exp, &flags); n != 3 || !Key(ks).Valid() {
			continue // torn line of a crash
		}
		out[Key(ks)] = PageHint{Expire: time.Duration(exp), NoWriteback: strings.Contains(flags, "n"), LocalOnly: strings.Contains(flags, "l")}
	}
	return out
}

// rewriteHints writes the hints of the resident pages and reopens the
// journal for appending (locked or during open).
func (l *Local) rewriteHints() error {
	tmp := l.hintsPath() + ".tmp"
	var b strings.Builder
	for k, pg := range l.pages {
		if pg.hint != (PageHint{}) {
			b.WriteString(formatHint(k, pg.hint) + "\n")
		}
	}
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.hintsPath()); err != nil {
		return err
	}
	f, err := os.OpenFile(l.hintsPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.hints, l.hintN = f, 0
	return nil
}

// PageHint returns a resident page's hint.
func (l *Local) PageHint(k Key) (PageHint, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pg := l.pages[k]; pg != nil {
		return pg.hint, true
	}
	return PageHint{}, false
}

// Path returns the file holding a resident page ("" if not resident).
func (l *Local) Path(k Key) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pages[k] == nil {
		return ""
	}
	return l.path(k)
}

func (l *Local) journalPath() string { return filepath.Join(l.opt.Dir, "dirty.journal") }

func (l *Local) readJournal() map[Key]bool {
	dirty := map[Key]bool{}
	f, err := os.Open(l.journalPath())
	if err != nil {
		return dirty
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2 {
			continue
		}
		k := Key(line[1:])
		switch line[0] {
		case '+':
			dirty[k] = true
		case '-':
			delete(dirty, k)
		}
	}
	return dirty
}

func (l *Local) rewriteJournal() error {
	var b strings.Builder
	for k, pg := range l.pages {
		if pg.state == Dirty {
			b.WriteString("+" + string(k) + "\n")
		}
	}
	// Replaced durably: a crash leaves the old or the new journal, never
	// an empty one (ext4 noauto_da_alloc, xfs).
	if err := hostfs.WriteFileSync(l.journalPath(), []byte(b.String()), 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(l.journalPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.journal = f
	return nil
}

func (l *Local) logDirty(k Key, dirty bool) {
	if l.journal == nil {
		return
	}
	c := "-"
	if dirty {
		c = "+"
	}
	fmt.Fprintf(l.journal, "%s%s\n", c, k)
}

// Close closes the dirty and hint journals.
func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hints != nil {
		l.hints.Close()
		l.hints = nil
	}
	if l.journal == nil {
		return nil
	}
	err := l.journal.Close()
	l.journal = nil
	return err
}

// SetDemote installs a function that receives the content of every page
// reclaimed (victim caching into a lower tier).
func (l *Local) SetDemote(f func(Key, []byte)) {
	l.mu.Lock()
	l.demote = f
	l.mu.Unlock()
}

// insert adds a page to T1 (locked).
func (l *Local) insert(pg *lpage) {
	pg.elem = l.t1.PushBack(pg)
	l.t1Bytes += pg.size
	l.pages[pg.key] = pg
	l.st.ResidentBytes += pg.size
	l.st.Pages++
}

// touch records an access: a page touched again moves to T2 (locked).
func (l *Local) touch(pg *lpage) {
	pg.last = l.opt.Now()
	if pg.inT2 {
		l.t2.MoveToBack(pg.elem)
		return
	}
	l.t1.Remove(pg.elem)
	l.t1Bytes -= pg.size
	pg.inT2 = true
	pg.elem = l.t2.PushBack(pg)
}

// Get implements Tier.
func (l *Local) Get(ctx context.Context, k Key) ([]byte, error) {
	l.mu.Lock()
	pg := l.pages[k]
	if pg == nil {
		l.mu.Unlock()
		return nil, ErrNotFound
	}
	l.touch(pg)
	l.mu.Unlock()
	b, err := os.ReadFile(l.path(k))
	if os.IsNotExist(err) {
		l.mu.Lock()
		l.drop(pg)
		l.mu.Unlock()
		return nil, ErrNotFound
	}
	return b, err
}

// Has implements Tier.
func (l *Local) Has(ctx context.Context, k Key) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pages[k] != nil
}

// Peek reads a resident page without counting an access.
func (l *Local) Peek(k Key) ([]byte, error) {
	if !l.Has(context.Background(), k) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(l.path(k))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return b, err
}

// Put implements Tier. A page put again keeps the dirtier state.
func (l *Local) Put(ctx context.Context, k Key, b []byte, s State) error {
	return l.put(k, b, s, nil)
}

// PutRange implements RangePutter: a new page file is a clone of r
// (FICLONERANGE: shares the file's blocks, nothing is written) where the
// file system supports it, else b is written. Build outputs hashed during
// a session are thereby not written to the disk a second time.
func (l *Local) PutRange(ctx context.Context, k Key, b []byte, r Range, s State) error {
	if r.File == nil || r.Ctime == 0 || l.noClone.Load() {
		return l.put(k, b, s, nil)
	}
	return l.put(k, b, s, func(tmp string) (bool, error) {
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o444)
		if err != nil {
			return false, err
		}
		cerr := unix.IoctlFileCloneRange(int(out.Fd()), &unix.FileCloneRange{
			Src_fd: int64(r.File.Fd()), Src_offset: uint64(r.Off), Src_length: uint64(len(b))})
		if cerr == nil {
			// The page must hold b (its key): the clone does if the
			// source was not written since b was read (every write
			// changes the ctime, which user space cannot set back).
			fi, err := out.Stat()
			if err == nil && fi.Size() == int64(len(b)) && fileCtime(r.File) == r.Ctime {
				return true, out.Close()
			}
			cerr = errCloneMismatch
		}
		out.Close()
		os.Remove(tmp)
		if errors.Is(cerr, unix.EOPNOTSUPP) || errors.Is(cerr, unix.EXDEV) || errors.Is(cerr, unix.ENOTTY) ||
			errors.Is(cerr, unix.ENOSYS) {
			l.noClone.Store(true) // not supported here: stop trying
		}
		return false, nil
	})
}

var errCloneMismatch = errors.New("cloned range differs from the page")

// fileCtime returns an open file's ctime in ns (0: unknown).
func fileCtime(f *os.File) int64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	if id, ok := FileIDOf(fi); ok {
		return id.Ctime
	}
	return 0
}

// put stores a page; clone, if set, creates the page file at tmp from a
// file range and reports whether it did (false: b is written).
func (l *Local) put(k Key, b []byte, s State, clone func(tmp string) (bool, error)) error {
	l.mu.Lock()
	if pg := l.pages[k]; pg != nil {
		if s == Dirty && pg.state == Clean {
			pg.state = Dirty
			l.st.DirtyBytes += pg.size
			l.logDirty(k, true)
		}
		l.touch(pg)
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()
	p := l.path(k)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	shared := l.opt.Pool != "" && os.Link(l.poolPath(k), p) == nil
	cloned := false
	if !shared {
		tmp := fmt.Sprintf("%s.%d.tmp", p, time.Now().UnixNano())
		if clone != nil {
			ok, err := clone(tmp)
			if err != nil {
				os.Remove(tmp)
				return err
			}
			cloned = ok
		}
		if !cloned {
			if err := os.WriteFile(tmp, b, 0o444); err != nil {
				os.Remove(tmp)
				return err
			}
		}
		if err := os.Rename(tmp, p); err != nil {
			os.Remove(tmp)
			return err
		}
		if l.opt.Pool != "" {
			pp := l.poolPath(k)
			os.MkdirAll(filepath.Dir(pp), 0o755)
			os.Link(p, pp)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pages[k] != nil { // raced with another Put
		return nil
	}
	pg := &lpage{key: k, size: int64(len(b)), state: s, last: l.opt.Now(), shared: shared, cloned: cloned}
	if shared {
		l.st.SharedBytes += pg.size
	}
	if cloned {
		l.st.ClonedBytes += pg.size
	}
	if g := l.ghosts[k]; g != nil {
		// A refault: ARC adapts towards the list the ghost came from.
		l.st.Refaults++
		l.st.RefaultBytes += pg.size
		if g.inB2 {
			l.p = max(0, l.p-pg.size)
			l.b2.Remove(g.elem)
		} else {
			l.p = min(l.opt.Capacity, l.p+pg.size)
			l.b1.Remove(g.elem)
		}
		delete(l.ghosts, k)
		l.insert(pg)
		l.touch(pg) // seen before: frequent
	} else {
		l.insert(pg)
	}
	if s == Dirty {
		l.st.DirtyBytes += pg.size
		l.logDirty(k, true)
	}
	if l.opt.Capacity > 0 && l.st.ResidentBytes > l.opt.Capacity {
		l.reclaimLocked(l.opt.Capacity)
	}
	l.checkThrash()
	return nil
}

// MarkClean implements Cleaner.
func (l *Local) MarkClean(k Key) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pg := l.pages[k]; pg != nil && pg.state == Dirty {
		pg.state = Clean
		l.st.DirtyBytes -= pg.size
		l.logDirty(k, false)
	}
}

// Discard implements Discarder: a dirty page is dropped unwritten.
func (l *Local) Discard(k Key) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pg := l.pages[k]; pg != nil && pg.state == Dirty && !pg.pinned {
		l.drop(pg)
	}
}

// Remove implements Remover.
func (l *Local) Remove(k Key) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pg := l.pages[k]; pg != nil {
		l.drop(pg)
	}
}

// DropUnreferenced removes the clean, unpinned pages keep rejects (no
// tree the owner still mounts or commits references them; the store has
// them) and returns their bytes. Dirty pages are never dropped here.
func (l *Local) DropUnreferenced(keep func(Key) bool) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var freed int64
	for _, pg := range l.pages {
		if pg.state != Clean || pg.pinned || keep(pg.key) {
			continue
		}
		freed += pg.size
		l.drop(pg)
	}
	return freed
}

// Pin implements Pinner.
func (l *Local) Pin(k Key, pinned bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pg := l.pages[k]; pg != nil && pg.pinned != pinned {
		pg.pinned = pinned
		if pinned {
			l.st.PinnedBytes += pg.size
		} else {
			l.st.PinnedBytes -= pg.size
		}
	}
}

// State reports a resident page's state.
func (l *Local) State(k Key) (State, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pg := l.pages[k]
	if pg == nil {
		return Clean, false
	}
	return pg.state, true
}

// DirtyKeys returns the dirty pages with their sizes and the time they
// were last written.
func (l *Local) DirtyKeys() []DirtyPage {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []DirtyPage
	for k, pg := range l.pages {
		if pg.state == Dirty {
			out = append(out, DirtyPage{Key: k, Size: pg.size, Since: pg.last, Hint: pg.hint})
		}
	}
	return out
}

// DirtyPage is a page awaiting writeback.
type DirtyPage struct {
	Key   Key
	Size  int64
	Since time.Time
	Hint  PageHint
}

// drop removes a page and its file without a ghost (locked).
func (l *Local) drop(pg *lpage) {
	l.unlink(pg)
	l.removeFile(pg.key)
}

func (l *Local) unlink(pg *lpage) {
	if pg.inT2 {
		l.t2.Remove(pg.elem)
	} else {
		l.t1.Remove(pg.elem)
		l.t1Bytes -= pg.size
	}
	delete(l.pages, pg.key)
	l.st.ResidentBytes -= pg.size
	l.st.Pages--
	if pg.state == Dirty {
		l.st.DirtyBytes -= pg.size
		l.logDirty(pg.key, false)
	}
	if pg.pinned {
		l.st.PinnedBytes -= pg.size
	}
	if pg.shared {
		l.st.SharedBytes -= pg.size
	}
	if pg.cloned {
		l.st.ClonedBytes -= pg.size
	}
}

// Reclaim implements Tier.
func (l *Local) Reclaim(ctx context.Context, target int64) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reclaimLocked(target), nil
}

// reclaimLocked evicts clean, unpinned pages (ARC order) until at most
// target bytes are resident.
func (l *Local) reclaimLocked(target int64) int64 {
	begin := time.Now()
	var freed, pages int64
	for l.st.ResidentBytes > target {
		// ARC: take from T1 while it exceeds its target p, else from T2.
		first, second := l.t2, l.t1
		if l.t1Bytes > l.p || l.t2.Len() == 0 {
			first, second = l.t1, l.t2
		}
		pg := victim(first)
		if pg == nil {
			pg = victim(second)
		}
		if pg == nil {
			break // everything dirty or pinned
		}
		if l.demote != nil && !pg.hint.LocalOnly {
			if b, err := os.ReadFile(l.path(pg.key)); err == nil {
				l.demote(pg.key, b)
			}
		}
		l.unlink(pg)
		l.removeFile(pg.key)
		g := &ghost{size: pg.size, last: pg.last, inB2: pg.inT2}
		if pg.inT2 {
			g.elem = l.b2.PushBack(pg.key)
		} else {
			g.elem = l.b1.PushBack(pg.key)
		}
		l.ghosts[pg.key] = g
		freed += pg.size
		pages++
	}
	l.trimGhosts()
	if freed > 0 {
		l.st.ReclaimBytes += freed
		l.opt.Observer.emit(Event{Type: EvReclaim, Tier: l.opt.Medium.Medium, Pages: pages, Bytes: freed, Seconds: time.Since(begin).Seconds()})
	}
	return freed
}

// victim is the least recently used clean, unpinned page of a list.
func victim(lst *list.List) *lpage {
	for e := lst.Front(); e != nil; e = e.Next() {
		pg := e.Value.(*lpage)
		if pg.state == Clean && !pg.pinned {
			return pg
		}
	}
	return nil
}

// trimGhosts bounds the ghost lists to about twice the capacity (or 64k
// entries when unbounded).
func (l *Local) trimGhosts() {
	maxN := 65536
	if l.opt.Capacity > 0 {
		maxN = int(2*l.opt.Capacity/PageSize) + 1024
	}
	for l.b1.Len()+l.b2.Len() > maxN {
		lst := l.b1
		if l.b2.Len() > l.b1.Len() {
			lst = l.b2
		}
		e := lst.Front()
		delete(l.ghosts, e.Value.(Key))
		lst.Remove(e)
	}
}

// workingSet sums the pages (resident and reclaimed) touched within the
// window (locked).
func (l *Local) workingSet() int64 {
	cut := l.opt.Now().Add(-l.opt.Window)
	var ws int64
	for _, pg := range l.pages {
		if !pg.last.Before(cut) {
			ws += pg.size
		}
	}
	for _, g := range l.ghosts {
		if !g.last.Before(cut) {
			ws += g.size
		}
	}
	return ws
}

// checkThrash updates the thrashing state: the working set exceeds the
// capacity and recently reclaimed pages come back (locked).
func (l *Local) checkThrash() {
	if l.opt.Capacity <= 0 || l.st.Refaults == 0 {
		return
	}
	t := l.workingSet() > l.opt.Capacity
	if t && !l.thrash {
		l.opt.Observer.emit(Event{Type: EvThrash, Tier: l.opt.Medium.Medium, Bytes: l.workingSet()})
	}
	l.thrash = t
}

// NoteFault counts a page fault observed by a link above (for the
// fault rate).
func (l *Local) NoteFault() {
	l.mu.Lock()
	l.st.Faults++
	l.mu.Unlock()
	l.faults.add(l.opt.Now(), 1)
}

// Stats implements Statter.
func (l *Local) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.st
	s.WorkingSetBytes = l.workingSet()
	s.Capacity = l.opt.Capacity
	s.Thrashing = l.thrash
	s.FaultRate = l.faults.get(l.opt.Now())
	return s
}

// Profile implements Tier.
func (l *Local) Profile() Profile {
	return Profile{Name: l.opt.Medium.Medium, Levels: []medium.Profile{l.opt.Medium}, Capacity: l.opt.Capacity}
}

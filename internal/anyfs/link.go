package anyfs

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Inclusion decides whether a page may live in both tiers of a link.
type Inclusion int

const (
	// Inclusive: pages faulted in stay in the lower tier too (a cache
	// over a backing store).
	Inclusive Inclusion = iota
	// Exclusive: a page lives in one tier; faulted pages move up and
	// reclaimed pages are demoted (the lower tier is a victim cache).
	Exclusive
)

// WriteMode decides when pages put into a link reach the lower tier.
type WriteMode int

const (
	// WriteBack keeps new pages dirty in the upper tier and writes them
	// back after DirtyExpire or when dirty bytes exceed DirtyRatio.
	WriteBack WriteMode = iota
	// WriteThrough writes to the lower tier before the put returns.
	WriteThrough
)

// Policy configures a link.
type Policy struct {
	Inclusion Inclusion
	Write     WriteMode
	// DirtyRatio: writeback starts when dirty bytes exceed this fraction
	// of the upper capacity (default 0.1); puts block (throttle) above
	// twice the ratio.
	DirtyRatio float64
	// DirtyExpire: dirty pages older than this are written back
	// (default 30s).
	DirtyExpire time.Duration
	// Readahead is the number of concurrent background page-ins of
	// Prefetch (default 8).
	Readahead int
	// NoPromote serves faults from the lower tier without keeping the
	// page in the upper one (read once, e.g. streaming restores).
	NoPromote bool
	// Interval is the writeback check interval (default 2s).
	Interval time.Duration
	// Parallel bounds concurrent writebacks (default 8).
	Parallel int
}

func (p Policy) withDefaults() Policy {
	if p.DirtyRatio <= 0 {
		p.DirtyRatio = 0.1
	}
	if p.DirtyExpire <= 0 {
		p.DirtyExpire = 30 * time.Second
	}
	if p.Readahead <= 0 {
		p.Readahead = 8
	}
	if p.Interval <= 0 {
		p.Interval = 2 * time.Second
	}
	if p.Parallel <= 0 {
		p.Parallel = 8
	}
	return p
}

// Link stacks an upper tier on a lower one; the pair is a Tier again,
// so stacks fold recursively.
type Link struct {
	upper, lower Tier
	pol          Policy
	obs          Observer
	now          func() time.Time

	mu       sync.Mutex
	dirty    map[Key]dirtyEntry
	dirtyB   int64
	inflight map[Key]*fetch
	st       Stats
	faults   rate
	wbMu     sync.Mutex // one writeback pass at a time
	stop     chan struct{}
	stopped  chan struct{}
}

type dirtyEntry struct {
	size  int64
	since time.Time
}

type fetch struct {
	done chan struct{}
	b    []byte
	err  error
}

// NewLink stacks upper over lower. Dirty pages the upper tier already
// holds (a restart) are adopted for writeback. Call Start to run the
// periodic writeback and Close to stop it.
func NewLink(upper, lower Tier, pol Policy, obs Observer) *Link {
	l := &Link{upper: upper, lower: lower, pol: pol.withDefaults(), obs: obs, now: time.Now,
		dirty: map[Key]dirtyEntry{}, inflight: map[Key]*fetch{}}
	if d, ok := upper.(interface{ DirtyKeys() []DirtyPage }); ok {
		for _, p := range d.DirtyKeys() {
			l.dirty[p.Key] = dirtyEntry{p.Size, p.Since}
			l.dirtyB += p.Size
		}
	}
	if pol.Inclusion == Exclusive {
		if lo, ok := upper.(*Local); ok {
			lo.SetDemote(func(k Key, b []byte) {
				if err := lower.Put(context.Background(), k, b, Clean); err == nil {
					l.mu.Lock()
					l.st.PageOuts++
					l.st.PageOutBytes += int64(len(b))
					l.mu.Unlock()
				}
			})
		}
	}
	return l
}

// Upper and Lower return the linked tiers.
func (l *Link) Upper() Tier { return l.upper }
func (l *Link) Lower() Tier { return l.lower }

// Start runs the periodic writeback until Close.
func (l *Link) Start() {
	l.mu.Lock()
	if l.stop != nil {
		l.mu.Unlock()
		return
	}
	l.stop, l.stopped = make(chan struct{}), make(chan struct{})
	l.mu.Unlock()
	go func() {
		defer close(l.stopped)
		t := time.NewTicker(l.pol.Interval)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				l.writeback(context.Background(), false)
			}
		}
	}()
}

// Close stops the periodic writeback (dirty pages stay dirty; Flush
// first to write them back).
func (l *Link) Close() {
	l.mu.Lock()
	stop, stopped := l.stop, l.stopped
	l.stop = nil
	l.mu.Unlock()
	if stop != nil {
		close(stop)
		<-stopped
	}
}

// Get implements Tier: a miss of the upper tier is a page fault served
// from the lower one (concurrent faults of a page share one page-in).
func (l *Link) Get(ctx context.Context, k Key) ([]byte, error) {
	b, err := l.upper.Get(ctx, k)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return b, err
	}
	return l.fault(ctx, k)
}

func (l *Link) fault(ctx context.Context, k Key) ([]byte, error) {
	l.mu.Lock()
	if f := l.inflight[k]; f != nil {
		l.mu.Unlock()
		select {
		case <-f.done:
			return f.b, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &fetch{done: make(chan struct{})}
	l.inflight[k] = f
	l.st.Faults++
	l.mu.Unlock()
	l.faults.add(l.now(), 1)
	if lo, ok := l.upper.(*Local); ok {
		lo.NoteFault()
	}
	begin := time.Now()
	f.b, f.err = l.lower.Get(ctx, k)
	if f.err == nil {
		l.mu.Lock()
		l.st.PageIns++
		l.st.PageInBytes += int64(len(f.b))
		l.mu.Unlock()
		l.obs.emit(Event{Type: EvPageIn, Tier: l.lower.Profile().Name, Pages: 1, Bytes: int64(len(f.b)), Seconds: time.Since(begin).Seconds()})
		if !l.pol.NoPromote {
			if err := l.upper.Put(ctx, k, f.b, Clean); err == nil && l.pol.Inclusion == Exclusive {
				if r, ok := l.lower.(Remover); ok {
					r.Remove(k)
				}
			}
		}
	}
	l.mu.Lock()
	delete(l.inflight, k)
	l.mu.Unlock()
	close(f.done)
	return f.b, f.err
}

// Put implements Tier. Write-through puts reach the lower tier first;
// write-back puts stay dirty in the upper tier until writeback (the
// state argument is the caller's: every page put into a write-back link
// is dirty relative to its lower tier).
func (l *Link) Put(ctx context.Context, k Key, b []byte, s State) error {
	if l.pol.Write == WriteThrough {
		if err := l.lower.Put(ctx, k, b, s); err != nil {
			return err
		}
		l.count(int64(len(b)), true)
		return l.upper.Put(ctx, k, b, Clean)
	}
	if err := l.upper.Put(ctx, k, b, Dirty); err != nil {
		return err
	}
	l.mu.Lock()
	if _, ok := l.dirty[k]; !ok {
		l.dirty[k] = dirtyEntry{int64(len(b)), l.now()}
		l.dirtyB += int64(len(b))
	}
	over := l.limit() > 0 && l.dirtyB > 2*l.limit()
	l.mu.Unlock()
	if over {
		l.writeback(ctx, false) // throttle the writer
	}
	return nil
}

// limit is the dirty byte threshold (0: no capacity, expiry only).
func (l *Link) limit() int64 {
	c := l.upper.Profile().Capacity
	if c <= 0 {
		return 0
	}
	return int64(float64(c) * l.pol.DirtyRatio)
}

func (l *Link) count(bytes int64, writeback bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st.PageOuts++
	l.st.PageOutBytes += bytes
	if writeback {
		l.st.Writebacks++
	}
}

// Has implements Tier.
func (l *Link) Has(ctx context.Context, k Key) bool {
	return l.upper.Has(ctx, k) || l.lower.Has(ctx, k)
}

// Reclaim implements Tier (the upper tier's residency).
func (l *Link) Reclaim(ctx context.Context, target int64) (int64, error) {
	return l.upper.Reclaim(ctx, target)
}

// Profile implements Tier: the composite profile of the stack.
func (l *Link) Profile() Profile { return Compose(l.upper.Profile(), l.lower.Profile()) }

// Discard implements Discarder: a dirty page is dropped without
// writeback (the file was deleted before flush).
func (l *Link) Discard(k Key) {
	l.mu.Lock()
	if d, ok := l.dirty[k]; ok {
		delete(l.dirty, k)
		l.dirtyB -= d.size
	}
	l.mu.Unlock()
	if d, ok := l.upper.(Discarder); ok {
		d.Discard(k)
	}
	if d, ok := l.lower.(Discarder); ok {
		d.Discard(k)
	}
}

// Pin implements Pinner.
func (l *Link) Pin(k Key, pinned bool) {
	if p, ok := l.upper.(Pinner); ok {
		p.Pin(k, pinned)
	}
}

// Dirty returns the dirty bytes awaiting writeback through this link.
func (l *Link) Dirty() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dirtyB
}

// Flush implements Flusher: every dirty page is written back, then the
// lower tier is flushed.
func (l *Link) Flush(ctx context.Context) error {
	if err := l.writeback(ctx, true); err != nil {
		return err
	}
	if f, ok := l.lower.(Flusher); ok {
		return f.Flush(ctx)
	}
	return nil
}

// FlushKeys writes back the given pages (if dirty) and flushes them
// through the lower tiers, so they are durable when it returns.
func (l *Link) FlushKeys(ctx context.Context, keys []Key) error {
	if err := l.writebackKeys(ctx, keys); err != nil {
		return err
	}
	if f, ok := l.lower.(interface {
		FlushKeys(context.Context, []Key) error
	}); ok {
		return f.FlushKeys(ctx, keys)
	}
	return nil
}

// writeback writes back expired dirty pages and, above the dirty
// threshold, the oldest until half of it is left (all: every page).
func (l *Link) writeback(ctx context.Context, all bool) error {
	l.mu.Lock()
	now := l.now()
	type cand struct {
		k Key
		dirtyEntry
	}
	var cs []cand
	for k, d := range l.dirty {
		cs = append(cs, cand{k, d})
	}
	l.mu.Unlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].since.Before(cs[j].since) })
	var keys []Key
	var total int64
	for _, c := range cs {
		total += c.size
	}
	limit := l.limit()
	for _, c := range cs {
		if all || now.Sub(c.since) >= l.pol.DirtyExpire || (limit > 0 && total > limit/2) {
			keys = append(keys, c.k)
			total -= c.size
		}
	}
	return l.writebackKeys(ctx, keys)
}

func (l *Link) writebackKeys(ctx context.Context, keys []Key) error {
	if len(keys) == 0 {
		return nil
	}
	l.wbMu.Lock()
	defer l.wbMu.Unlock()
	begin := time.Now()
	sem := make(chan struct{}, l.pol.Parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var pages, bytes int64
	for _, k := range keys {
		l.mu.Lock()
		d, ok := l.dirty[k]
		l.mu.Unlock()
		if !ok {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(k Key, d dirtyEntry) {
			defer func() { <-sem; wg.Done() }()
			b, err := peek(ctx, l.upper, k)
			if err == nil {
				err = l.lower.Put(ctx, k, b, Dirty)
			}
			if errors.Is(err, ErrNotFound) {
				err = nil // discarded meanwhile
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			l.mu.Lock()
			if cur, ok := l.dirty[k]; ok && cur.since.Equal(d.since) {
				delete(l.dirty, k)
				l.dirtyB -= d.size
			}
			l.st.PageOuts++
			l.st.PageOutBytes += d.size
			l.st.Writebacks++
			l.mu.Unlock()
			if c, ok := l.upper.(Cleaner); ok {
				c.MarkClean(k)
			}
			mu.Lock()
			pages++
			bytes += d.size
			mu.Unlock()
		}(k, d)
	}
	wg.Wait()
	if pages > 0 {
		l.obs.emit(Event{Type: EvWriteback, Tier: l.lower.Profile().Name, Pages: pages, Bytes: bytes, Seconds: time.Since(begin).Seconds()})
	}
	return firstErr
}

// peek reads a page without counting it as an access where possible.
func peek(ctx context.Context, t Tier, k Key) ([]byte, error) {
	if p, ok := t.(interface {
		Peek(Key) ([]byte, error)
	}); ok {
		return p.Peek(k)
	}
	return t.Get(ctx, k)
}

// Prefetch implements Prefetcher: pages not resident in the upper tier
// are paged in in the background (readahead) with the policy's
// concurrency.
func (l *Link) Prefetch(ctx context.Context, keys []Key) {
	var todo []Key
	for _, k := range keys {
		if !l.upper.Has(ctx, k) {
			todo = append(todo, k)
		}
	}
	if len(todo) == 0 {
		return
	}
	go func() {
		sem := make(chan struct{}, l.pol.Readahead)
		var wg sync.WaitGroup
		for _, k := range todo {
			if ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(k Key) {
				defer func() { <-sem; wg.Done() }()
				l.Get(ctx, k)
			}(k)
		}
		wg.Wait()
	}()
}

// Stats implements Statter: the upper tier's residency with this link's
// fault, page-in and writeback counters.
func (l *Link) Stats() Stats {
	s := StatsOf(l.upper)
	l.mu.Lock()
	s.Faults, s.PageIns, s.PageInBytes = l.st.Faults, l.st.PageIns, l.st.PageInBytes
	s.PageOuts, s.PageOutBytes, s.Writebacks = l.st.PageOuts, l.st.PageOutBytes, l.st.Writebacks
	l.mu.Unlock()
	s.FaultRate = l.faults.get(l.now())
	return s
}

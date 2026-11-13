package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/traumventures/offshore/internal/access"
	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/hostfs"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/store"
)

// Build slots on anyfs (internal/anyfs).
//
// A slot's data directory (slot/data, bound into sessions by cacheBinds)
// is a kernel overlayfs: a slot-local upper directory over a read-only
// FUSE lower (slot/lower) serving the tree of the repository's committed
// layer chain. File contents are materialized by the machine-wide Files
// cache (<cache>/anyfs/files) from the pages of the repository's tier
// stack: a local tier (<cache>/anyfs/repos/<repo>/local) over the
// repository's store prefix (repos/<repo>/ of the store grant). Without a
// kernel overlay (no CAP_SYS_ADMIN) the userspace overlay serves the
// same union; without FUSE the slot is a plain directory (a new slot
// over a committed chain is extracted eagerly).
//
// Writeback is a commit: the merged view is scanned against the head
// chain's tree (unchanged files reuse their pages), changed files become
// dirty pages of the local tier and a layer on top of the chain; the
// layer's pages are flushed to the store and the head moves there
// (heads/slot of the repository prefix). Commits run debounced when a
// slot is idle, right after a session when its upper grew beyond the
// dirty threshold, on save, drain and stop. Pages of files deleted
// before a flush are discarded unwritten. Deep chains are compacted into
// a new base, which also rewrites pages older than anyfs.RefreshAge.
// A rebase (fresh upper over the new head, committed upper files moved
// into Files) runs on idle slots whose upper is large and under disk
// pressure; reclaim drops clean pages and materialized files (faulted
// in again on use). A second slot of a repository is another overlay over
// the same committed chain.

// fsMode is how slots are mounted.
type fsMode int

const (
	fsKernel fsMode = iota // kernel overlayfs over a FUSE lower
	fsUser                 // userspace overlay (FUSE)
	fsPlain                // plain directory (no FUSE)
)

func (m fsMode) String() string {
	return [...]string{"kernel", "user", "plain"}[m]
}

// headName is the head of a repository's slot lineage.
const headName = "slot"

// prefetchSmall: files up to this size are faulted in at mount (cargo
// reads hundreds of fingerprints serially).
const prefetchSmall = 64 << 10

// cacheFSOptions configure the slot file system.
type cacheFSOptions struct {
	// Mode forces a mount mode (nil: probe the host).
	Mode *fsMode
	// Capacity bounds the local tiers (materialized files and pages) of
	// all repositories together (0: unbounded); see enforceCapacity.
	Capacity int64
	// Idle is how long a slot stays unused before it is committed
	// ([writeback] dirty_expire, default 30s).
	Idle time.Duration
	// DirtyBytes: a session that grows the upper by more than this is
	// committed right after it ends (0: idle commits only).
	DirtyBytes int64
	// CompactDepth: chains deeper than this are compacted (default 16).
	CompactDepth int
	// RebaseBytes: idle slots whose upper exceeds this are rebased
	// (default 4 GiB).
	RebaseBytes int64
	Log         *slog.Logger
	Now         func() time.Time
	// Observer receives the stacks' events (page-ins, writebacks).
	Observer anyfs.Observer
	// WritebackInterval is how often dirty pages are checked against
	// their class's writeback delay (default 2s).
	WritebackInterval time.Duration
	// FlushDeadline bounds how long a committed head waits before it is
	// flushed to the store (default headFlushDeadline).
	FlushDeadline time.Duration
	// HashBytesPerSec bounds the background hashing of written files
	// while the machine's IO is under pressure (0: defaultHashBytesPerSec;
	// negative: unbounded). The default keeps hashing from competing with
	// an IO-bound build on slow disks such as gp3; otherwise (outputs in
	// the page cache) hashing runs at full speed on idle cores.
	HashBytesPerSec float64
	// ReadWorkers bounds the files a commit reads at once (0:
	// anyfs.DefaultReadWorkers).
	ReadWorkers int
}

// defaultHashBytesPerSec bounds background hashing during sessions under
// IO pressure: on a gp3-like disk unbounded hashing doubled the session
// time in the bench.
const defaultHashBytesPerSec = 32 << 20

func hashRate(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return defaultHashBytesPerSec
	}
	return v
}

// cacheFS mounts and commits the build slots of a machine.
type cacheFS struct {
	dir        string
	opt        cacheFSOptions
	log        *slog.Logger
	files      *anyfs.Files
	mode       fsMode
	reflink    bool
	allowOther bool
	reclaimed  atomic.Int64 // bytes of materialized files reclaimed
	obs        anyfs.Observer

	mu     sync.Mutex
	repos  map[string]*repoFS
	mounts map[string]*slotMount
	heads  map[string]proto.LayerHead // repo -> last flushed head
	snapMu map[string]*sync.Mutex     // slot -> snapshot/commit lock
	hashed map[string]*anyfs.Hasher   // slot -> background hasher of its upper
	queued map[string]bool            // slot -> a session ended uncaptured (queueCapture)
}

// repoFS is a repository's tier stack and layer store.
type repoFS struct {
	id     string
	c      *cacheFS
	local  *anyfs.Local
	layers *blobcache.DirStore // local copies of layer objects and heads
	tier   repoTier
	cmu    sync.Mutex // one commit or flush at a time

	mu      sync.Mutex
	remote  blobcache.Store
	storeNm string
	expires int64
	link    *anyfs.Link
	// seed is a read-only source below remote (see seed.go); seedPending
	// makes the next flush refresh every page the own prefix lacks.
	seed        blobcache.Store
	seedPending bool
	primary     blobcache.Store // remote without the seed

	access *access.Model // access classes of the repository's files
	ws     *workingSet   // files read in recent sessions
}

// slotMount is a mounted slot.
type slotMount struct {
	repo  *repoFS
	base  string
	src   *anyfs.TreeSource
	lower *anyfs.LowerFS // kernel mode: the lazy (digest view) layer
	user  *anyfs.UserOverlay
	// cancel stops the readahead (nil: none).
	cancel context.CancelFunc
	// reads tracks the files sessions read (nil: not tracked).
	reads *readTracker
}

func newCacheFS(dir string, opt cacheFSOptions) (*cacheFS, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Idle <= 0 {
		opt.Idle = 30 * time.Second
	}
	if opt.FlushDeadline <= 0 {
		opt.FlushDeadline = headFlushDeadline
	}
	if opt.CompactDepth <= 0 {
		opt.CompactDepth = 16
	}
	if opt.RebaseBytes <= 0 {
		opt.RebaseBytes = 4 << 30
	}
	c := &cacheFS{dir: dir, opt: opt, log: opt.Log, repos: map[string]*repoFS{}, mounts: map[string]*slotMount{},
		heads: map[string]proto.LayerHead{}, allowOther: os.Geteuid() == 0}
	obs := func(e anyfs.Event) {
		if e.Type == anyfs.EvReclaim && e.Tier == "files" {
			c.reclaimed.Add(e.Bytes)
		}
		if opt.Observer != nil {
			opt.Observer(e)
		}
	}
	c.obs = obs
	c.cleanTrash()
	var err error
	if c.files, err = anyfs.OpenFiles(filepath.Join(dir, "files"), obs); err != nil {
		return nil, err
	}
	for _, d := range c.files.Digests() {
		c.files.SetBacked(d) // their pages are in a repository tier or store
	}
	c.reflink = hostfs.ProbeReflink(dir)
	if opt.Mode != nil {
		c.mode = *opt.Mode
	} else {
		c.mode = probeMode(dir, c.log)
	}
	c.log.Info("build slot file system", "mode", c.mode, "reflink", c.reflink)
	return c, nil
}

// probeMode picks the slot mount mode of the host.
func probeMode(dir string, log *slog.Logger) fsMode {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		log.Warn("no FUSE: build slots are plain directories (commits scan them)", "err", err)
		return fsPlain
	}
	if err := anyfs.KernelOverlaySupported(filepath.Join(dir, "probe")); err != nil {
		log.Warn("kernel overlayfs unavailable, build slots use the userspace overlay", "err", err)
		return fsUser
	}
	return fsKernel
}

// repo returns (opening) a repository's stack.
func (c *cacheFS) repo(id string) (*repoFS, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.repos[id]; r != nil {
		return r, nil
	}
	base := filepath.Join(c.dir, "repos", id)
	local, err := anyfs.OpenLocal(anyfs.LocalOptions{Dir: filepath.Join(base, "local"), Capacity: c.opt.Capacity, Now: c.opt.Now, Observer: c.obs,
		Pool: filepath.Join(c.dir, "pool")})
	if err != nil {
		return nil, err
	}
	layers, err := blobcache.NewDirStore(filepath.Join(base, "layers"), blobcache.DefaultTTL)
	if err != nil {
		return nil, err
	}
	r := &repoFS{id: id, c: c, local: local, layers: layers, access: loadAccess(base), ws: loadWorkingSet(filepath.Join(base, workingSetFile))}
	r.tier = repoTier{r}
	c.repos[id] = r
	return r, nil
}

// setGrant connects a repository's stack to its store prefix (a newer
// grant replaces the link; dirty pages are adopted by the new link).
func (c *cacheFS) setGrant(repo string, g store.Grant) error {
	r, err := c.repo(repo)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.link != nil && r.storeNm == g.Store && (r.expires == 0 || (g.Expires != 0 && g.Expires <= r.expires)) {
		r.mu.Unlock()
		return nil
	}
	st, err := store.Open(g, store.RepoScope(repo), blobcache.MaxTTL)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	r.primary, r.storeNm, r.expires = st, g.Store, g.Expires
	r.mu.Unlock()
	return c.relink(r)
}

// relink (re)builds a repository's link to its store prefix (with the
// seed source below it, if any).
func (c *cacheFS) relink(r *repoFS) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primary == nil {
		return nil // connected by the grant that follows
	}
	var st blobcache.Store = r.primary
	if r.seed != nil {
		st = fallbackStore{primary: r.primary, source: r.seed}
	}
	r.remote = st
	if r.link != nil {
		r.link.Close()
	}
	// Committed pages are written back in the background by their access
	// class (PageHint): immutable early, churn late, ephemeral only by an
	// explicit flush; the store head moves once a head's pages are clean.
	r.link = anyfs.NewLink(r.local, anyfs.NewStore(st, anyfs.StoreMedium()), anyfs.Policy{Write: anyfs.WriteBack,
		DirtyExpire: access.DefaultWritebackDelay, Interval: c.opt.WritebackInterval}, r.c.obs)
	r.link.Start()
	return nil
}

// refreshAge is the age after which the repository's chain is rewritten:
// half its store's retention (anyfs.RefreshAge without a store).
func (r *repoFS) refreshAge() time.Duration {
	if _, _, remote, _ := r.stack(); remote != nil {
		return anyfs.RefreshAgeOf(remote)
	}
	return anyfs.RefreshAge
}

// stack returns the repository's current top tier, link and remote.
func (r *repoFS) stack() (anyfs.Tier, *anyfs.Link, blobcache.Store, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.link == nil {
		return r.local, nil, nil, ""
	}
	return r.link, r.link, r.remote, r.storeNm
}

// repoTier is a repository's stack as one Tier whose link may change
// (grants arrive and renew while slots are mounted).
type repoTier struct{ r *repoFS }

func (t repoTier) top() anyfs.Tier { tt, _, _, _ := t.r.stack(); return tt }
func (t repoTier) Get(ctx context.Context, k anyfs.Key) ([]byte, error) {
	return t.top().Get(ctx, k)
}
func (t repoTier) Put(ctx context.Context, k anyfs.Key, b []byte, s anyfs.State) error {
	return t.top().Put(ctx, k, b, s)
}
func (t repoTier) PutRange(ctx context.Context, k anyfs.Key, b []byte, rg anyfs.Range, s anyfs.State) error {
	return anyfs.PutPage(ctx, t.top(), k, b, rg, s)
}
func (t repoTier) Has(ctx context.Context, k anyfs.Key) bool { return t.top().Has(ctx, k) }
func (t repoTier) Reclaim(ctx context.Context, n int64) (int64, error) {
	return t.top().Reclaim(ctx, n)
}
func (t repoTier) Profile() anyfs.Profile { return t.top().Profile() }
func (t repoTier) Discard(k anyfs.Key) {
	if d, ok := t.top().(anyfs.Discarder); ok {
		d.Discard(k)
	}
}

// chain loads a committed chain by its top layer id ("" : empty),
// copying layer objects from the store when the machine lacks them.
func (r *repoFS) chain(ctx context.Context, id string) (anyfs.Chain, error) {
	if id == "" {
		return nil, nil
	}
	c, err := anyfs.LoadChain(ctx, r.layers, id)
	if err == nil {
		return c, nil
	}
	_, _, remote, _ := r.stack()
	if remote == nil {
		return nil, err
	}
	c, rerr := anyfs.LoadChain(ctx, remote, id)
	if rerr != nil {
		return nil, fmt.Errorf("%w (store: %v)", err, rerr)
	}
	for _, l := range c {
		if err := anyfs.SaveLayer(ctx, r.layers, l); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// head returns the repository's committed head on this machine ("" if
// none or older than ttl).
func (c *cacheFS) head(ctx context.Context, repo string, ttl time.Duration) (anyfs.Chain, error) {
	r, err := c.repo(repo)
	if err != nil {
		return nil, err
	}
	id, err := anyfs.Head(ctx, r.layers, headName)
	if err != nil {
		return nil, nil
	}
	ch, err := r.chain(ctx, id)
	if err != nil || len(ch) == 0 || c.opt.Now().Sub(ch.Oldest()) >= ttl {
		return nil, err
	}
	return ch, nil
}

// prepare makes a leased slot's data directory usable: a fresh slot
// starts over the repository's committed head (no copy), then the slot
// is mounted. It reports whether the slot holds previous build data.
func (c *cacheFS) prepare(ctx context.Context, sl *slots, slot, repo string, ttl time.Duration) (bool, error) {
	m, err := readMeta(slot)
	if err != nil {
		return false, err
	}
	fresh := m.Base == "" && m.Head == "" && m.Runs == 0 && m.Committed == 0
	if fresh {
		ch, err := c.head(ctx, repo, ttl)
		if err != nil {
			c.log.Warn("committed head unreadable, starting cold", "repo", repo, "err", err)
		}
		if top := ch.Top(); top != nil {
			m.Base, m.Head, m.Created = top.ID, top.ID, ch.Oldest().Unix()
			m.Committed, m.Bytes = m.LastUsed, top.Bytes
			if err := sl.update(slot, func(x *slotMeta) {
				x.Base, x.Head, x.Created, x.Committed, x.Bytes = m.Base, m.Head, m.Created, m.Committed, m.Bytes
				x.Clean = x.Leases - 1 // the head is the state before this session
			}); err != nil {
				return false, err
			}
			if c.mode == fsPlain {
				r, _ := c.repo(repo)
				if err := anyfs.Extract(ctx, r.tier, ch.Tree(), filepath.Join(slot, "data")); err != nil {
					return false, fmt.Errorf("extract committed slot: %w", err)
				}
			}
		}
	}
	if err := c.mount(ctx, slot, repo, m.Base); err != nil {
		return false, err
	}
	return m.Base != "" || !fresh, nil
}

// mount mounts a slot over the chain base (no-op when mounted or plain).
func (c *cacheFS) mount(ctx context.Context, slot, repo, base string) error {
	if c.mode == fsPlain {
		return os.MkdirAll(filepath.Join(slot, "data"), 0o755)
	}
	data := filepath.Join(slot, "data")
	c.mu.Lock()
	sm := c.mounts[slot]
	c.mu.Unlock()
	if sm != nil && sm.base == base && anyfs.IsMountpoint(data) {
		return nil
	}
	c.unmount(slot)
	r, err := c.repo(repo)
	if err != nil {
		return err
	}
	ch, err := r.chain(ctx, base)
	if err != nil {
		return err
	}
	src := anyfs.NewTreeSource(ch.Tree(), c.files, r.tier)
	sm = &slotMount{repo: r, base: base, src: src, reads: newReadTracker(data)}
	upper, work := filepath.Join(slot, "upper"), filepath.Join(slot, "work")
	h := c.hasher(slot, r)
	sm.reads.onWrite = func(rel string) { h.Queue(rel, filepath.Join(upper, filepath.FromSlash(rel))) }
	mounted := false
	if c.mode == fsKernel {
		// Metadata layer + materialized files + lazy FUSE (digest
		// view): resident content never goes through the daemon.
		var ks *anyfs.Slot
		ks, err = anyfs.MountSlot(ctx, data, upper, work, filepath.Join(slot, "meta"), filepath.Join(slot, "lower"), src,
			anyfs.SlotOptions{Lower: anyfs.LowerOptions{FsName: "offshore-slot", PrefetchMax: prefetchSmall, AllowOther: c.allowOther}})
		if err == nil {
			sm.lower = ks.Lazy
			mounted = true
		} else if !dirEmpty(upper) {
			return fmt.Errorf("mount build slot: %w", err)
		} else {
			c.log.Warn("kernel overlay mount failed, using the userspace overlay", "slot", slot, "err", err)
		}
	}
	if !mounted {
		sm.user, err = anyfs.MountUserOverlay(data, upper, src, anyfs.UserOverlayOptions{FsName: "offshore-slot", AllowOther: c.allowOther, Timeout: time.Minute,
			OnOpen: sm.reads.onOpen})
		if err != nil {
			return fmt.Errorf("mount build slot: %w", err)
		}
		go src.PrefetchSmall(context.Background(), prefetchSmall, 16)
	}
	if mounted {
		if err := sm.reads.watchKernel(); err != nil {
			c.log.Warn("build slot reads not tracked", "slot", slot, "err", err)
			sm.reads = nil
		}
	}
	if dirEmpty(upper) {
		var meta []byte
		if top := ch.Top(); top != nil {
			meta = top.Meta
		}
		c.readahead(sm, meta) // fresh over a head: warm what the next build reads
	}
	c.mu.Lock()
	c.mounts[slot] = sm
	c.mu.Unlock()
	return nil
}

func dirEmpty(d string) bool {
	des, err := os.ReadDir(d)
	return err != nil || len(des) == 0
}

// unmount unmounts a slot (lazily if busy); safe on unmounted slots.
func (c *cacheFS) unmount(slot string) {
	c.mu.Lock()
	sm := c.mounts[slot]
	delete(c.mounts, slot)
	c.mu.Unlock()
	data := filepath.Join(slot, "data")
	if sm != nil && sm.cancel != nil {
		sm.cancel()
	}
	if sm != nil && sm.reads != nil {
		sm.reads.close()
	}
	if sm != nil && sm.user != nil {
		sm.user.Unmount()
	} else if anyfs.IsMountpoint(data) {
		anyfs.Unmount(data)
	}
	if sm != nil && sm.lower != nil {
		sm.lower.Unmount()
	} else if l := filepath.Join(slot, "lower"); anyfs.IsMountpoint(l) {
		anyfs.Unmount(l)
	}
}

// unmountAll unmounts every slot (shutdown).
func (c *cacheFS) unmountAll() {
	c.mu.Lock()
	var ss []string
	for s := range c.mounts {
		ss = append(ss, s)
	}
	hs := c.hashed
	c.hashed = nil
	c.mu.Unlock()
	for _, s := range ss {
		c.unmount(s)
	}
	for _, h := range hs {
		h.Close()
	}
}

// hasher returns (starting) a slot's background hasher: files written in
// the slot's upper directory are hashed once they settled, their pages
// put into the repository's tier (written back by their access class
// during the session), so a commit looks their digests up instead of
// reading them (plan). Ephemeral classes are not hashed.
func (c *cacheFS) hasher(slot string, r *repoFS) *anyfs.Hasher {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h := c.hashed[slot]; h != nil {
		return h
	}
	ctx := context.Background()
	h := anyfs.NewHasher(anyfs.HasherOptions{
		Sink: func(k anyfs.Key, b []byte) error { return r.tier.Put(ctx, k, b, anyfs.Dirty) },
		// Pages are reflinks of the hashed upper files where the cache
		// file system supports it: the build outputs are not written a
		// second time (they were: twice the disk writes and space).
		RangeSink: func(k anyfs.Key, b []byte, rg anyfs.Range) error {
			return anyfs.PutPage(ctx, r.tier, k, b, rg, anyfs.Dirty)
		},
		Skip: func(rel string) bool { return r.access.Policy(rel).Class == access.Ephemeral },
		OnHashed: func(n *anyfs.Node) {
			hint := pageHint(r.access.Policy(n.Path))
			for _, p := range n.Pages {
				r.local.Hint(p.Key, hint)
			}
		},
		BytesPerSec: hashRate(c.opt.HashBytesPerSec),
		Now:         c.opt.Now,
	})
	if c.hashed == nil {
		c.hashed = map[string]*anyfs.Hasher{}
	}
	c.hashed[slot] = h
	return h
}

// slotHasher returns a slot's background hasher (nil: none).
func (c *cacheFS) slotHasher(slot string) *anyfs.Hasher {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hashed[slot]
}

// removeSlot deletes a slot directory, unmounting it first.
func (c *cacheFS) removeSlot(slot string) {
	if c != nil {
		c.unmount(slot)
		c.mu.Lock()
		h := c.hashed[slot]
		delete(c.hashed, slot)
		c.mu.Unlock()
		if h != nil {
			h.Close()
		}
	} else {
		for _, d := range []string{"data", "lower"} {
			if p := filepath.Join(slot, d); anyfs.IsMountpoint(p) {
				anyfs.Unmount(p)
			}
		}
	}
	os.RemoveAll(slot)
}

// commitResult is a commit of a slot.
type commitResult struct {
	repo    *repoFS
	chain   anyfs.Chain // the head chain after the commit
	tree    *anyfs.Tree
	changed bool
	refresh bool // compacted: rewrite old pages in the store
	scan    anyfs.ScanStats
	upper   int64 // upper bytes at the commit
	scanned int64 // watermark of the commit (unix ns)
}

// commit scans a slot (the caller holds it) and commits its changes on
// top of the slot's head. live are the layer ids other slots of the
// repository mount or start from (their pages are never discarded).
func (c *cacheFS) commit(ctx context.Context, slot, repo string, m slotMeta, live []string) (commitResult, error) {
	r, err := c.repo(repo)
	if err != nil {
		return commitResult{}, err
	}
	r.cmu.Lock()
	defer r.cmu.Unlock()
	p, err := c.plan(ctx, slot, r, m)
	if err != nil {
		return commitResult{repo: r}, err
	}
	res, err := c.finish(ctx, slot, r, m, p, filepath.Join(slot, "data"), false, live)
	c.release(slot, p, err == nil)
	return res, err
}

// slotPlan is a planned commit of a slot: what changed since its head
// (files not read yet), the reads since the last commit and the
// watermark. Snapshots persist it.
type slotPlan struct {
	Head       string           `json:"head"` // the layer the plan is relative to
	Changes    *anyfs.ChangeSet `json:"changes"`
	Reads      []string         `json:"reads,omitempty"`
	ReadsKnown bool             `json:"reads_known,omitempty"`
	Scanned    int64            `json:"scanned_ns"`
	Upper      int64            `json:"upper_bytes,omitempty"`
	// Resolved are the changed files whose digests the background hasher
	// knew (their pages are in the tier; the hasher holds them).
	Resolved []string `json:"resolved,omitempty"`
	Runs     int64    `json:"runs"`   // the slot's runs at the plan
	Leases   int64    `json:"leases"` // the slot's leases at the plan
}

// plan stats a slot's changes since its head (the caller holds the slot
// and r.cmu or the slot's snapshot lock).
func (c *cacheFS) plan(ctx context.Context, slot string, r *repoFS, m slotMeta) (slotPlan, error) {
	head := m.Head
	if head == "" {
		head = m.Base
	}
	p := slotPlan{Head: head, Runs: m.Runs, Leases: m.Leases}
	ch, err := r.chain(ctx, head)
	if err != nil {
		return p, err
	}
	if err := c.mount(ctx, slot, r.id, m.Base); err != nil {
		return p, err
	}
	opt := anyfs.ChangeOptions{ScanOptions: c.scanOptions(ctx, r, ch.Tree(), m.Scanned)}
	if c.mode != fsPlain {
		opt.Upper = filepath.Join(slot, "upper")
		if opt.Since == 0 && head == m.Base {
			opt.Since = 1 // the upper holds every change since the mount over the head
		}
		c.mu.Lock()
		if sm := c.mounts[slot]; sm != nil {
			if sm.user != nil {
				opt.UpperFormat = anyfs.UpperUser
			}
			// Metacopy entries (renamed or chmod'ed lower files) keep
			// the content of the lower file they redirect to: the
			// mounted chain's digests and pages, unread.
			opt.Lower = sm.src.Tree()
		}
		c.mu.Unlock()
	}
	// Changes from now on have a newer ctime (a second of slack for
	// coarse file system timestamps).
	p.Scanned = time.Now().Add(-time.Second).UnixNano()
	if t := c.tracker(slot); t != nil {
		p.Reads, p.ReadsKnown = t.takePending(), true
	}
	if p.Changes, err = anyfs.PlanChanges(filepath.Join(slot, "data"), opt); err != nil {
		return p, fmt.Errorf("scan build slot: %w", err)
	}
	if c.mode != fsPlain {
		p.Upper = treeSize(filepath.Join(slot, "upper"))
		if h := c.slotHasher(slot); h != nil {
			// Files hashed in the background while the session wrote
			// them (unchanged since: same inode, size, mtime and ctime)
			// are neither captured nor read again.
			upper := filepath.Join(slot, "upper")
			p.Resolved = p.Changes.Resolve(func(n *anyfs.Node) bool {
				return h.Lookup(n, filepath.Join(upper, filepath.FromSlash(n.Path)))
			})
			// The commit reads the rest itself (at full speed): hashing
			// them in the background too would read them twice.
			h.Forget(p.Changes.Read)
		}
	}
	return p, nil
}

// release ends the hasher holds of a plan's resolved files (committed:
// the commit referencing their pages is done).
func (c *cacheFS) release(slot string, p slotPlan, committed bool) {
	if h := c.slotHasher(slot); h != nil && len(p.Resolved) > 0 {
		h.Release(p.Resolved, committed)
	}
}

// scanOptions are the scan options of a repository's commits over prev.
func (c *cacheFS) scanOptions(ctx context.Context, r *repoFS, prev *anyfs.Tree, since int64) anyfs.ScanOptions {
	return anyfs.ScanOptions{Prev: prev, Since: since, Workers: c.opt.ReadWorkers,
		Sink: func(k anyfs.Key, b []byte) error { return r.tier.Put(ctx, k, b, anyfs.Dirty) },
		RangeSink: func(k anyfs.Key, b []byte, rg anyfs.Range) error {
			return anyfs.PutPage(ctx, r.tier, k, b, rg, anyfs.Dirty)
		},
		Alone: func(rel string, size int64) bool { return size >= dedupeMin && r.access.Policy(rel).Dedupe }}
}

// finish reads a plan's changed files from content (the slot's data
// directory, or a snapshot of them with move: adopted by moving) and
// commits the tree as a layer on top of the plan's head (r.cmu held).
func (c *cacheFS) finish(ctx context.Context, slot string, r *repoFS, m slotMeta, p slotPlan, content string, move bool, live []string) (commitResult, error) {
	res := commitResult{repo: r, scanned: p.Scanned, upper: p.Upper}
	ch, err := r.chain(ctx, p.Head)
	if err != nil {
		return res, err
	}
	base := ch.Tree()
	opt := c.scanOptions(ctx, r, base, 0)
	var stream *anyfs.Stream
	if _, link, _, _ := r.stack(); link != nil {
		// New pages stream to the store while the files are read: the
		// commit's upload overlaps its hashing (the flush then finds them
		// clean).
		// Only files whose class is written back early (not churn,
		// ephemeral: they wait for their writeback delay or the flush).
		stream = link.Stream(ctx)
		put := func(k anyfs.Key, b []byte, rg anyfs.Range) error { return stream.PutRange(k, b, rg) }
		opt.FileSink = func(rel string) anyfs.RangeSink {
			if h := pageHint(r.access.Policy(rel)); h.Expire < 0 && !h.NoWriteback {
				return put
			}
			return nil
		}
	}
	tree, st, err := p.Changes.Tree(content, opt)
	if stream != nil {
		if ss := stream.Wait(); ss.Err != nil {
			c.log.Warn("stream commit pages to the store (left for the flush)", "repo", r.id, "failed", ss.Failed, "err", ss.Err)
		}
	}
	if err != nil {
		return res, fmt.Errorf("read build slot changes: %w", err)
	}
	res.scan, res.tree, res.chain = st, tree, ch
	diff := anyfs.Diff(base, tree)
	if len(diff) == 0 && (len(ch) > 0 || tree.Len() == 0) {
		return res, nil
	}
	now := c.opt.Now()
	c.observe(r, base, tree, p.Reads, p.ReadsKnown, now)
	c.hintPages(r, diff)
	l := anyfs.Commit(ch, base, tree, now, r.ws.marshal(wsMeta))
	if len(ch) == 0 && m.Created > 0 {
		l.Created = m.Created
	}
	ch = append(ch, l)
	if l.Depth >= c.opt.CompactDepth || now.Sub(time.Unix(ch[0].At, 0)) >= r.refreshAge() {
		b := anyfs.Compact(ch, now)
		if err := anyfs.SaveLayer(ctx, r.layers, l); err != nil {
			return res, err
		}
		l, ch, res.refresh = b, anyfs.Chain{b}, true
	}
	if err := anyfs.SaveLayer(ctx, r.layers, l); err != nil {
		return res, err
	}
	if err := anyfs.SetHead(ctx, r.layers, headName, l.ID); err != nil {
		return res, err
	}
	res.chain, res.changed = ch, true
	if move || (c.reflink && c.mode != fsPlain) {
		// Committed files become resident content without a fetch (a
		// rebase or another slot over the head reads them).
		dir := filepath.Join(slot, "upper")
		if move {
			dir = content
		}
		for i := range diff {
			n := &diff[i]
			if n.Kind != anyfs.File {
				continue
			}
			f := filepath.Join(dir, filepath.FromSlash(n.Path))
			if fullUpperFile(f, n.Size) {
				c.files.Adopt(n, f, true, move)
			}
		}
	}
	c.discard(ctx, r, append(live, l.ID))
	return res, nil
}

// fullUpperFile reports whether an upper file holds its data (not an
// overlayfs metacopy entry whose data is in the lower layer).
func fullUpperFile(p string, size int64) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != size {
		return false
	}
	_, err = unix.Lgetxattr(p, "trusted.overlay.metacopy", nil)
	return err != nil
}

// discard drops dirty pages no tree of the live layers references:
// files deleted (or rewritten) before their pages were flushed.
func (c *cacheFS) discard(ctx context.Context, r *repoFS, live []string) {
	dirty := r.local.DirtyKeys()
	if len(dirty) == 0 {
		return
	}
	keep := map[anyfs.Key]bool{}
	seen := map[string]bool{}
	for _, id := range live {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ch, err := r.chain(ctx, id)
		if err != nil {
			return // unknown references: keep everything
		}
		for _, k := range ch.Tree().Keys() {
			keep[k] = true
		}
	}
	c.mu.Lock()
	var hs []*anyfs.Hasher
	for slot, sm := range c.mounts {
		if sm.repo == r {
			for _, k := range sm.src.Tree().Keys() {
				keep[k] = true
			}
			if h := c.hashed[slot]; h != nil {
				hs = append(hs, h)
			}
		}
	}
	c.mu.Unlock()
	for _, h := range hs {
		// Pages hashed in the background, not committed yet.
		for _, k := range h.Keys() {
			keep[k] = true
		}
	}
	for _, d := range dirty {
		if !keep[d.Key] {
			r.tier.Discard(d.Key)
		}
	}
}

var errNoStore = errors.New("no store grant for the repository")

// dirtyIn counts the pages of a tree still awaiting writeback.
func (c *cacheFS) dirtyIn(r *repoFS, t *anyfs.Tree) int {
	dirty := r.local.DirtyKeys()
	if len(dirty) == 0 {
		return 0
	}
	set := make(map[anyfs.Key]bool, len(dirty))
	for _, d := range dirty {
		set[d.Key] = true
	}
	n := 0
	for _, k := range t.Keys() {
		if set[k] {
			n++
		}
	}
	return n
}

// flush writes a committed chain back to the store: the dirty pages of
// its tree, its layer objects, then the head.
//
// The upload of the dirty pages runs before r.cmu is taken, so a commit
// of the next session does not wait for it (minutes for a large head);
// under the lock the keys are flushed again (only pages dirtied or
// discarded meanwhile cost anything; a page a commit discarded fails the
// flush, which is retried with the newer head) before the layers and the
// head are published. Refreshes (compaction, seeds) stay under the lock.
func (c *cacheFS) flush(ctx context.Context, res commitResult) (proto.LayerHead, error) {
	r := res.repo
	begin := time.Now()
	var before anyfs.Stats
	if _, link, _, _ := r.stack(); link != nil {
		before = link.Stats()
		r.mu.Lock()
		seeded := r.seedPending
		r.mu.Unlock()
		if !res.refresh && res.tree != nil && !seeded {
			link.FlushKeys(ctx, res.tree.Keys()) // best effort: the locked pass decides
		}
	}
	r.cmu.Lock()
	defer r.cmu.Unlock()
	_, link, remote, storeNm := r.stack()
	top := res.chain.Top()
	if link == nil {
		return proto.LayerHead{}, errNoStore
	}
	if top == nil {
		return proto.LayerHead{}, errors.New("nothing committed")
	}
	keys := res.tree.Keys()
	r.mu.Lock()
	seeded := r.seedPending
	r.mu.Unlock()
	if res.refresh || seeded {
		st := anyfs.NewStore(remote, anyfs.StoreMedium())
		for _, k := range keys {
			if st.Has(ctx, k) {
				continue
			}
			b, err := link.Get(ctx, k)
			if err != nil {
				return proto.LayerHead{}, fmt.Errorf("refresh page %s: %w", k.Short(), err)
			}
			if err := link.Put(ctx, k, b, anyfs.Dirty); err != nil {
				return proto.LayerHead{}, err
			}
		}
	}
	if err := link.FlushKeys(ctx, keys); err != nil {
		return proto.LayerHead{}, err
	}
	for _, l := range res.chain {
		// Layer objects the store lacks (new, or deleted by the control
		// plane's garbage collection) or that would expire too soon.
		if info, err := remote.Stat(ctx, anyfs.LayerObject(l.ID)); err == nil && time.Since(info.ModTime) < anyfs.RefreshAgeOf(remote) {
			continue
		}
		if err := anyfs.SaveLayer(ctx, remote, l); err != nil {
			return proto.LayerHead{}, err
		}
	}
	if err := anyfs.SetHead(ctx, remote, headName, top.ID); err != nil {
		return proto.LayerHead{}, err
	}
	if seeded {
		// Every page and layer of the head is in the own prefix now.
		r.mu.Lock()
		r.seed, r.seedPending = nil, false
		r.mu.Unlock()
		if err := r.c.relink(r); err != nil {
			return proto.LayerHead{}, err
		}
	}
	after := link.Stats()
	h := proto.LayerHead{Repo: r.id, Store: storeNm, Layer: top.ID, Chain: res.chain.IDs(), Bytes: top.Bytes, Delta: top.Delta,
		Created: res.chain.Oldest().Unix(), At: top.At, Pages: after.PageOuts - before.PageOuts,
		PageBytes: after.PageOutBytes - before.PageOutBytes, Seconds: time.Since(begin).Seconds()}
	c.mu.Lock()
	c.heads[r.id] = h
	c.mu.Unlock()
	return h, nil
}

// flushedHeads returns the heads flushed by this worker.
func (c *cacheFS) flushedHeads() []proto.LayerHead {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]proto.LayerHead, 0, len(c.heads))
	for _, h := range c.heads {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// restore makes a head of the store the repository's head on this
// machine and returns its chain (a new slot mounts it lazily).
func (c *cacheFS) restore(ctx context.Context, repo, layer string, ttl time.Duration) (anyfs.Chain, error) {
	r, err := c.repo(repo)
	if err != nil {
		return nil, err
	}
	ch, err := r.chain(ctx, layer)
	if err != nil {
		return nil, err
	}
	if len(ch) == 0 {
		return nil, errors.New("empty layer chain")
	}
	if c.opt.Now().Sub(ch.Oldest()) >= ttl {
		return nil, errors.New("layer chain is older than the build slot TTL")
	}
	return ch, anyfs.SetHead(ctx, r.layers, headName, layer)
}

// rebase gives an idle, committed slot a fresh upper over its head: the
// upper's committed files move into Files (no refetch), the slot is
// mounted over the head chain. The caller holds the slot.
func (c *cacheFS) rebase(ctx context.Context, slot, repo string, m slotMeta) (string, error) {
	if c.mode == fsPlain || m.Head == "" || m.Head == m.Base {
		return m.Base, nil
	}
	r, err := c.repo(repo)
	if err != nil {
		return m.Base, err
	}
	ch, err := r.chain(ctx, m.Head)
	if err != nil {
		return m.Base, err
	}
	tree := ch.Tree()
	c.unmount(slot)
	upper := filepath.Join(slot, "upper")
	filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(upper, p)
		if n, ok := tree.Get(filepath.ToSlash(rel)); ok && n.Kind == anyfs.File && fullUpperFile(p, n.Size) {
			c.files.Adopt(n, p, true, true)
		}
		return nil
	})
	os.RemoveAll(upper)
	os.RemoveAll(filepath.Join(slot, "work"))
	return m.Head, c.mount(ctx, slot, repo, m.Head)
}

// reclaim frees up to want bytes of resident data: materialized files
// first (they are faulted in again from pages), then clean pages.
func (c *cacheFS) reclaim(ctx context.Context, want int64) int64 {
	if want <= 0 {
		return 0
	}
	c.coldByClass()
	fs := c.files.Stats().ResidentBytes
	freed := c.files.Reclaim(max(0, fs-want))
	c.mu.Lock()
	repos := make([]*repoFS, 0, len(c.repos))
	for _, r := range c.repos {
		repos = append(repos, r)
	}
	c.mu.Unlock()
	for _, r := range repos {
		if freed >= want {
			break
		}
		res := r.local.Stats().ResidentBytes
		n, _ := r.local.Reclaim(ctx, max(0, res-(want-freed)))
		freed += n
	}
	return freed
}

// openRepo returns a repository's stack if this process opened it.
func (c *cacheFS) openRepo(id string) *repoFS {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repos[id]
}

// openRepos returns the opened repository stacks.
func (c *cacheFS) openRepos() []*repoFS {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*repoFS, 0, len(c.repos))
	for _, r := range c.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// dropUnreferenced removes the clean pages and materialized files that no
// tree a slot mounts or starts from references any more (live: repository
// id -> layer ids of its slots; the local head and running hashers count
// too): pages of files a trim or a demoted slot deleted. They are in the
// store (clean), so nothing is lost. A repository busy committing or
// flushing is skipped; materialized files only go when every
// repository's references are known. It returns the bytes dropped.
func (c *cacheFS) dropUnreferenced(ctx context.Context, live map[string][]string) int64 {
	digests := map[string]bool{}
	complete := true
	var freed int64
	for _, r := range c.openRepos() {
		if !r.cmu.TryLock() {
			complete = false
			continue
		}
		keep, err := c.referenced(ctx, r, live[r.id], digests)
		if err == nil {
			freed += r.local.DropUnreferenced(func(k anyfs.Key) bool { return keep[k] })
		} else {
			complete = false
		}
		r.cmu.Unlock()
	}
	if complete {
		freed += c.files.DropUnreferenced(func(d string) bool { return digests[d] })
	}
	return freed
}

// referenced collects the pages (returned) and file digests (added to
// digests) of a repository's live trees: the layers in live, its local
// head, mounted chains and the pages its hashers hold (r.cmu held).
func (c *cacheFS) referenced(ctx context.Context, r *repoFS, live []string, digests map[string]bool) (map[anyfs.Key]bool, error) {
	keep := map[anyfs.Key]bool{}
	addTree := func(t *anyfs.Tree) {
		for _, k := range t.Keys() {
			keep[k] = true
		}
		for _, n := range t.Nodes() {
			if n.Kind == anyfs.File {
				if n.Digest == "" && n.Size == 0 {
					continue
				}
				digests[n.Digest] = true
			}
		}
	}
	if id, err := anyfs.Head(ctx, r.layers, headName); err == nil {
		live = append(live, id)
	}
	seen := map[string]bool{}
	for _, id := range live {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ch, err := r.chain(ctx, id)
		if err != nil {
			return nil, err // unknown references: keep everything
		}
		addTree(ch.Tree())
	}
	c.mu.Lock()
	var hs []*anyfs.Hasher
	var trees []*anyfs.Tree
	for slot, sm := range c.mounts {
		if sm.repo == r {
			trees = append(trees, sm.src.Tree())
			if h := c.hashed[slot]; h != nil {
				hs = append(hs, h)
			}
		}
	}
	c.mu.Unlock()
	for _, t := range trees {
		addTree(t)
	}
	for _, h := range hs {
		for _, k := range h.Keys() {
			keep[k] = true
		}
	}
	return keep, nil
}

// resident is the machine-wide resident bytes of the local tiers
// (materialized files plus each repository's pages, shared pages once).
func (c *cacheFS) resident() int64 {
	c.mu.Lock()
	repos := make([]*repoFS, 0, len(c.repos))
	for _, r := range c.repos {
		repos = append(repos, r)
	}
	c.mu.Unlock()
	n := c.files.Stats().ResidentBytes
	for _, r := range repos {
		st := r.local.Stats()
		n += st.ResidentBytes - st.SharedBytes
	}
	return n
}

// enforceCapacity reclaims resident data above the capacity. Capacity
// bounds the local tiers of all repositories together (each repository's
// tier alone is bounded by it too): per repository only, a machine with
// several repositories held a multiple of it.
func (c *cacheFS) enforceCapacity(ctx context.Context) int64 {
	if c.opt.Capacity <= 0 {
		return 0
	}
	over := c.resident() - c.opt.Capacity
	if over <= 0 {
		return 0
	}
	return c.reclaim(ctx, over)
}

// vm reports the slot file system in virtual memory terms.
func (c *cacheFS) vm() *proto.VMStats {
	c.mu.Lock()
	repos := make([]*repoFS, 0, len(c.repos))
	for _, r := range c.repos {
		repos = append(repos, r)
	}
	c.mu.Unlock()
	var s anyfs.Stats
	stack := "local"
	for _, r := range repos {
		t, link, _, _ := r.stack()
		st := anyfs.StatsOf(t)
		if link != nil {
			stack = link.Profile().String()
		}
		st.Capacity = 0
		s = s.Add(st)
	}
	f := c.files.Stats()
	if ws := c.workingSetBytes(); ws > 0 {
		s.WorkingSetBytes = ws // files sessions read (tracked), not pages touched
	}
	return &proto.VMStats{Capacity: c.opt.Capacity, ResidentBytes: s.ResidentBytes - s.SharedBytes + f.ResidentBytes, DirtyBytes: s.DirtyBytes,
		PinnedBytes: s.PinnedBytes, WorkingSetBytes: s.WorkingSetBytes, Faults: s.Faults + f.Faults, PageInBytes: s.PageInBytes + f.PageInBytes,
		WritebackBytes: s.PageOutBytes, ReclaimBytes: s.ReclaimBytes + c.reclaimed.Load(), FaultRate: s.FaultRate + f.FaultRate,
		Thrashing: s.Thrashing, Stack: stack}
}

// faultBytes is the bytes materialized by faults so far (per session
// working set deltas).
func (c *cacheFS) faultBytes() int64 { return c.files.Stats().PageInBytes }

// refaultBytes is the bytes faulted in again after a reclaim so far
// (machine wide; per session deltas are the session's thrashing).
func (c *cacheFS) refaultBytes() int64 {
	c.mu.Lock()
	repos := make([]*repoFS, 0, len(c.repos))
	for _, r := range c.repos {
		repos = append(repos, r)
	}
	c.mu.Unlock()
	n := c.files.Stats().RefaultBytes
	for _, r := range repos {
		t, _, _, _ := r.stack()
		n += anyfs.StatsOf(t).RefaultBytes
	}
	return n
}

// capacity is the local tier's capacity (0 = unlimited/unknown).
func (c *cacheFS) capacity() int64 { return c.opt.Capacity }

// residentBytes is the disk held by pages and materialized files.
func (c *cacheFS) residentBytes() int64 {
	v := c.vm()
	return v.ResidentBytes
}

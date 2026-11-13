package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/store"
)

// Build slot commits, saves and restores (see cachefs.go).

// errSlotBusy: a session holds the slot.
var errSlotBusy = errors.New("build slot in use")

// connectStore links a repository's stack to its store prefix with the
// given grant or the last one of a session (nil if none is known).
func (s *Worker) connectStore(repo string, g *store.Grant) error {
	var gr store.Grant
	if g != nil {
		gr = *g
	} else {
		var ok bool
		if gr, ok = s.grants.get(repo, 10*time.Minute); !ok {
			return errNoStore
		}
	}
	return s.cfs.setGrant(repo, gr)
}

// latestSlot returns the most recently used slot of a repository.
func (s *Worker) latestSlot(repo string) (slotRef, bool) {
	var refs []slotRef
	for _, r := range s.slots.list() {
		if r.Repo == repo {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return slotRef{}, false
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Meta.LastUsed > refs[j].Meta.LastUsed })
	return refs[0], true
}

// liveLayers are the layers the slots of a repository mount or start
// from, except the head of slot (its commit replaces it).
func (s *Worker) liveLayers(repo, slot string) []string {
	var out []string
	for _, r := range s.slots.list() {
		if r.Repo != repo {
			continue
		}
		out = append(out, r.Meta.Base)
		if r.Path != slot {
			out = append(out, r.Meta.Head)
		}
	}
	return out
}

// commitSlot commits an idle slot's changes (it is held meanwhile) and
// records the new head in its metadata.
func (s *Worker) commitSlot(ctx context.Context, ref slotRef) (commitResult, error) {
	// The snapshot lock first: a background commit of the previous
	// session may hold it for a while, and holding the slot meanwhile
	// would make the next session wait.
	lk := s.cfs.snapLock(ref.Path)
	lk.Lock()
	defer lk.Unlock()
	if !s.slots.acquire(ref.Path) {
		return commitResult{}, errSlotBusy
	}
	if _, err := s.commitPendingLocked(ctx, ref.Path, ref.Repo); err != nil {
		s.slots.releaseMaint(ref.Path, ref.Meta.Bytes)
		return commitResult{}, err
	}
	m, err := readMeta(ref.Path)
	if err != nil {
		s.slots.releaseMaint(ref.Path, ref.Meta.Bytes)
		return commitResult{}, err
	}
	res, err := s.cfs.commit(ctx, ref.Path, ref.Repo, m, s.liveLayers(ref.Repo, ref.Path))
	if err == nil {
		now := s.cfg.now().Unix()
		s.slots.update(ref.Path, func(x *slotMeta) {
			if top := res.chain.Top(); top != nil {
				x.Head = top.ID
			}
			x.Committed, x.UpperBytes, x.Scanned = now, res.upper, res.scanned
			x.Clean = x.Leases // the slot is held: no session runs
			if res.changed {
				x.Bytes = res.tree.Bytes()
			}
		})
		m.Bytes = res.tree.Bytes()
	}
	s.slots.releaseMaint(ref.Path, m.Bytes)
	return res, err
}

// writebackSlot commits a slot and flushes its head to the store; a
// busy slot flushes its last commit. It returns the flushed head.
func (s *Worker) writebackSlot(ctx context.Context, ref slotRef, why string) (proto.LayerHead, error) {
	begin := time.Now()
	res, err := s.commitSlot(ctx, ref)
	if errors.Is(err, errSlotBusy) {
		m, _ := readMeta(ref.Path)
		var r *repoFS
		if r, err = s.cfs.repo(ref.Repo); err == nil {
			res.repo = r
			if res.chain, err = r.chain(ctx, m.Head); err == nil {
				res.tree = res.chain.Tree()
			}
		}
	}
	if err != nil {
		return proto.LayerHead{}, err
	}
	if res.chain.Top() == nil {
		return proto.LayerHead{}, errors.New("build slot is empty")
	}
	h, err := s.cfs.flush(ctx, res)
	if err != nil {
		return h, err
	}
	s.log.Info("build slot written back", "why", why, "repo", ref.Repo, "layer", h.Layer, "depth", len(h.Chain),
		"read", res.scan.Read, "pages", h.Pages, "page_bytes", h.PageBytes, "seconds", time.Since(begin).Seconds())
	return h, nil
}

func (s *Worker) saveSlot(ctx context.Context, repo string, g *store.Grant) (proto.SlotResult, error) {
	if err := s.connectStore(repo, g); err != nil {
		return proto.SlotResult{}, err
	}
	ref, ok := s.latestSlot(repo)
	if !ok {
		return proto.SlotResult{}, fmt.Errorf("no build slot for repository %s", repo)
	}
	h, err := s.writebackSlot(ctx, ref, "save")
	if err != nil {
		return proto.SlotResult{}, err
	}
	return proto.SlotResult{Head: &h, Pages: h.Pages, PageBytes: h.PageBytes}, nil
}

// restoreSlot makes a store head this machine's head of the repository
// and mounts it as a new slot (contents are faulted in on use).
func (s *Worker) restoreSlot(ctx context.Context, repo string, head proto.LayerHead, g *store.Grant) (proto.SlotResult, error) {
	return s.restoreSlotFrom(ctx, repo, head, g, nil)
}

// restoreSlotFrom is restoreSlot of a head that may live in another
// prefix: src grants the prefix of head.Repo (a sibling worktree) or
// another store (seed.go); without src the head is in the repository's
// own prefix.
func (s *Worker) restoreSlotFrom(ctx context.Context, repo string, head proto.LayerHead, g, src *store.Grant) (proto.SlotResult, error) {
	begin := time.Now()
	if head.Layer == "" {
		return proto.SlotResult{}, errors.New("restore: no head layer")
	}
	if err := s.connectStore(repo, g); err != nil {
		return proto.SlotResult{}, err
	}
	if from := head.Repo; src != nil || from != "" && from != repo {
		if from == "" {
			from = repo
		}
		gr := g
		if src != nil {
			gr = src
		}
		if gr == nil {
			return proto.SlotResult{}, errors.New("restore from another prefix: no grant")
		}
		if !gr.Allows(store.RepoScope(from)) {
			return proto.SlotResult{}, fmt.Errorf("restore: grant does not cover repository %s", from)
		}
		if err := s.cfs.setSeed(repo, from, *gr); err != nil {
			return proto.SlotResult{}, err
		}
	}
	ch, err := s.cfs.restore(ctx, repo, head.Layer, s.cfg.BuildTTL)
	if err != nil {
		return proto.SlotResult{}, err
	}
	now := s.cfg.now()
	top := ch.Top()
	meta := slotMeta{Created: ch.Oldest().Unix(), LastUsed: now.Unix(), Restored: now.Unix(), Bytes: top.Bytes,
		Base: top.ID, Head: top.ID, Committed: now.Unix()}
	if prev, ok := s.latestSlot(repo); ok {
		meta.Caches = prev.Meta.Caches
	}
	slot, err := s.slots.adopt(repo, meta)
	if err != nil {
		return proto.SlotResult{}, err
	}
	defer s.slots.unpin(slot)
	if s.cfs.mode == fsPlain {
		r, _ := s.cfs.repo(repo)
		err = anyfs.Extract(ctx, r.tier, ch.Tree(), filepath.Join(slot, "data"))
	} else {
		err = s.cfs.mount(ctx, slot, repo, top.ID)
	}
	if err != nil {
		s.cfs.removeSlot(slot)
		return proto.SlotResult{}, err
	}
	secs := time.Since(begin).Seconds()
	s.slots.update(slot, func(m *slotMeta) { m.RestoreSeconds = secs })
	return proto.SlotResult{Seconds: secs}, nil
}

// restoreUsage reports a session's first use of a restored slot.
func restoreUsage(m slotMeta) (bool, float64) {
	return m.Restored != 0 && m.Runs == 0, m.RestoreSeconds
}

// unflushed reports whether a slot has changes the store lacks: used
// since its last commit, or committed but not flushed by this worker.
func (s *Worker) unflushed(r slotRef) bool {
	if r.Meta.uncommitted() {
		return true
	}
	if r.Meta.Head == "" || r.Meta.Head == r.Meta.Base && r.Meta.Restored != 0 {
		return false
	}
	s.cfs.mu.Lock()
	h := s.cfs.heads[r.Repo]
	s.cfs.mu.Unlock()
	return h.Layer != r.Meta.Head
}

// writebackChanged writes back the most recent slot of every repository
// with a valid grant and unflushed changes, all within limit (on drain,
// and on SIGTERM before the worker exits).
func (s *Worker) writebackChanged(limit time.Duration, why string) {
	deadline := time.Now().Add(limit)
	for _, repo := range s.changedRepos() {
		if s.connectStore(repo, nil) != nil {
			continue
		}
		ref, _ := s.latestSlot(repo)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, err := s.writebackSlot(ctx, ref, why)
		cancel()
		if err != nil {
			s.log.Warn("writeback on "+why, "repo", repo, "err", err)
		}
	}
}

// changedRepos returns the repositories whose most recent slot has
// unflushed changes.
func (s *Worker) changedRepos() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range s.slots.list() {
		if seen[r.Repo] {
			continue
		}
		seen[r.Repo] = true
		if ref, ok := s.latestSlot(r.Repo); ok && s.unflushed(ref) {
			out = append(out, r.Repo)
		}
	}
	sort.Strings(out)
	return out
}

// sessionEnded commits a slot right away when its session grew the
// upper beyond the dirty threshold (otherwise the idle commit of
// maintainSlots takes it).
//
// Overlay slots commit the snapshot of the session (captured before the
// release) first; the head is then flushed right away when the session
// grew the upper beyond the dirty threshold.
func (s *Worker) slotReleased(slot, repo string) {
	if s.cfs.mode == fsPlain {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	before, err := readMeta(slot)
	if err != nil {
		return
	}
	res, err := s.commitPending(ctx, slot, repo)
	if err != nil {
		s.log.Warn("build slot snapshot commit", "slot", slot, "err", err)
		return
	}
	if s.cfs.takeQueued(slot) {
		// This session ended while an earlier snapshot was committing:
		// commit the live slot now that it is done (a session holding
		// the slot meanwhile captures it at its own end).
		if m, err := readMeta(slot); err == nil && m.uncommitted() {
			cr, err := s.commitSlot(ctx, slotRef{Repo: repo, Path: slot, Meta: m})
			switch {
			case err == nil:
				if cr.changed {
					res = cr
				}
			case errors.Is(err, errSlotBusy):
			default:
				s.log.Warn("build slot commit of a queued session", "slot", slot, "err", err)
			}
		}
	}
	if res.changed {
		s.scheduleHeadFlush(repo)
	}
	if s.cfs.opt.DirtyBytes <= 0 {
		return
	}
	if res.repo != nil {
		if res.upper-before.UpperBytes > s.cfs.opt.DirtyBytes && s.connectStore(repo, nil) == nil && res.chain.Top() != nil {
			if res.tree == nil {
				res.tree = res.chain.Tree()
			}
			if _, err := s.cfs.flush(ctx, res); err != nil {
				s.log.Warn("build slot writeback", "repo", repo, "err", err)
			}
		}
		return
	}
	m, err := readMeta(slot)
	if err != nil || treeSize(filepath.Join(slot, "upper"))-m.UpperBytes <= s.cfs.opt.DirtyBytes {
		return
	}
	s.maintainSlot(ctx, slotRef{Repo: repo, Path: slot, Meta: m}, "dirty")
}

// maintainSlots commits idle slots, flushes them when a grant is known
// and rebases idle slots with a large upper (lifecycle tick). Each
// repository is maintained by one pass at a time, repositories in
// parallel: a slow commit or flush of one (minutes for a large head)
// delays neither the others nor the next tick's pass over them. It
// returns when the repositories it took are done.
func (s *Worker) maintainSlots(ctx context.Context) {
	byRepo := map[string][]slotRef{}
	for _, r := range s.slots.list() {
		if !r.Leased {
			byRepo[r.Repo] = append(byRepo[r.Repo], r)
		}
	}
	var wg sync.WaitGroup
	for repo, refs := range byRepo {
		if !s.claimMaint(repo) {
			continue // a pass is still at it
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.endMaint(repo)
			for _, r := range refs {
				if ctx.Err() != nil {
					return
				}
				s.maintainOne(ctx, r)
			}
		}()
	}
	wg.Wait()
}

// maintainOne is maintainSlots' work on one idle slot.
func (s *Worker) maintainOne(ctx context.Context, r slotRef) {
	now := s.cfg.now()
	if r.Meta.uncommitted() && now.Sub(time.Unix(r.Meta.LastUsed, 0)) >= s.cfs.opt.Idle {
		s.maintainSlot(ctx, r, "idle")
	} else if s.unflushed(r) {
		s.flushWhenClean(ctx, r, false)
	} else if r.Meta.Head != r.Meta.Base && r.Meta.UpperBytes > s.cfs.opt.RebaseBytes {
		s.rebaseSlot(ctx, r)
	}
}

// claimMaint marks a repository as maintained; false if a pass has it.
func (s *Worker) claimMaint(repo string) bool {
	s.maintMu.Lock()
	defer s.maintMu.Unlock()
	if s.maintRepos[repo] {
		return false
	}
	if s.maintRepos == nil {
		s.maintRepos = map[string]bool{}
	}
	s.maintRepos[repo] = true
	return true
}

// endMaint ends claimMaint.
func (s *Worker) endMaint(repo string) {
	s.maintMu.Lock()
	defer s.maintMu.Unlock()
	delete(s.maintRepos, repo)
}

// maintainSlot commits a slot and writes it back when the store is
// reachable: an idle commit leaves the pages to the class writeback and
// moves the store head once they are clean (flushWhenClean) or at the
// head flush deadline, other reasons flush right away.
func (s *Worker) maintainSlot(ctx context.Context, r slotRef, why string) {
	if why == "idle" {
		res, err := s.commitSlot(ctx, r)
		if err != nil {
			if !errors.Is(err, errSlotBusy) {
				s.log.Warn("build slot commit", "repo", r.Repo, "slot", r.Path, "err", err)
			}
			return
		}
		if res.changed {
			s.scheduleHeadFlush(r.Repo)
		}
		if m, err := readMeta(r.Path); err == nil {
			r.Meta = m
			s.flushWhenClean(ctx, r, false)
		}
		return
	}
	if s.connectStore(r.Repo, nil) == nil {
		if _, err := s.writebackSlot(ctx, r, why); err != nil && !errors.Is(err, errSlotBusy) {
			s.log.Warn("build slot writeback", "repo", r.Repo, "slot", r.Path, "err", err)
		}
		return
	}
	if _, err := s.commitSlot(ctx, r); err != nil && !errors.Is(err, errSlotBusy) {
		s.log.Warn("build slot commit", "repo", r.Repo, "slot", r.Path, "err", err)
	}
}

// rebaseSlot gives an idle committed slot a fresh upper over its head.
func (s *Worker) rebaseSlot(ctx context.Context, r slotRef) bool {
	if !s.slots.acquire(r.Path) {
		return false
	}
	defer s.slots.releaseMaint(r.Path, r.Meta.Bytes)
	m, err := readMeta(r.Path)
	if err != nil || m.uncommitted() {
		return false // changed since the commit
	}
	base, err := s.cfs.rebase(ctx, r.Path, r.Repo, m)
	if err != nil {
		s.log.Warn("build slot rebase", "slot", r.Path, "err", err)
	}
	s.slots.update(r.Path, func(x *slotMeta) {
		x.Base = base
		if base == x.Head {
			x.UpperBytes = 0
			x.Physical = slotUpperBytes(r.Path)
		}
	})
	return err == nil
}

// mountSlot mounts a slot that is not mounted (after a worker restart)
// for reading its outputs.
func (s *Worker) mountSlot(slot, repo string) {
	if s.cfs == nil {
		return
	}
	m, err := readMeta(slot)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.cfs.mount(ctx, slot, repo, m.Base); err != nil {
		s.log.Warn("mount build slot", "slot", slot, "err", err)
	}
}

// headFlushDeadline bounds how long a committed head waits for its
// pages' class writeback before it is flushed whole: the store head is
// at most this far behind the first commit it lacks, so losing the
// machine loses about the last session. The long class writeback delay
// of churn (access.ChurnWritebackDelay) only spares the upload of pages
// superseded (deleted or rewritten, then discarded at the next commit)
// before the deadline; pages the committed head still references are
// flushed by it, ephemeral (NoWriteback) ones included: a head in the
// store never references a page the store lacks.
const headFlushDeadline = 30 * time.Second

// headCheckInterval is how often an armed head watch checks whether the
// pages of the latest committed head are clean (then the store head moves
// right away: layer objects and heads/slot only).
var headCheckInterval = time.Second

// scheduleHeadFlush arms the repository's head watch unless it is armed
// already: the store head moves as soon as every page the latest
// committed head references is clean (unrelated dirty pages do not
// matter), at the latest at the flush deadline, which counts from the
// first unflushed commit (later commits are flushed with it).
func (s *Worker) scheduleHeadFlush(repo string) {
	s.headMu.Lock()
	defer s.headMu.Unlock()
	if s.headTimers == nil {
		s.headTimers = map[string]chan struct{}{}
	}
	if s.headTimers[repo] != nil {
		return
	}
	stop := make(chan struct{})
	s.headTimers[repo] = stop
	deadline := time.NewTimer(s.cfs.opt.FlushDeadline)
	tick := time.NewTicker(min(headCheckInterval, max(s.cfs.opt.FlushDeadline/4, time.Millisecond)))
	go func() {
		defer deadline.Stop()
		defer tick.Stop()
		done := func() {
			s.headMu.Lock()
			if s.headTimers[repo] == stop {
				delete(s.headTimers, repo)
			}
			s.headMu.Unlock()
		}
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if s.flushHeadIfClean(repo) {
					done()
					return
				}
			case <-deadline.C:
				done()
				s.flushHeadDeadline(repo)
				return
			}
		}
	}()
}

// flushHeadIfClean moves the store head to the repository's latest
// committed head when its pages are all clean; it reports whether the
// store has that head now.
func (s *Worker) flushHeadIfClean(repo string) bool {
	ref, ok := s.latestSlot(repo)
	if !ok || ref.Meta.Head == "" {
		return true
	}
	if s.headFlushed(repo, ref.Meta.Head) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s.flushWhenClean(ctx, ref, false)
	return s.headFlushed(repo, ref.Meta.Head)
}

// headFlushed reports whether layer is the repository's flushed head.
func (s *Worker) headFlushed(repo, layer string) bool {
	s.cfs.mu.Lock()
	defer s.cfs.mu.Unlock()
	return s.cfs.heads[repo].Layer == layer
}

// stopHeadFlushes disarms the head watches (shutdown).
func (s *Worker) stopHeadFlushes() {
	s.headMu.Lock()
	defer s.headMu.Unlock()
	for repo, stop := range s.headTimers {
		close(stop)
		delete(s.headTimers, repo)
	}
}

// flushHeadDeadline flushes the most recent committed head of a
// repository when the store lacks it (its deadline passed).
func (s *Worker) flushHeadDeadline(repo string) {
	ref, ok := s.latestSlot(repo)
	if !ok || ref.Meta.Head == "" {
		return
	}
	s.cfs.mu.Lock()
	flushed := s.cfs.heads[repo].Layer == ref.Meta.Head
	s.cfs.mu.Unlock()
	if flushed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s.flushWhenClean(ctx, ref, true)
}

// flushWhenClean moves the store head to a slot's committed head once
// the class writeback has written all its pages (cheap: layer objects
// and the head), or flushes it whole at the deadline (force, or the
// commit is older than the flush deadline).
func (s *Worker) flushWhenClean(ctx context.Context, ref slotRef, force bool) {
	if ref.Meta.Head == "" || s.connectStore(ref.Repo, nil) != nil {
		return
	}
	r, err := s.cfs.repo(ref.Repo)
	if err != nil {
		return
	}
	ch, err := r.chain(ctx, ref.Meta.Head)
	if err != nil || ch.Top() == nil {
		return
	}
	res := commitResult{repo: r, chain: ch, tree: ch.Tree()}
	lag := s.cfg.now().Sub(time.Unix(ref.Meta.Committed, 0))
	dirty := s.cfs.dirtyIn(r, res.tree)
	if dirty > 0 && !force && lag < s.cfs.opt.FlushDeadline {
		return
	}
	h, err := s.cfs.flush(ctx, res)
	if err != nil {
		s.log.Warn("build slot head flush", "repo", ref.Repo, "err", err)
		return
	}
	why := "clean"
	if dirty > 0 {
		why = "deadline"
	}
	s.log.Info("build slot written back", "why", why, "repo", ref.Repo, "layer", h.Layer, "dirty_pages", dirty,
		"pages", h.Pages, "page_bytes", h.PageBytes, "lag_seconds", lag.Seconds())
}

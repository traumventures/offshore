package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/hostfs"
)

// Session snapshots: every session of an overlay slot is committed, even
// when the next session starts right away (no idle gap).
//
// When a session ends, before the slot is released, its changes are
// planned (anyfs.PlanChanges: stats of the upper entries newer than the
// head's watermark) and the changed files captured into
// slot/snaps/<ns>/files, by reflink where the file system supports it
// (FICLONE, metadata only), else by copy; the plan is written last
// (plan.json), which makes the snapshot complete. The slot is released
// and the snapshot committed in the background (hashing, pages, layer,
// head; the captured files move into the file cache), while the next
// session may already write to the slot. A commit from the live slot
// first commits pending snapshots. On start the worker commits complete
// snapshots and drops incomplete ones before serving: a crash loses at
// most the session that was running.

const snapsDir = "snaps"

// snapCopyMax: without reflinks a session whose changed files exceed this
// is not captured (copying them would delay the next session); the idle
// commit takes it.
const snapCopyMax = 256 << 20

var errNoSnapshots = errors.New("plain slots are committed without snapshots")

// snapLock serializes a slot's snapshots and commits.
func (c *cacheFS) snapLock(slot string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapMu == nil {
		c.snapMu = map[string]*sync.Mutex{}
	}
	l := c.snapMu[slot]
	if l == nil {
		l = &sync.Mutex{}
		c.snapMu[slot] = l
	}
	return l
}

// snapshotSlot captures the session that just ended in a slot (the
// session still holds it).
func (s *Worker) snapshotSlot(ctx context.Context, slot, repo string) error {
	if s.cfs.mode == fsPlain {
		return errNoSnapshots
	}
	lk := s.cfs.snapLock(slot)
	lk.Lock()
	defer lk.Unlock()
	if _, err := s.commitPendingLocked(ctx, slot, repo); err != nil {
		return err
	}
	m, err := readMeta(slot)
	if err != nil {
		return err
	}
	r, err := s.cfs.repo(repo)
	if err != nil {
		return err
	}
	begin := time.Now()
	p, err := s.cfs.plan(ctx, slot, r, m)
	if err != nil {
		return err
	}
	if !s.cfs.reflink {
		var size int64
		read := map[string]bool{}
		for _, q := range p.Changes.Read {
			read[q] = true
		}
		for _, n := range p.Changes.Nodes {
			if read[n.Path] {
				size += n.Size
			}
		}
		if size > snapCopyMax {
			// Copying would hold the slot too long: the idle commit takes
			// the session .
			s.log.Info("build slot session not captured (no reflink, large change)", "slot", slot, "bytes", size)
			if t := s.cfs.tracker(slot); t != nil {
				t.requeue(p.Reads)
			}
			s.cfs.release(slot, p, false)
			return nil
		}
	}
	dir := filepath.Join(slot, snapsDir, fmt.Sprintf("%020d", time.Now().UnixNano()))
	upper, data := filepath.Join(slot, "upper"), filepath.Join(slot, "data")
	source := func(rel string) string {
		u := filepath.Join(upper, filepath.FromSlash(rel))
		if fi, err := os.Lstat(u); err == nil && fullUpperFile(u, fi.Size()) {
			return u // same file system as the snapshot: a reflink clone
		}
		return filepath.Join(data, filepath.FromSlash(rel))
	}
	if err := p.Changes.Capture(filepath.Join(dir, "files"), source); err != nil {
		os.RemoveAll(dir)
		s.cfs.release(slot, p, false)
		return fmt.Errorf("capture session: %w", err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		s.cfs.release(slot, p, false)
		return err
	}
	// plan.json marks the capture complete: fsynced with its directory.
	// The captured files are not fsynced one by one (hundreds per session);
	// recovery checks their sizes against the plan instead (capturedIntact).
	if err := hostfs.WriteFileSync(filepath.Join(dir, "plan.json"), b, 0o644); err != nil {
		os.RemoveAll(dir)
		s.cfs.release(slot, p, false)
		return err
	}
	s.log.Debug("build slot session captured", "slot", slot, "files", len(p.Changes.Read), "known", len(p.Resolved),
		"known_bytes", p.Changes.Stats.KnownRead, "seconds", time.Since(begin).Seconds())
	return nil
}

// capturedIntact reports whether every captured file has the size the
// plan recorded. A crash after plan.json was made durable but before the
// captured data reached the disk (delayed allocation on ext4 or xfs) leaves
// such files short or empty; committing them would publish wrong content.
func capturedIntact(cs *anyfs.ChangeSet, files string) bool {
	size := make(map[string]int64, len(cs.Nodes))
	for _, n := range cs.Nodes {
		if n.Kind == anyfs.File {
			size[n.Path] = n.Size
		}
	}
	for _, p := range cs.Read {
		want, ok := size[p]
		if !ok {
			continue
		}
		fi, err := os.Lstat(filepath.Join(files, filepath.FromSlash(p)))
		if err != nil || fi.Size() != want {
			return false
		}
	}
	return true
}

// pendingSnapshots returns a slot's snapshot directories, oldest first.
func pendingSnapshots(slot string) []string {
	des, err := os.ReadDir(filepath.Join(slot, snapsDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range des {
		if d.IsDir() {
			out = append(out, filepath.Join(slot, snapsDir, d.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// commitPending commits a slot's snapshots (the slot may be in use).
func (s *Worker) commitPending(ctx context.Context, slot, repo string) (commitResult, error) {
	lk := s.cfs.snapLock(slot)
	lk.Lock()
	defer lk.Unlock()
	return s.commitPendingLocked(ctx, slot, repo)
}

// commitPendingLocked commits the complete snapshots oldest first and
// drops incomplete and stale ones; it returns the last commit (repo nil:
// none).
func (s *Worker) commitPendingLocked(ctx context.Context, slot, repo string) (commitResult, error) {
	var last commitResult
	for _, dir := range pendingSnapshots(slot) {
		var p slotPlan
		b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
		if err == nil {
			err = json.Unmarshal(b, &p)
		}
		m, merr := readMeta(slot)
		if merr != nil {
			return last, merr
		}
		head := m.Head
		if head == "" {
			head = m.Base
		}
		intact := err == nil && p.Changes != nil && capturedIntact(p.Changes, filepath.Join(dir, "files"))
		if err != nil || p.Changes == nil || p.Head != head || !intact {
			s.log.Warn("dropping build slot snapshot", "slot", slot, "snapshot", filepath.Base(dir), "complete", err == nil, "intact", intact)
			os.RemoveAll(dir)
			s.cfs.release(slot, p, false)
			continue
		}
		r, err := s.cfs.repo(repo)
		if err != nil {
			return last, err
		}
		r.cmu.Lock()
		res, err := s.cfs.finish(ctx, slot, r, m, p, filepath.Join(dir, "files"), true, s.liveLayers(repo, slot))
		r.cmu.Unlock()
		if err != nil {
			return last, err // retried later: the hasher keeps holding the plan's pages
		}
		s.cfs.release(slot, p, true)
		now := s.cfg.now().Unix()
		s.slots.update(slot, func(x *slotMeta) {
			if top := res.chain.Top(); top != nil {
				x.Head = top.ID
			}
			x.Scanned, x.UpperBytes = p.Scanned, p.Upper
			if x.Leases == p.Leases {
				// No session started since the capture (which ran while
				// the ending session held the slot).
				x.Committed, x.Clean = now, p.Leases
			}
			if res.changed {
				x.Bytes = res.tree.Bytes()
			}
		})
		os.RemoveAll(dir)
		last = res
	}
	return last, nil
}

// recoverSnapshots commits the snapshots a previous worker process left
// (it crashed or stopped before committing them).
func (s *Worker) recoverSnapshots(ctx context.Context) {
	for _, r := range s.slots.list() {
		if len(pendingSnapshots(r.Path)) == 0 {
			continue
		}
		if _, err := s.commitPending(ctx, r.Path, r.Repo); err != nil {
			s.log.Warn("recover build slot snapshots", "slot", r.Path, "err", err)
		} else {
			s.log.Info("recovered build slot snapshots", "slot", r.Path)
		}
	}
}

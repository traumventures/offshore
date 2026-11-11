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

	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/snapshot"
	"github.com/traumventures/offshore/internal/store"
)

// Build slot snapshots.
//
// Save writes the repository's most recently used slot to the store
// (repos/<repo>/ of the grant) as content addressed chunks; the previous
// manifest of the slot is kept next to it, so the next save reads only
// changed files. Restore creates a new slot from a snapshot: the skeleton
// (directories, sparse placeholders with sizes and mtimes) is ready at
// once, file contents are filled on first access through a FUSE fill
// layer mounted on the slot's data directory and in the background; once
// complete and not in use, the mount is replaced by the filled directory.
// Without FUSE the restore fills everything before it returns.

const lastManifest = "snapshot.json"

// mountFill mounts the fill layer (variable for tests without FUSE).
var mountFill = snapshot.Mount

// fill is a restore in progress.
type fill struct {
	slot    string
	f       *snapshot.Filler
	unmount func() error
	begin   time.Time
	done    chan struct{}
	err     error
}

// fills tracks restores in progress per slot.
type fills struct {
	mu sync.Mutex
	m  map[string]*fill
}

func (s *Worker) repoStore(repo string, g *store.Grant) (store.Store, store.Grant, error) {
	var gr store.Grant
	if g != nil {
		gr = *g
	} else {
		var ok bool
		if gr, ok = s.grants.get(repo, 10*time.Minute); !ok {
			return nil, gr, errors.New("no store grant for the repository (no recent session and none in the request)")
		}
	}
	st, err := store.Open(gr, store.RepoScope(repo), blobcache.MaxTTL)
	return st, gr, err
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

func (s *Worker) saveSlot(ctx context.Context, repo string) (proto.SlotResult, error) {
	st, gr, err := s.repoStore(repo, nil)
	if err != nil {
		return proto.SlotResult{}, err
	}
	ref, ok := s.latestSlot(repo)
	if !ok {
		return proto.SlotResult{}, fmt.Errorf("no build slot for repository %s", repo)
	}
	if !s.slots.acquire(ref.Path) {
		return proto.SlotResult{}, errors.New("build slot in use or being restored")
	}
	defer s.slots.releaseMaint(ref.Path, ref.Meta.Bytes)
	meta, _ := json.Marshal(ref.Meta)
	opt := snapshot.Options{Meta: meta}
	if prev, err := readLastManifest(ref.Path); err == nil {
		opt.Previous = prev
	}
	m, stats, err := snapshot.Save(ctx, filepath.Join(ref.Path, "data"), st, opt)
	if err != nil {
		return proto.SlotResult{}, err
	}
	writeLastManifest(ref.Path, m)
	info := proto.SnapshotInfo{Repo: repo, Store: gr.Store, Bytes: stats.Bytes, At: m.At}
	s.recordSnapshot(info)
	return proto.SlotResult{Snapshot: &info, Chunks: stats.Chunks, NewChunks: stats.NewChunks, Seconds: stats.Seconds}, nil
}

func readLastManifest(slot string) (*snapshot.Manifest, error) {
	b, err := os.ReadFile(filepath.Join(slot, lastManifest))
	if err != nil {
		return nil, err
	}
	var m snapshot.Manifest
	return &m, json.Unmarshal(b, &m)
}

func writeLastManifest(slot string, m *snapshot.Manifest) {
	if b, err := json.Marshal(m); err == nil {
		os.WriteFile(filepath.Join(slot, lastManifest), b, 0o644)
	}
}

// recordSnapshot remembers a snapshot for Status.Snapshots.
func (s *Worker) recordSnapshot(info proto.SnapshotInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.snapshots {
		if o.Repo == info.Repo {
			s.snapshots[i] = info
			return
		}
	}
	s.snapshots = append(s.snapshots, info)
}

func (s *Worker) restoreSlot(ctx context.Context, repo string, snap proto.SnapshotInfo) (proto.SlotResult, error) {
	begin := time.Now()
	st, _, err := s.repoStore(repo, nil)
	if err != nil {
		return proto.SlotResult{}, err
	}
	m, err := snapshot.Load(ctx, st, snap.At)
	if err != nil {
		return proto.SlotResult{}, err
	}
	var meta slotMeta
	if len(m.Meta) > 0 {
		json.Unmarshal(m.Meta, &meta)
	}
	now := s.cfg.now()
	if meta.Created == 0 {
		meta.Created = m.At
	}
	if meta.expired(now, s.cfg.BuildTTL) {
		return proto.SlotResult{}, errors.New("snapshot is older than the build slot TTL")
	}
	meta.LastUsed, meta.Restored, meta.Bytes = now.Unix(), now.Unix(), m.Bytes
	meta.Digests = nil // machine caches are linked per machine
	meta.Runs = 0      // the read index stays on the old machine
	slot, err := s.slots.adopt(repo, meta)
	if err != nil {
		return proto.SlotResult{}, err
	}
	fl := &fill{slot: slot, begin: begin, done: make(chan struct{})}
	backing := filepath.Join(slot, "fill")
	fl.f = snapshot.NewFiller(m, backing, st, filepath.Join(slot, "chunks"))
	if err := fl.f.Skeleton(); err != nil {
		os.RemoveAll(slot)
		s.slots.unpin(slot)
		return proto.SlotResult{}, err
	}
	data := filepath.Join(slot, "data")
	if fl.unmount, err = mountFill(data, fl.f); err != nil {
		// No FUSE: fill eagerly, then use the directory.
		s.log.Info("restore without fill layer, filling eagerly", "slot", slot, "err", err)
		if err := fl.f.FillAll(ctx, 16); err != nil {
			os.RemoveAll(slot)
			s.slots.unpin(slot)
			return proto.SlotResult{}, err
		}
		fl.f.Close()
		os.Remove(data)
		if err := os.Rename(backing, data); err != nil {
			return proto.SlotResult{}, err
		}
		s.slots.update(slot, func(m *slotMeta) { m.RestoreSeconds = time.Since(begin).Seconds() })
		s.slots.unpin(slot)
		return proto.SlotResult{Chunks: fl.f.Stats().NewChunks, NewChunks: fl.f.Stats().NewChunks, Seconds: time.Since(begin).Seconds()}, nil
	}
	s.fills.mu.Lock()
	if s.fills.m == nil {
		s.fills.m = map[string]*fill{}
	}
	s.fills.m[slot] = fl
	s.fills.mu.Unlock()
	go s.backgroundFill(fl)
	return proto.SlotResult{Seconds: time.Since(begin).Seconds()}, nil
}

// backgroundFill fetches the rest of a lazy restore and finalizes it.
func (s *Worker) backgroundFill(fl *fill) {
	fl.err = fl.f.FillAll(context.Background(), 8)
	close(fl.done)
	if fl.err != nil {
		s.log.Warn("restore fill failed", "slot", fl.slot, "err", fl.err)
	} else {
		s.log.Info("restore filled", "slot", fl.slot, "seconds", time.Since(fl.begin).Seconds())
	}
	s.finishFill(fl.slot)
}

// finishFill replaces a completed restore's fill layer by the directory
// once no session uses the slot (called again on release). A failed
// restore removes the slot.
func (s *Worker) finishFill(slot string) {
	s.fills.mu.Lock()
	fl := s.fills.m[slot]
	s.fills.mu.Unlock()
	if fl == nil {
		return
	}
	select {
	case <-fl.done:
	default:
		return
	}
	if !s.slots.acquirePinned(slot) {
		return // in use; the release finishes it
	}
	defer func() {
		s.fills.mu.Lock()
		delete(s.fills.m, slot)
		s.fills.mu.Unlock()
	}()
	if err := fl.unmount(); err != nil {
		s.log.Warn("unmount fill layer", "slot", slot, "err", err)
	}
	fl.f.Close()
	data := filepath.Join(slot, "data")
	if fl.err == nil {
		os.Remove(data)
		fl.err = os.Rename(filepath.Join(slot, "fill"), data)
	}
	s.slots.unpin(slot)
	if fl.err != nil {
		s.slots.releaseMaint(slot, 0)
		os.RemoveAll(slot)
		return
	}
	s.slots.update(slot, func(m *slotMeta) { m.RestoreSeconds = time.Since(fl.begin).Seconds() })
	s.slots.releaseMaint(slot, treeSize(data))
}

// restoreUsage reports a session's first use of a restored slot.
func restoreUsage(m slotMeta) (bool, float64) {
	return m.Restored != 0 && m.Runs == 0, m.RestoreSeconds
}

// saveOnDrain snapshots the changed slots when the machine drains at the
// end of its lifetime, so that a new machine can restore them (reported in
// Status.Snapshots).
func (s *Worker) saveOnDrain() { s.saveChanged(8*time.Minute, "drain") }

// saveChanged snapshots the most recent slot of every repository that has
// a valid grant and changed since its last snapshot, all within limit (on
// drain, and on SIGTERM before the worker exits).
func (s *Worker) saveChanged(limit time.Duration, why string) {
	deadline := time.Now().Add(limit)
	for _, repo := range s.changedRepos() {
		if _, ok := s.grants.get(repo, 10*time.Minute); !ok {
			continue
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		res, err := s.saveSlot(ctx, repo)
		cancel()
		if err != nil {
			s.log.Warn("snapshot on "+why, "repo", repo, "err", err)
			continue
		}
		s.log.Info("snapshot on "+why, "repo", repo, "bytes", res.Snapshot.Bytes, "new_chunks", res.NewChunks, "seconds", res.Seconds)
	}
}

// changedRepos returns the repositories whose most recent slot was used
// after its last snapshot (or never saved).
func (s *Worker) changedRepos() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range s.slots.list() {
		if seen[r.Repo] {
			continue
		}
		seen[r.Repo] = true
		ref, ok := s.latestSlot(r.Repo)
		if !ok {
			continue
		}
		if m, err := readLastManifest(ref.Path); err == nil && m.At >= ref.Meta.LastUsed {
			continue
		}
		out = append(out, r.Repo)
	}
	sort.Strings(out)
	return out
}

package worker

import (
	"context"
	"errors"
	"time"
)

// Per-slot budget and the in-slot trim (docs/design-storage-v2.md).
//
// Every slot has a budget: the sum of its caches' max_size, else twice
// its size after its first run here, at least minSlotBudget and at most
// maxSlotBudgetShare of the disk. After every session the background
// trim removes garbage (pruneSlot stages 1 and 2: old incremental
// sessions, units unread for their policy's runs) when the slot exceeds
// trimShare of its budget, and size-trims to the budget above it.
//
// Deleting files of an overlay slot frees nothing by itself: a file of
// the lower becomes a whiteout, its content stays in the repository's
// page tier (and the file cache) while any tree references it. So a trim
// is committed (the store head shrinks too, once flushed), the idle slot
// is rebased over the new head (it no longer mounts the old chain) and
// the pages and files no live tree references are dropped
// (cacheFS.dropUnreferenced): that frees the disk.

// minSlotBudget is the least budget of a slot (a variable for tests).
var minSlotBudget int64 = 10 << 30

const (
	maxSlotBudgetShare = 0.40
	trimShare          = 0.75
)

// slotBudget is a slot's budget on a disk of total bytes (0: unknown).
func slotBudget(m slotMeta, total int64) int64 {
	b := cachesBudget(m)
	if b <= 0 {
		b = 2 * m.ColdBytes
	}
	b = max(b, minSlotBudget)
	if total > 0 {
		b = min(b, int64(float64(total)*maxSlotBudgetShare))
	}
	return b
}

// afterSession trims a released slot (in the background, exec).
func (s *Worker) afterSession(slot, repo string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s.trimSlot(ctx, slot, repo, false)
}

// trimSlot trims an idle slot: garbage above trimShare of its budget,
// then down to the budget (garbageOnly: the garbage alone, regardless of
// the budget; the reclaim of a garbage entry), and makes the deletions
// free the disk (shrinkSlot). It returns the bytes statfs says it freed.
func (s *Worker) trimSlot(ctx context.Context, slot, repo string, garbageOnly bool) int64 {
	m, err := readMeta(slot)
	if err != nil {
		return 0
	}
	total, before, _ := s.diskStat()
	budget := slotBudget(m, total)
	if !garbageOnly && m.Bytes <= int64(float64(budget)*trimShare) {
		return 0
	}
	if !s.slots.acquire(slot) {
		return 0 // a session took it
	}
	res := pruneSlot(slot, m, 0)
	if !garbageOnly && res.BytesLeft > budget {
		r := pruneSlot(slot, m, budget)
		res.Freed, res.Units, res.Sessions, res.BytesLeft = res.Freed+r.Freed, res.Units+r.Units, res.Sessions+r.Sessions, r.BytesLeft
	}
	if res.Freed > 0 {
		plain := s.cfs == nil || s.cfs.mode == fsPlain
		s.slots.update(slot, func(x *slotMeta) {
			x.Garbage = 0
			x.Leases++ // a change to commit, like a session's
			if plain {
				x.Physical = res.BytesLeft
			}
		})
	}
	s.slots.releaseMaint(slot, res.BytesLeft)
	if res.Freed == 0 {
		return 0
	}
	s.shrinkSlot(ctx, slot, repo)
	_, after, _ := s.diskStat()
	freed := max(0, after-before)
	s.log.Info("trimmed build slot", "slot", slot, "budget", budget, "units", res.Units, "sessions", res.Sessions,
		"logical_freed", res.Freed, "left", res.BytesLeft, "freed", freed)
	return freed
}

// shrinkSlot commits an idle slot's deletions, flushes the head (when the
// store is reachable), rebases the slot over it and drops what no live
// tree references any more.
func (s *Worker) shrinkSlot(ctx context.Context, slot, repo string) {
	if s.cfs == nil {
		return
	}
	m, err := readMeta(slot)
	if err != nil {
		return
	}
	ref := slotRef{Repo: repo, Path: slot, Meta: m}
	if _, err := s.commitSlot(ctx, ref); err != nil {
		if !errors.Is(err, errSlotBusy) {
			s.log.Warn("commit trimmed build slot", "slot", slot, "err", err)
		}
		return
	}
	if ref.Meta, err = readMeta(slot); err != nil {
		return
	}
	if s.connectStore(repo, nil) == nil {
		s.flushWhenClean(ctx, ref, true)
	}
	if s.cfs.mode != fsPlain && ref.Meta.Head != ref.Meta.Base {
		s.rebaseSlot(ctx, ref)
	}
	if n := s.cfs.dropUnreferenced(ctx, s.liveLayerMap()); n > 0 {
		s.log.Info("dropped pages and files of a trim", "slot", slot, "bytes", n)
	}
}

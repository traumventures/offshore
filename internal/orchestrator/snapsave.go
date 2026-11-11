package orchestrator

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// saveTimeout bounds one slot save before a machine stops.
const saveTimeout = 10 * time.Minute

// saveSlotLogged saves repo's slot on a machine through the worker, records
// the snapshot with the machine's provider (cache affinity of the
// scheduler) and logs a snapshot_save event.
func (o *Orchestrator) saveSlotLogged(ctx context.Context, slots SlotOps, t SlotTarget, prov, repo, why string) (scheduler.Snapshot, error) {
	start := time.Now()
	snap, err := slots.SaveSlot(ctx, t, repo)
	ev := o.machineEvent(events.SnapshotSave, t.Machine)
	ev.Repo, ev.Reason, ev.Seconds = repo, why, time.Since(start).Seconds()
	if err != nil {
		ev.Error = err.Error()
		o.obs.log.Append(ev)
		return snap, err
	}
	snap.Provider = prov
	if snap.Repo == "" {
		snap.Repo = repo
	}
	ev.Store, ev.Bytes = snap.Store, snap.Bytes
	o.obs.log.Append(ev)
	o.mu.Lock()
	o.addSnapshotLocked(snap)
	o.mu.Unlock()
	return snap, nil
}

// unsavedSlots returns the repositories with a build slot on the machine
// that changed since the worker's last snapshot of them.
func unsavedSlots(m *Machine) []string {
	st := m.Status
	if st == nil || st.Disk == nil {
		return nil
	}
	var idle int64
	if len(st.Sessions) == 0 {
		idle = st.IdleSince
	}
	var repos []string
	for repo, b := range st.Disk.SlotBytes {
		if b <= 0 {
			continue
		}
		saved := false
		for _, s := range st.Snapshots {
			if s.Repo == repo && idle > 0 && s.At >= idle {
				saved = true
			}
		}
		if !saved {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	return repos
}

// saveBeforeStop snapshots the changed build slots of a machine that is
// about to stop or be destroyed, when its provider has a store: other
// machines can then restore them (scheduler cache affinity). Failures are
// logged; the machine stops anyway.
func (o *Orchestrator) saveBeforeStop(ctx context.Context, id, reason string) {
	o.mu.Lock()
	t, m, ok := o.slotTargetLocked(id)
	slots, stores := o.Slots, o.Stores
	var repos []string
	var prov string
	if ok && m.State == provider.Running {
		repos, prov = unsavedSlots(m), m.Provider
	}
	o.mu.Unlock()
	if !ok || slots == nil || stores == nil || len(repos) == 0 {
		return
	}
	if g, err := stores.Granter(ctx, prov); err != nil || g == nil {
		return
	}
	for _, repo := range repos {
		sctx, cancel := context.WithTimeout(ctx, saveTimeout)
		snap, err := o.saveSlotLogged(sctx, slots, t, prov, repo, "before "+reason)
		cancel()
		if err != nil {
			o.log.Warn("snapshot before stop failed", "machine", id, "repo", repo, "err", err)
			continue
		}
		o.log.Info("snapshot before stop", "machine", id, "repo", repo, "bytes", snap.Bytes)
	}
}

// moveSlot performs a SlotMove: save the warm slot on a.From, then restore
// that snapshot on the target.
func (o *Orchestrator) moveSlot(ctx context.Context, slots SlotOps, t SlotTarget, a scheduler.SlotAction) error {
	o.mu.Lock()
	src, sm, ok := o.slotTargetLocked(a.From)
	var prov string
	if ok {
		prov = sm.Provider
	}
	o.mu.Unlock()
	if !ok {
		return errors.New("move source " + a.From + " is not reachable")
	}
	snap, err := o.saveSlotLogged(ctx, slots, src, prov, a.Repo, "move to "+t.Machine)
	if err != nil {
		return err
	}
	return slots.RestoreSlot(ctx, t, a.Repo, snap)
}

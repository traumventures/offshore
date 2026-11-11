package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// Provider health: a capacity or quota error takes a provider out of
// scale-out for a cooldown that doubles on every further error (up to
// maxCooldown) and resets once a machine of the provider becomes ready.
const (
	baseCooldown = 2 * time.Minute
	maxCooldown  = 30 * time.Minute
	// startBackoff blocks starting a stopped machine after a failed start.
	startBackoff = 2 * time.Minute
	// slotTimeout bounds slot preparation (prune, resize, restore) before
	// a lease is handed out; on failure the session runs cold.
	slotTimeout = 5 * time.Minute
)

// provHealth is the scheduling state of a provider instance.
type provHealth struct {
	down     time.Time
	cooldown time.Duration
	boot     time.Duration // learned create -> ready latency (EWMA)
	start    time.Duration // learned start -> ready latency (EWMA)
}

// leaseInfo is the expected end of a lease's session.
type leaseInfo struct {
	start    time.Time
	expected time.Duration
}

func ewma(old, v time.Duration) time.Duration {
	if old <= 0 {
		return v
	}
	return (old*7 + v*3) / 10
}

// providerFailedLocked records a failed create or start: capacity and
// quota errors downgrade the provider for a cooldown with backoff.
func (o *Orchestrator) providerFailedLocked(prov string, err error) {
	if !errors.Is(err, provider.ErrCapacity) {
		return
	}
	h := o.health[prov]
	if h.cooldown <= 0 {
		h.cooldown = baseCooldown
	} else {
		h.cooldown = min(h.cooldown*2, maxCooldown)
	}
	h.down = o.now().Add(h.cooldown)
	o.health[prov] = h
	o.log.Warn("provider out of capacity, cooling down", "provider", prov, "until", h.down, "err", err)
	o.obs.log.Append(events.Event{Type: events.ProviderDown, Provider: prov, Error: err.Error(), Seconds: h.cooldown.Seconds()})
}

// providerReadyLocked records a machine of prov that became ready after d.
func (o *Orchestrator) providerReadyLocked(prov string, d time.Duration, started bool) {
	h := o.health[prov]
	h.cooldown, h.down = 0, time.Time{}
	if started {
		h.start = ewma(h.start, d)
	} else {
		h.boot = ewma(h.boot, d)
	}
	o.health[prov] = h
}

// snapshotsPath persists known build slot snapshots.
func (o *Orchestrator) snapshotsPath() string { return filepath.Join(o.stateDir, "snapshots.json") }

func (o *Orchestrator) loadSnapshots() {
	b, err := os.ReadFile(o.snapshotsPath())
	if err != nil {
		return
	}
	var m map[string][]scheduler.Snapshot
	if json.Unmarshal(b, &m) == nil && m != nil {
		o.snapshots = m
	}
}

func (o *Orchestrator) saveSnapshotsLocked() {
	b, err := json.Marshal(o.snapshots)
	if err != nil {
		return
	}
	tmp := o.snapshotsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, o.snapshotsPath())
	}
}

// snapshotsKeep bounds the snapshots remembered per repository.
const snapshotsKeep = 4

// addSnapshotLocked remembers a snapshot (newest first).
func (o *Orchestrator) addSnapshotLocked(s scheduler.Snapshot) {
	if s.Repo == "" || s.At.IsZero() {
		return
	}
	ss := o.snapshots[s.Repo]
	for _, x := range ss {
		if x.Store == s.Store && x.At.Equal(s.At) {
			return
		}
	}
	ss = append(ss, s)
	sort.Slice(ss, func(i, j int) bool { return ss[i].At.After(ss[j].At) })
	if len(ss) > snapshotsKeep {
		ss = ss[:snapshotsKeep]
	}
	o.snapshots[s.Repo] = ss
	o.saveSnapshotsLocked()
}

// statusSnapshotsLocked takes the snapshots a worker reports.
func (o *Orchestrator) statusSnapshotsLocked(m *Machine) {
	if m.Status == nil {
		return
	}
	for _, si := range m.Status.Snapshots {
		prov := si.Provider
		if prov == "" {
			prov = m.Provider
		}
		o.addSnapshotLocked(scheduler.Snapshot{Repo: si.Repo, Provider: prov, Store: si.Store, Bytes: si.Bytes, At: time.Unix(si.At, 0)})
	}
}

// schedPoolLocked adds the cost model's inputs to a pool snapshot:
// provider latencies and health, snapshots, expected releases, readiness,
// disk resizability and the demand forecast.
func (o *Orchestrator) schedPoolLocked(f *scheduler.Pool) {
	f.Providers = map[string]scheduler.ProviderState{}
	for name := range o.provs {
		h := o.health[name]
		f.Providers[name] = scheduler.ProviderState{Boot: h.boot, Start: h.start, Down: h.down, Store: name}
	}
	f.Snapshots = map[string][]scheduler.Snapshot{}
	for repo, ss := range o.snapshots {
		f.Snapshots[repo] = append([]scheduler.Snapshot(nil), ss...)
	}
	for i := range f.Machines {
		sm := &f.Machines[i]
		if p, ok := o.provs[sm.Provider]; ok {
			_, sm.CanResize = p.(provider.DiskResizer)
		}
		sm.StartBlocked = o.startBlocked[sm.ID]
		if c, ok := o.creating[sm.ID]; ok && sm.State == scheduler.Starting {
			lat := f.Providers[sm.Provider].Boot
			if c.started {
				lat = f.Providers[sm.Provider].Start
			}
			if lat > 0 {
				sm.ReadyAt = c.at.Add(lat)
			}
		}
		m := o.machines[sm.ID]
		if m == nil {
			continue
		}
		seen := map[string]bool{}
		if m.Status != nil {
			for _, se := range m.Status.Sessions {
				r := scheduler.Release{Res: scheduler.Resources{CPU: se.CPU, Memory: se.Memory}, Start: time.Unix(se.Started, 0)}
				if li, ok := o.leases[se.Lease]; ok && se.Lease != "" {
					r.At, seen[se.Lease] = li.start.Add(li.expected), true
				} else {
					r.At = r.Start.Add(scheduler.DefaultRun)
				}
				sm.Releases = append(sm.Releases, r)
			}
		}
		for lid, pl := range m.pending {
			if seen[lid] {
				continue
			}
			if li, ok := o.leases[lid]; ok {
				sm.Releases = append(sm.Releases, scheduler.Release{At: li.start.Add(li.expected), Res: pl.res, Start: li.start})
			}
		}
	}
	o.forecast.Observe(*f)
	o.forecast.Apply(f)
}

// planLocked runs the policy's Plan on the current pool.
func (o *Orchestrator) planLocked() []scheduler.Action {
	f := o.poolLocked(o.now())
	acts := o.policy.Plan(f)
	o.forecast.Note(f, acts)
	return acts
}

// slotTarget addresses a machine for slot operations.
func (o *Orchestrator) slotTargetLocked(id string) (SlotTarget, *Machine, bool) {
	m, ok := o.machines[id]
	if !ok || m.Addr == "" {
		return SlotTarget{}, nil, false
	}
	return SlotTarget{Machine: id, Addr: m.Addr, Fingerprint: m.Fingerprint, Token: o.AdminToken(id, slotTimeout+time.Minute)}, m, true
}

// prepareSlot performs a placement's slot action on the machine before
// the session starts (prune, disk resize, snapshot restore). Failures are
// logged; the session then runs on whatever slot is there (cold).
func (o *Orchestrator) prepareSlot(ctx context.Context, machineID string, a scheduler.SlotAction) {
	if a.Kind == scheduler.SlotNone {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, slotTimeout)
	defer cancel()
	o.mu.Lock()
	t, m, ok := o.slotTargetLocked(machineID)
	var p provider.Provider
	if ok {
		p = o.provs[m.Provider]
	}
	slots := o.Slots
	o.mu.Unlock()
	if !ok {
		return
	}
	start := time.Now()
	ev := o.machineEvent(events.SlotAction, machineID)
	ev.Repo, ev.Reason, ev.Bytes = a.Repo, a.Kind.String(), a.Bytes
	var err error
	switch a.Kind {
	case scheduler.SlotPrune:
		if slots == nil {
			err = errors.New("no slot operations configured")
			break
		}
		var freed int64
		freed, err = slots.PruneSlot(ctx, t, a.Repo, a.Bytes)
		ev.Bytes = freed
	case scheduler.SlotResize:
		r, ok := p.(provider.DiskResizer)
		if !ok {
			err = errors.New("provider cannot resize disks")
			break
		}
		err = r.ResizeDisk(ctx, machineID, a.Bytes)
	case scheduler.SlotRestore:
		if slots == nil {
			err = errors.New("no slot operations configured")
			break
		}
		ev.Store, ev.Bytes = a.Snapshot.Store, a.Snapshot.Bytes
		err = slots.RestoreSlot(ctx, t, a.Repo, a.Snapshot)
	}
	ev.Seconds = time.Since(start).Seconds()
	if err != nil {
		ev.Error = err.Error()
		o.log.Warn("slot preparation failed, running cold", "machine", machineID, "action", a.Kind, "repo", a.Repo, "err", err)
	}
	o.obs.log.Append(ev)
}

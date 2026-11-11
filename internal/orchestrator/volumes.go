package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// VolumeState is a cache volume the orchestrator manages (volumes.json):
// attached to a machine of a volume size, or detached and kept for reuse
// (keep_detached) until ExpiresAt.
type VolumeState struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Zone     string `json:"zone,omitempty"`
	Bytes    int64  `json:"bytes"`
	Machine  string `json:"machine,omitempty"` // attached to (empty: detached)
	// SlotBytes are the build slots on the volume when its machine last
	// reported them (repository -> bytes).
	SlotBytes       map[string]int64       `json:"slot_bytes,omitempty"`
	PricePerGBMonth float64                `json:"price_per_gb_month,omitempty"`
	Policy          scheduler.VolumePolicy `json:"policy"`
	CreatedAt       time.Time              `json:"created_at"`
	DetachedAt      time.Time              `json:"detached_at,omitzero"`
	ExpiresAt       time.Time              `json:"expires_at,omitzero"`
	ResizedAt       time.Time              `json:"resized_at,omitzero"`
	// costAt is when storage cost was last charged to an event.
	CostAt time.Time `json:"cost_at"`

	releasing bool // releaseVolume in progress
}

// volumeWait bounds waiting for a destroyed machine to release its volume.
var volumeWait = 10 * time.Minute

func (o *Orchestrator) volumesPath() string { return filepath.Join(o.stateDir, "volumes.json") }

func (o *Orchestrator) loadVolumes() {
	b, err := os.ReadFile(o.volumesPath())
	if err != nil {
		return
	}
	var m map[string]*VolumeState
	if json.Unmarshal(b, &m) == nil && m != nil {
		o.volumes = m
	}
}

func (o *Orchestrator) saveVolumesLocked() {
	b, err := json.Marshal(o.volumes)
	if err != nil {
		return
	}
	tmp := o.volumesPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, o.volumesPath())
	}
}

// Volumes returns the managed cache volumes, sorted by id.
func (o *Orchestrator) Volumes() []VolumeState {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]VolumeState, 0, len(o.volumes))
	for _, v := range o.volumes {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// volumeOfLocked returns the volume attached to a machine.
func (o *Orchestrator) volumeOfLocked(machineID string) *VolumeState {
	for _, v := range o.volumes {
		if v.Machine == machineID {
			return v
		}
	}
	return nil
}

// volumeEventLocked is an event about v; Cost charges its storage since
// the last event at its size (the costs of a volume's events add up to
// its storage cost).
func (o *Orchestrator) volumeEventLocked(typ string, v *VolumeState) events.Event {
	now := o.now()
	ev := events.Event{Type: typ, Volume: v.ID, Provider: v.Provider, Zone: v.Zone, Machine: v.Machine, Bytes: v.Bytes}
	if !v.CostAt.IsZero() && now.After(v.CostAt) {
		ev.Cost = float64(v.Bytes) / 1e9 * v.PricePerGBMonth / medium.HoursPerMonth * now.Sub(v.CostAt).Hours()
	}
	v.CostAt = now
	return ev
}

// acquireVolume returns the volume a new machine of size gets: the
// detached volume the policy chose (want), else a reusable detached one
// of the provider (the warmest for the environment's waiting repository,
// then the newest), else a new one.
func (o *Orchestrator) acquireVolume(ctx context.Context, id string, size scheduler.Size, want, repo string) (*VolumeState, error) {
	pv := provider.Split(o.provs[size.Provider]).Volumes
	if pv == nil {
		return nil, fmt.Errorf("provider %s has no volumes", size.Provider)
	}
	now := o.now()
	o.mu.Lock()
	var best *VolumeState
	for _, v := range o.volumes {
		if v.Provider != size.Provider || v.Machine != "" || (!v.ExpiresAt.IsZero() && !now.Before(v.ExpiresAt)) {
			continue
		}
		if want != "" {
			if v.ID == want {
				best = v
				break
			}
			continue
		}
		if best == nil || v.SlotBytes[repo] > best.SlotBytes[repo] ||
			v.SlotBytes[repo] == best.SlotBytes[repo] && v.DetachedAt.After(best.DetachedAt) {
			best = v
		}
	}
	if best != nil {
		best.Machine, best.ExpiresAt = id, time.Time{} // reserved for id
		o.saveVolumesLocked()
		o.mu.Unlock()
		return best, nil
	}
	o.mu.Unlock()
	pol := size.Volume
	start := time.Now()
	pvol, err := pv.CreateVolume(ctx, provider.VolumeSpec{
		Owner: o.cfg.Owner, Name: "cache-" + id, Bytes: pol.Min,
		Type: pol.Type, IOPS: pol.IOPS, Throughput: pol.Throughput, VPUs: pol.VPUs,
	})
	if err != nil {
		return nil, o.provErr(size.Provider, "create_volume", err)
	}
	v := &VolumeState{ID: pvol.ID, Provider: size.Provider, Name: pvol.Name, Zone: pvol.Zone, Bytes: pvol.Bytes,
		Machine: id, PricePerGBMonth: scheduler.SlotMedium(size.Stack).PricePerGBMonth, Policy: pol, CreatedAt: now}
	o.mu.Lock()
	o.volumes[v.ID] = v
	ev := o.volumeEventLocked(events.VolumeCreate, v)
	o.saveVolumesLocked()
	o.mu.Unlock()
	ev.Seconds = time.Since(start).Seconds()
	o.obs.log.Append(ev)
	return v, nil
}

// attachedVolume records that machine id got v (after a successful create).
func (o *Orchestrator) attachedVolume(v *VolumeState, zone string) {
	o.mu.Lock()
	if zone != "" {
		v.Zone = zone
	}
	v.DetachedAt = time.Time{}
	ev := o.volumeEventLocked(events.VolumeAttach, v)
	o.saveVolumesLocked()
	o.mu.Unlock()
	o.obs.log.Append(ev)
}

// releaseVolume handles the volume of a machine that is gone (destroyed or
// failed to create): kept detached for reuse until its TTL (keep_detached)
// or deleted. The provider releases a destroyed machine's volume when the
// instance has terminated: wait for that.
func (o *Orchestrator) releaseVolume(ctx context.Context, machineID string) {
	o.mu.Lock()
	v := o.volumeOfLocked(machineID)
	if v == nil || v.releasing {
		o.mu.Unlock()
		return
	}
	v.releasing = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		v.releasing = false
		o.mu.Unlock()
	}()
	pv := provider.Split(o.provs[v.Provider]).Volumes
	if pv == nil {
		return
	}
	deadline := time.Now().Add(volumeWait)
	for {
		pvs, err := pv.ListVolumes(ctx, o.cfg.Owner)
		if err == nil {
			state := provider.VolumeState("")
			for _, x := range pvs {
				if x.ID == v.ID {
					state = x.State
				}
			}
			if state == provider.VolumeAvailable || state == "" {
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			o.log.Warn("volume not released by its machine; the reaper retries", "volume", v.ID, "machine", machineID)
			o.mu.Lock()
			v.Machine, v.DetachedAt, v.ExpiresAt = "", o.now(), o.now()
			if v.Policy.KeepDetached {
				v.ExpiresAt = o.now().Add(v.Policy.TTL)
			}
			o.saveVolumesLocked()
			o.mu.Unlock()
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	o.mu.Lock()
	keep := v.Policy.KeepDetached && v.Policy.TTL > 0
	now := o.now()
	ev := o.volumeEventLocked(events.VolumeDetach, v)
	v.Machine, v.DetachedAt = "", now
	if keep {
		v.ExpiresAt = now.Add(v.Policy.TTL)
	}
	o.saveVolumesLocked()
	o.mu.Unlock()
	if keep {
		if err := o.provErr(v.Provider, "detach_volume", pv.DetachVolume(ctx, v.ID, v.ExpiresAt)); err != nil {
			ev.Error = err.Error()
		}
		o.obs.log.Append(ev)
		return
	}
	o.obs.log.Append(ev)
	o.deleteVolume(ctx, pv, v)
}

func (o *Orchestrator) deleteVolume(ctx context.Context, pv provider.Volumes, v *VolumeState) {
	err := pv.DeleteVolume(ctx, v.ID)
	o.mu.Lock()
	ev := o.volumeEventLocked(events.VolumeDelete, v)
	if err == nil {
		delete(o.volumes, v.ID)
	} else {
		ev.Error = err.Error()
	}
	o.saveVolumesLocked()
	o.mu.Unlock()
	if err != nil {
		o.provErr(v.Provider, "delete_volume", err)
	}
	o.obs.log.Append(ev)
}

// resizeVolume grows the volume of a machine to at least bytes: by the
// policy's step at least, at most to its max; never within the
// provider's resize cooldown.
func (o *Orchestrator) resizeVolume(ctx context.Context, machineID string, bytes int64) error {
	o.mu.Lock()
	v := o.volumeOfLocked(machineID)
	o.mu.Unlock()
	if v == nil {
		return errors.New("machine has no cache volume")
	}
	pv := provider.Split(o.provs[v.Provider]).Volumes
	if pv == nil {
		return errors.New("provider has no volumes")
	}
	target := volumeTarget(v.Policy, v.Bytes, bytes)
	if target <= v.Bytes {
		return fmt.Errorf("volume %s at its max size %d", v.ID, v.Bytes)
	}
	if cd := pv.VolumeCaps().ResizeCooldown; cd > 0 && !v.ResizedAt.IsZero() && o.now().Before(v.ResizedAt.Add(cd)) {
		return fmt.Errorf("volume %s: %w (until %s)", v.ID, provider.ErrResizeCooldown, v.ResizedAt.Add(cd).Format(time.RFC3339))
	}
	start := time.Now()
	if err := pv.ResizeVolume(ctx, v.ID, target); err != nil {
		return o.provErr(v.Provider, "resize_volume", err)
	}
	o.mu.Lock()
	ev := o.volumeEventLocked(events.VolumeResize, v)
	v.Bytes, v.ResizedAt = target, o.now()
	ev.Bytes = target
	o.saveVolumesLocked()
	o.mu.Unlock()
	ev.Seconds = time.Since(start).Seconds()
	o.obs.log.Append(ev)
	return nil
}

// volumeTarget is the size a resize to need grows a volume of cur to:
// at least one step, at most max (0: unbounded).
func volumeTarget(p scheduler.VolumePolicy, cur, need int64) int64 {
	t := max(need, cur+p.Step)
	if p.Max > 0 {
		t = min(t, p.Max)
	}
	return t
}

// statusVolumeLocked remembers the build slots on a machine's volume.
func (o *Orchestrator) statusVolumeLocked(m *Machine) {
	if m.Status == nil || m.Status.Disk == nil {
		return
	}
	if v := o.volumeOfLocked(m.ID); v != nil {
		v.SlotBytes = m.Status.Disk.SlotBytes
	}
}

// poolVolumesLocked adds volume facts to the scheduler's pool: each
// machine's volume and the detached volumes kept for reuse.
func (o *Orchestrator) poolVolumesLocked(f *scheduler.Pool) {
	now := f.Now
	for i := range f.Machines {
		sm := &f.Machines[i]
		if m, ok := o.machines[sm.ID]; ok {
			sm.Zone = m.Zone
		}
		for _, s := range o.cfg.Sizes {
			if s.Provider == sm.Provider && s.Name == sm.Size {
				sm.Medium, sm.Stack = s.Medium, s.Stack
			}
		}
		if v := o.volumeOfLocked(sm.ID); v != nil {
			sm.Medium, sm.Volume = medium.Volume, v.ID
			if sm.Zone == "" {
				sm.Zone = v.Zone
			}
			if cd := scheduler.SlotMedium(sm.Stack).ResizeCooldown; cd > 0 && !v.ResizedAt.IsZero() {
				sm.ResizableAt = v.ResizedAt.Add(cd)
			}
			if p, ok := o.provs[sm.Provider]; ok && provider.Split(p).Volumes != nil {
				sm.CanResize = v.Policy.Max == 0 || v.Bytes < v.Policy.Max
			}
		}
	}
	for _, v := range o.volumes {
		if v.Machine != "" || (!v.ExpiresAt.IsZero() && !now.Before(v.ExpiresAt)) {
			continue
		}
		f.Volumes = append(f.Volumes, scheduler.Volume{ID: v.ID, Provider: v.Provider, Zone: v.Zone, Bytes: v.Bytes,
			SlotBytes: v.SlotBytes, ExpiresAt: v.ExpiresAt})
	}
	sort.Slice(f.Volumes, func(i, j int) bool { return f.Volumes[i].ID < f.Volumes[j].ID })
}

// syncVolumes reconciles the managed volumes with a provider's listing:
// managed volumes the provider no longer has are forgotten, expired
// detached ones deleted.
func (o *Orchestrator) syncVolumes(ctx context.Context, prov string, pv provider.Volumes, listed []provider.Volume) {
	byID := map[string]provider.Volume{}
	for _, v := range listed {
		byID[v.ID] = v
	}
	now := o.now()
	var expired []*VolumeState
	o.mu.Lock()
	for id, v := range o.volumes {
		if v.Provider != prov {
			continue
		}
		pvol, ok := byID[id]
		if !ok {
			// Created after the listing started: not missing.
			if now.Sub(v.CreatedAt) > time.Minute {
				delete(o.volumes, id)
			}
			continue
		}
		v.Bytes = max(v.Bytes, pvol.Bytes)
		if v.Machine == "" && !v.ExpiresAt.IsZero() && !now.Before(v.ExpiresAt) && pvol.State == provider.VolumeAvailable {
			expired = append(expired, v)
		}
	}
	o.saveVolumesLocked()
	o.mu.Unlock()
	for _, v := range expired {
		o.deleteVolume(ctx, pv, v)
	}
}

// managedLocked reports whether a provider volume is managed here.
func (o *Orchestrator) managedLocked(id string) bool {
	_, ok := o.volumes[id]
	return ok
}

// waitingRepoLocked is the repository of the longest waiting task of an
// environment ("" when none waits).
func (o *Orchestrator) waitingRepoLocked(env string) string {
	var best scheduler.Task
	for _, t := range o.queue {
		if t.EnvHash == env && (best.ID == "" || t.Since.Before(best.Since)) {
			best = t
		}
	}
	return best.RepoID
}

// resolveLocalDisks sets the disk of local-medium sizes without one from
// their instance type's instance store (and its profile), once.
func (o *Orchestrator) resolveLocalDisks(ctx context.Context) {
	o.mu.Lock()
	sizes := append([]scheduler.Size(nil), o.cfg.Sizes...)
	prof := o.cfg.Profile
	o.mu.Unlock()
	changed := false
	for i, s := range sizes {
		if s.Medium == medium.Volume || s.Capacity.Disk > 0 {
			continue
		}
		ld, ok := o.provs[s.Provider].(provider.LocalDisks)
		if !ok {
			continue
		}
		typ := s.Name
		if len(s.InstanceTypes) > 0 {
			typ = s.InstanceTypes[0]
		}
		n, err := ld.InstanceStorage(ctx, typ)
		if err != nil {
			o.log.Warn("instance storage of a size", "size", s.Name, "err", err)
			continue
		}
		if n <= 0 {
			continue
		}
		sizes[i].Capacity.Disk = n
		nvme := medium.Default(o.provs[s.Provider].Name(), medium.LocalNVMe)
		if prof != nil {
			nvme = prof(s.Provider, medium.LocalNVMe)
		}
		sizes[i].Stack = scheduler.WithTop(s.Stack, nvme, n)
		changed = true
		o.log.Info("size caches on its instance store", "size", s.Name, "bytes", n)
	}
	if changed {
		o.mu.Lock()
		o.cfg.Sizes = sizes
		o.mu.Unlock()
	}
}

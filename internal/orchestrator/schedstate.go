package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// Provider health: a capacity or quota error takes a provider out of
// scale-out for a cooldown that doubles on every further error (up to
// maxCooldown) and resets once a machine of the provider becomes ready.
const (
	baseCooldown = 2 * time.Minute
	maxCooldown  = 30 * time.Minute
	// startBackoff blocks starting a stopped machine after a failed start.
	startBackoff = 2 * time.Minute
	// Any other failed create (environment image, provider error, machine
	// not becoming ready) blocks creates of the environment on the
	// provider for a backoff that doubles per further failure up to
	// maxCreateBackoff and resets on the next ready machine.
	baseCreateBackoff = 10 * time.Second
	maxCreateBackoff  = 10 * time.Minute

	// slotTimeout bounds slot preparation (prune, resize, restore) before
	// a lease is handed out; on failure the session runs cold.
	slotTimeout = 5 * time.Minute
)

// reconcileWait bounds how long Reconcile waits for its stops and
// destroys (snapshot saves before a stop can take minutes); a var for tests.
var reconcileWait = 5 * time.Second

// provHealth is the scheduling state of a provider instance.
type provHealth struct {
	down     time.Time
	cooldown time.Duration
	// disabled is the class of the error that disabled the provider
	// (account_limit, auth; "" = enabled).
	disabled    provider.ErrorClass
	disabledErr string
	disabledAt  time.Time
	boot        time.Duration // learned create -> ready latency (EWMA)
	start       time.Duration // learned start -> ready latency (EWMA)
}

// createBackoff blocks creates of an environment on a provider after a
// failed create.
type createBackoff struct {
	until time.Time
	delay time.Duration
	err   string
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

// providerFailedLocked records a failed create or start of prov and
// reports whether the provider took it (a cooldown or a disable; else the
// caller applies the per-environment create backoff). The error class
// decides (provider.Classify): capacity and quota errors downgrade the
// provider for a cooldown with backoff; account limits and auth errors
// disable it until the configuration changes or an operator resets it
// (ResetProvider), loudly: waiting does not heal them.
func (o *Orchestrator) providerFailedLocked(prov string, err error) bool {
	class := provider.Classify(err)
	h := o.health[prov]
	switch {
	case class.Disables():
		first := h.disabled == ""
		h.disabled, h.disabledErr, h.disabledAt = class, err.Error(), o.now()
		h.down = disabledUntil
		o.health[prov] = h
		if first {
			o.log.Error("PROVIDER DISABLED: it gets no machines until its configuration changes or `offshore provider reset "+prov+"`",
				"provider", prov, "class", class, "err", err)
		}
		o.obs.log.Append(events.Event{Type: events.ProviderDown, Provider: prov, Error: err.Error(), ErrorClass: string(class), Reason: "disabled"})
		return true
	case class.CoolsDown():
		if h.cooldown <= 0 {
			h.cooldown = baseCooldown
		} else {
			h.cooldown = min(h.cooldown*2, maxCooldown)
		}
		h.down = o.now().Add(h.cooldown)
		o.health[prov] = h
		o.log.Warn("provider out of "+string(class)+", cooling down", "provider", prov, "until", h.down, "err", err)
		o.obs.log.Append(events.Event{Type: events.ProviderDown, Provider: prov, Error: err.Error(), ErrorClass: string(class), Reason: "cooldown", Seconds: h.cooldown.Seconds()})
		return true
	}
	return false
}

// disabledUntil stands in for "no end" in the scheduler's provider state.
var disabledUntil = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// ProviderHealth is the scheduling state of a provider for status and
// doctor.
type ProviderHealth struct {
	Name string `json:"name"`
	// State is ok, cooldown (capacity or quota, until Until) or disabled
	// (account limit or auth error, until a configuration change or a
	// reset).
	State      string    `json:"state"`
	ErrorClass string    `json:"error_class,omitempty"`
	Error      string    `json:"error,omitempty"`
	Since      time.Time `json:"since,omitzero"`
	Until      time.Time `json:"until,omitzero"`
	// BootSeconds and StartSeconds are the learned create and start
	// latencies (0: unknown).
	BootSeconds  float64 `json:"boot_seconds,omitempty"`
	StartSeconds float64 `json:"start_seconds,omitempty"`
}

// Providers returns the health of every configured provider, by name.
func (o *Orchestrator) Providers() []ProviderHealth {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	var out []ProviderHealth
	for name := range o.provs {
		h := o.health[name]
		ph := ProviderHealth{Name: name, State: "ok", BootSeconds: h.boot.Seconds(), StartSeconds: h.start.Seconds()}
		switch {
		case h.disabled != "":
			ph.State, ph.ErrorClass, ph.Error, ph.Since = "disabled", string(h.disabled), h.disabledErr, h.disabledAt
		case now.Before(h.down):
			ph.State, ph.Until = "cooldown", h.down
		}
		out = append(out, ph)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResetProvider re-enables a disabled provider and ends its cooldown.
func (o *Orchestrator) ResetProvider(name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.provs[name]; !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	o.resetProviderLocked(name, "manual reset")
	o.notify()
	return nil
}

func (o *Orchestrator) resetProviderLocked(name, why string) {
	h := o.health[name]
	if h.disabled == "" && h.down.IsZero() {
		return
	}
	if h.disabled != "" {
		o.log.Warn("provider re-enabled", "provider", name, "why", why)
	}
	h.disabled, h.disabledErr, h.disabledAt, h.down, h.cooldown = "", "", time.Time{}, time.Time{}, 0
	o.health[name] = h
}

// providerReadyLocked records a machine of prov that became ready after d.
func (o *Orchestrator) providerReadyLocked(prov string, d time.Duration, started bool) {
	h := o.health[prov]
	h.cooldown, h.down = 0, time.Time{}
	h.disabled, h.disabledErr, h.disabledAt = "", "", time.Time{}
	if started {
		h.start = ewma(h.start, d)
	} else {
		h.boot = ewma(h.boot, d)
	}
	o.health[prov] = h
}

// createFailedLocked records a failed create of env on prov and returns
// the backoff before the next create there.
func (o *Orchestrator) createFailedLocked(env, prov string, err error) time.Duration {
	key := scheduler.CreateKey(env, prov)
	b := o.createFail[key]
	if b.delay <= 0 {
		b.delay = baseCreateBackoff
	} else {
		b.delay = min(b.delay*2, maxCreateBackoff)
	}
	b.until, b.err = o.now().Add(b.delay), err.Error()
	o.createFail[key] = b
	return b.delay
}

// createBlockedLocked returns the active create backoffs of env.
func (o *Orchestrator) createBlockedLocked(env string) []createBackoff {
	var out []createBackoff
	now := o.now()
	for key, b := range o.createFail {
		if strings.HasPrefix(key, env+"/") && now.Before(b.until) {
			out = append(out, b)
		}
	}
	return out
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
	f.CreateBlocked = map[string]time.Time{}
	for key, b := range o.createFail {
		f.CreateBlocked[key] = b.until
	}
	f.Snapshots = map[string][]scheduler.Snapshot{}
	for repo, rl := range o.heads {
		f.Snapshots[repo] = append([]scheduler.Snapshot(nil), rl.Heads...)
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
	o.poolVolumesLocked(f)
	o.forecast.Observe(*f)
	o.forecast.Apply(f)
}

// planLocked runs the policy's Plan on the current pool. Creates the
// pool does not allow (provider cooldown, create backoff) are dropped
// whatever the policy decided: a failing create must never spin.
func (o *Orchestrator) planLocked() []scheduler.Action {
	f := o.poolLocked(o.now())
	acts := o.policy.Plan(f)
	kept := acts[:0]
	for _, a := range acts {
		if a.Kind == scheduler.Create && !f.CanCreate(a.EnvHash, a.Size.Provider) {
			continue
		}
		kept = append(kept, a)
	}
	acts = kept
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
// the session starts (prune, disk resize, snapshot restore, a restore
// with its disk resize in one step). Failures are
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
	ev.Repo, ev.Reason, ev.Bytes = a.Repo, a.Steps(), a.Bytes
	prune := func(bytes int64) (int64, error) {
		if slots == nil {
			return 0, errors.New("no slot operations configured")
		}
		return slots.PruneSlot(ctx, t, a.Repo, bytes)
	}
	resize := func(bytes int64) error {
		o.mu.Lock()
		vol := o.volumeOfLocked(machineID)
		o.mu.Unlock()
		if vol != nil {
			return o.resizeVolume(ctx, machineID, bytes)
		}
		r, ok := p.(provider.DiskResizer)
		if !ok {
			return errors.New("provider cannot resize disks")
		}
		return r.ResizeDisk(ctx, machineID, bytes)
	}
	var err error
	// A restore or move grows the disk in the same step; a failure there
	// is logged and the restore proceeds (it may thrash).
	if a.Grow > 0 {
		if rerr := resize(a.Grow); rerr != nil {
			o.log.Warn("disk resize before restore failed", "machine", machineID, "repo", a.Repo, "err", rerr)
		}
	}
	switch a.Kind {
	case scheduler.SlotPrune:
		var freed int64
		freed, err = prune(a.Bytes)
		ev.Bytes = freed
	case scheduler.SlotResize:
		err = resize(a.Bytes)
	case scheduler.SlotRestore:
		if slots == nil {
			err = errors.New("no slot operations configured")
			break
		}
		ev.Store, ev.Bytes = a.Snapshot.Store, a.Snapshot.Bytes
		err = slots.RestoreSlot(ctx, t, a.Repo, a.Snapshot)
	case scheduler.SlotMove:
		if slots == nil {
			err = errors.New("no slot operations configured")
			break
		}
		ev.Store, ev.Bytes = a.Snapshot.Store, a.Bytes
		err = o.moveSlot(ctx, slots, t, a)
	}
	ev.Seconds = time.Since(start).Seconds()
	if err != nil {
		ev.Error = err.Error()
		o.log.Warn("slot preparation failed, running cold", "machine", machineID, "action", a.Kind, "repo", a.Repo, "err", err)
	}
	o.obs.log.Append(ev)
}

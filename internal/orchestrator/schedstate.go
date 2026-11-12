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

// pruneTimeout bounds a prune before a placement: the session keeps its
// warm slot when the prune fails, so waiting longer costs more than the
// disk the prune would free (a normal prune takes about a second).
var pruneTimeout = 90 * time.Second

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
	// quota is the account's vCPU limit for the pool (provider.Quota),
	// if hasQuota; quotaAt when it was read.
	quota    float64
	hasQuota bool
	quotaAt  time.Time
	// capCPU caps the pool's vCPUs on the provider until capUntil after a
	// limit error: at what it ran when the account said no more.
	capCPU   float64
	capUntil time.Time
}

// limitCap is how long a cap from a limit error holds before the
// scheduler may try more machines on the provider again.
const limitCap = time.Hour

// quotaRefresh is how often provider quotas are read.
const quotaRefresh = 10 * time.Minute

// providerCPULocked is the vCPUs of the pool's running and starting
// machines of prov.
func (o *Orchestrator) providerCPULocked(prov string) float64 {
	cpu := 0.0
	for _, m := range o.machines {
		if m.Provider == prov && (m.State == provider.Running || m.State == provider.Starting) {
			cpu += m.CPU
		}
	}
	return cpu
}

// limit is the hard limit of prov for the scheduler: the smaller of
// the discovered quota and a cap after a limit error.
func (h provHealth) limit(now time.Time) scheduler.Limit {
	var l scheduler.Limit
	if h.hasQuota {
		// Limit.CPU 0 means none: a used-up quota is a tiny limit.
		l = scheduler.Limit{CPU: max(h.quota, 1e-3), Source: "quota"}
	}
	if h.capCPU > 0 && now.Before(h.capUntil) && (l.CPU == 0 || h.capCPU < l.CPU) {
		l = scheduler.Limit{CPU: h.capCPU, Source: "limit error"}
	}
	return l
}

// refreshQuotas reads the vCPU quotas of the providers that report them
// (provider.Quota), at most every quotaRefresh.
func (o *Orchestrator) refreshQuotas(ctx context.Context) {
	now := o.now()
	type job struct {
		name string
		q    provider.Quota
		own  float64
	}
	var jobs []job
	o.mu.Lock()
	for name, p := range o.provs {
		q, ok := p.(provider.Quota)
		if !ok || o.cfg.Policy.Disabled(name) || now.Sub(o.health[name].quotaAt) < quotaRefresh {
			continue
		}
		h := o.health[name]
		h.quotaAt = now
		o.health[name] = h
		jobs = append(jobs, job{name, q, o.providerCPULocked(name)})
	}
	o.mu.Unlock()
	for _, j := range jobs {
		qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		v, ok, err := j.q.Quota(qctx, o.cfg.Owner, j.own)
		cancel()
		if err != nil {
			o.log.Warn("provider quota unknown", "provider", j.name, "err", err)
			continue
		}
		o.mu.Lock()
		h := o.health[j.name]
		if h.quota != v || h.hasQuota != ok {
			o.log.Info("provider quota", "provider", j.name, "vcpus", v, "known", ok, "own", j.own)
		}
		h.quota, h.hasQuota = v, ok
		o.health[j.name] = h
		o.mu.Unlock()
	}
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
	expected time.Duration // pessimistic run (release forecast)
	run      time.Duration // median run (0: unknown)
	// memMean, memSD: the peak memory belief at placement (statistical
	// admission).
	memMean, memSD int64
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
		o.obs.emit(events.Event{Type: events.ProviderDown, Provider: prov, Error: err.Error(), ErrorClass: string(class), Reason: "disabled"})
		return true
	case class == provider.ClassQuota && o.providerCPULocked(prov) > 0:
		// The account allows no more than the pool runs now: cap the
		// provider there (not disabled, no cooldown); the cap lapses
		// after limitCap so a raised limit is used again.
		h.capCPU, h.capUntil = o.providerCPULocked(prov), o.now().Add(limitCap)
		o.health[prov] = h
		o.log.Warn("provider at its account limit, capped at its current vCPUs", "provider", prov, "vcpus", h.capCPU, "until", h.capUntil, "err", err)
		o.obs.emit(events.Event{Type: events.ProviderDown, Provider: prov, Error: err.Error(), ErrorClass: string(class), Reason: "capped", Seconds: limitCap.Seconds()})
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
		o.obs.emit(events.Event{Type: events.ProviderDown, Provider: prov, Error: err.Error(), ErrorClass: string(class), Reason: "cooldown", Seconds: h.cooldown.Seconds()})
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
		if l := h.limit(f.Now); l != (scheduler.Limit{}) {
			if f.ProviderLimit == nil {
				f.ProviderLimit = map[string]scheduler.Limit{}
			}
			f.ProviderLimit[name] = l
		}
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
					r.At, r.Run, seen[se.Lease] = li.start.Add(li.expected), li.run, true
					r.MemMean, r.MemSD = li.memMean, li.memSD
				} else {
					r.At, r.Unknown = r.Start.Add(scheduler.DefaultRun), true
				}
				sm.Releases = append(sm.Releases, r)
			}
		}
		for lid, pl := range m.pending {
			if seen[lid] {
				continue
			}
			if li, ok := o.leases[lid]; ok {
				sm.Releases = append(sm.Releases, scheduler.Release{At: li.start.Add(li.expected), Res: pl.res, Start: li.start, Run: li.run, MemMean: li.memMean, MemSD: li.memSD})
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
		if a.Kind == scheduler.Create && (!f.CanCreate(a.EnvHash, a.Size.Provider) || o.cfg.Policy.Disabled(a.Size.Provider)) {
			continue
		}
		if m := o.machines[a.Machine]; (a.Kind == scheduler.Start || a.Kind == scheduler.Undrain) && m != nil && o.cfg.Policy.Disabled(m.Provider) {
			continue
		}
		if strings.Contains(a.Reason, "capacity exception") {
			o.log.Warn("capacity exception: scaling out beyond the runaway guard", "action", a.Kind, "provider", a.Size.Provider, "machine", a.Machine, "reason", a.Reason)
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
	timeout := slotTimeout
	if a.Kind == scheduler.SlotPrune {
		timeout = pruneTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	o.obs.emit(ev)
}

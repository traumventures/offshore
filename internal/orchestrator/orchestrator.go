// Package orchestrator assigns sessions to machines and manages the fleet:
// cache-affine scheduling, scale up, warm pool, idle stop, lifetime
// rotation and cleanup of expired resources.
package orchestrator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/auth"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/scheduler"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// Config is the fleet configuration. Placement and scaling decisions are
// made by Policy; the orchestrator only gathers state and executes them.
type Config struct {
	Owner        string
	Sizes        []scheduler.Size // machine types; the first is the default
	MaxLifetime  time.Duration
	QueueTimeout time.Duration
	Provision    []string
	BuildTTL     time.Duration
	DepsTTL      time.Duration
	Swap         bool // machines allow sessions to swap
	Policy       scheduler.Config
}

// Request asks for capacity for one session.
type Request struct {
	EnvHash  string
	Image    string
	Services []provider.Service
	RepoID   string
	Key      string              // learning key (repo/tool/subcommand)
	Need     scheduler.Resources // estimated need
	Limit    scheduler.Resources // hard ceiling (0 = none)
}

// Lease is a reservation on a machine.
type Lease struct {
	ID          string
	MachineID   string
	Addr        string
	Fingerprint string
	// Token is a session token signed by the control plane, valid only for
	// this lease on this machine until shortly after the machine expires.
	Token string
	Grant scheduler.Grant
	// MemoryOvercommit is the fleet's memory overcommit factor; the server
	// uses it as admission safety net.
	MemoryOvercommit float64
}

// Machine is the orchestrator view of a machine.
type Machine struct {
	provider.Machine
	Provider string // name of the provider instance
	Status   *proto.Status
	StatusAt time.Time
	Draining bool
	pending  map[string]pendingLease
}

type pendingLease struct {
	res     scheduler.Resources
	expires time.Time
}

// creation is a machine being created; it is capacity on its way.
type creation struct {
	prov string
	env  string
	size scheduler.Size
	at   time.Time
}

// Orchestrator manages one owner's fleet.
type Orchestrator struct {
	cfg      Config
	provs    map[string]provider.Provider // by provider instance name
	stateDir string
	log      *slog.Logger
	master   auth.Master
	signer   ed25519.PrivateKey

	mu         sync.Mutex
	policy     scheduler.Policy
	machines   map[string]*Machine
	refreshed  time.Time
	creating   map[string]creation       // machine id -> creation
	queue      map[string]scheduler.Task // waiting tasks
	recentEnvs map[string]time.Time      // env hash -> last use (warm pool)
	images     map[string]string         // env hash -> image
	services   map[string][]provider.Service
	changed    chan struct{}

	// OnMachineDown is called when a machine stops or disappears, so that
	// connections to it can be dropped.
	OnMachineDown func(id string)

	// ProbeFunc fetches a server status; replaceable in tests.
	ProbeFunc func(ctx context.Context, addr, fingerprint, token string) (proto.Status, error)

	// EnsureImage makes the environment image available on a provider
	// before a machine is created there (nil: images are assumed present).
	EnsureImage func(ctx context.Context, prov provider.Provider, envHash string) error
}

// New creates an orchestrator.
// New creates an orchestrator over one or more named providers. One
// orchestrator may span several providers (e.g. local Docker plus AWS); the
// policy chooses among their sizes.
func New(cfg Config, provs map[string]provider.Provider, stateDir string, log *slog.Logger) (*Orchestrator, error) {
	if len(provs) == 0 {
		return nil, errors.New("orchestrator: no providers")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	master, err := auth.LoadMaster(stateDir)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: fleet master key: %w", err)
	}
	fixConfig(&cfg)
	o := &Orchestrator{
		cfg: cfg, provs: provs, stateDir: stateDir, log: log, master: master, signer: master.Signer(),
		policy:   scheduler.Default{Cfg: cfg.Policy},
		machines: map[string]*Machine{}, creating: map[string]creation{}, queue: map[string]scheduler.Task{},
		recentEnvs: map[string]time.Time{}, images: map[string]string{}, services: map[string][]provider.Service{}, changed: make(chan struct{}),
		ProbeFunc: Probe,
	}
	o.loadRecent()
	return o, nil
}

// SetConfig updates the fleet configuration and rebuilds the default policy.
func (o *Orchestrator) SetConfig(cfg Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fixConfig(&cfg)
	if _, ok := o.policy.(scheduler.Default); ok {
		o.policy = scheduler.Default{Cfg: cfg.Policy}
	}
	o.cfg = cfg
}

// SetPolicy replaces the scheduling policy.
func (o *Orchestrator) SetPolicy(p scheduler.Policy) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.policy = p
}

func fixConfig(cfg *Config) {
	if cfg.Policy.ExpiryMargin == 0 {
		cfg.Policy.ExpiryMargin = 15 * time.Minute
		if cfg.MaxLifetime > 0 && cfg.MaxLifetime < time.Hour {
			cfg.Policy.ExpiryMargin = cfg.MaxLifetime / 4
		}
	}
	if cfg.Policy.MaxMachines == 0 {
		cfg.Policy.MaxMachines = 1
	}
}

func (o *Orchestrator) notify() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// fingerprint returns the certificate fingerprint of a machine, recomputed
// from the master key so that provider labels need not be trusted. Machines
// without an expiry fall back to their label.
func (o *Orchestrator) fingerprint(m provider.Machine) string {
	if m.ExpiresAt.IsZero() {
		return m.Fingerprint
	}
	fp, err := o.master.MachineFingerprint(m.ID, m.ExpiresAt)
	if err != nil {
		return m.Fingerprint
	}
	if m.Fingerprint != "" && m.Fingerprint != fp {
		o.log.Debug("machine fingerprint label differs from derived identity", "id", m.ID)
	}
	return fp
}

// probeToken mints a short-lived token that only reads a machine's status.
func (o *Orchestrator) probeToken(machineID string) string {
	return auth.NewToken(o.signer, auth.ScopeProbe, machineID, "", time.Now().Add(2*time.Minute))
}

// sessionToken mints the token of a lease. It lives as long as the machine
// (builds may be long) but is bound to the lease.
func (o *Orchestrator) sessionToken(m provider.Machine, leaseID string) string {
	until := m.ExpiresAt.Add(time.Hour)
	if m.ExpiresAt.IsZero() {
		until = time.Now().Add(24 * time.Hour)
	}
	return auth.NewToken(o.signer, auth.ScopeSession, m.ID, leaseID, until)
}

// AdminToken mints a token with every permission on a machine (operator
// use; the orchestrator itself never needs one).
func (o *Orchestrator) AdminToken(machineID string, ttl time.Duration) string {
	return auth.NewToken(o.signer, auth.ScopeAdmin, machineID, "", time.Now().Add(ttl))
}

func (o *Orchestrator) recentPath() string { return filepath.Join(o.stateDir, "recent-envs.json") }

type recentEnv struct {
	Image    string             `json:"image"`
	Used     int64              `json:"used"`
	Services []provider.Service `json:"services,omitempty"`
}

func (o *Orchestrator) loadRecent() {
	b, err := os.ReadFile(o.recentPath())
	if err != nil {
		return
	}
	var m map[string]recentEnv
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for h, r := range m {
		o.recentEnvs[h] = time.Unix(r.Used, 0)
		o.images[h] = r.Image
		o.services[h] = r.Services
	}
}

func (o *Orchestrator) saveRecentLocked() {
	m := map[string]recentEnv{}
	for h, t := range o.recentEnvs {
		m[h] = recentEnv{Image: o.images[h], Used: t.Unix(), Services: o.services[h]}
	}
	b, _ := json.Marshal(m)
	os.WriteFile(o.recentPath(), b, 0o600)
}

func newID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Refresh reloads machines from the provider and probes running servers.
func (o *Orchestrator) Refresh(ctx context.Context) error {
	list, err := o.listAll(ctx)
	if err != nil {
		return err
	}
	type probe struct {
		id     string
		status *proto.Status
	}
	results := make(chan probe, len(list))
	var wg sync.WaitGroup
	o.mu.Lock()
	seen := map[string]bool{}
	var down []string
	for _, tm := range list {
		pm := tm.Machine
		seen[pm.ID] = true
		m, ok := o.machines[pm.ID]
		if !ok {
			m = &Machine{Provider: tm.prov, pending: map[string]pendingLease{}}
			o.machines[pm.ID] = m
		}
		if m.State == provider.Running && pm.State != provider.Running {
			down = append(down, pm.ID)
		}
		pm.Fingerprint = o.fingerprint(pm)
		m.Machine = pm
		if pm.State != provider.Running {
			m.Status = nil
		}
		if pm.State == provider.Running && pm.Addr != "" {
			wg.Add(1)
			go func(id, addr, fp, token string) {
				defer wg.Done()
				st, err := o.ProbeFunc(ctx, addr, fp, token)
				if err != nil {
					results <- probe{id, nil}
					return
				}
				results <- probe{id, &st}
			}(pm.ID, pm.Addr, pm.Fingerprint, o.probeToken(pm.ID))
		}
	}
	for id := range o.machines {
		if !seen[id] && o.machines[id].State != provider.Starting {
			delete(o.machines, id)
			down = append(down, id)
		}
	}
	cb := o.OnMachineDown
	o.mu.Unlock()
	if cb != nil {
		for _, id := range down {
			cb(id)
		}
	}
	wg.Wait()
	close(results)
	o.mu.Lock()
	for r := range results {
		if m, ok := o.machines[r.id]; ok {
			m.Status = r.status
			m.StatusAt = time.Now()
		}
	}
	o.refreshed = time.Now()
	o.notify()
	o.mu.Unlock()
	return nil
}

type taggedMachine struct {
	provider.Machine
	prov string
}

// listAll lists machines of every provider. A failing provider fails the
// whole listing, so that its machines are never mistaken for gone.
func (o *Orchestrator) listAll(ctx context.Context) ([]taggedMachine, error) {
	var out []taggedMachine
	for name, p := range o.provs {
		ms, err := p.List(ctx, o.cfg.Owner)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		for _, m := range ms {
			out = append(out, taggedMachine{m, name})
		}
	}
	return out, nil
}

// provOf returns the provider of a known machine.
func (o *Orchestrator) provOf(id string) (provider.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	name := ""
	if m, ok := o.machines[id]; ok {
		name = m.Provider
	} else if c, ok := o.creating[id]; ok {
		name = c.prov
	}
	p, ok := o.provs[name]
	if !ok {
		return nil, fmt.Errorf("machine %s: unknown provider %q", id, name)
	}
	return p, nil
}

// Probe fetches a server's status.
func Probe(ctx context.Context, addr, fingerprint, token string) (proto.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, addr, fingerprint)
	if err != nil {
		return proto.Status{}, err
	}
	defer conn.CloseWithError(0, "")
	_, pc, err := transport.OpenStream(ctx, conn, token, proto.KindStatus)
	if err != nil {
		return proto.Status{}, err
	}
	var st proto.Status
	err = pc.Expect(proto.TStatus, &st)
	return st, err
}

// fleetLocked builds the scheduler's view of the fleet.
func (o *Orchestrator) fleetLocked(now time.Time) scheduler.Fleet {
	f := scheduler.Fleet{Now: now, Sizes: o.cfg.Sizes, Recent: map[string]time.Time{}}
	for env, t := range o.recentEnvs {
		f.Recent[env] = t
	}
	for id, m := range o.machines {
		sm := scheduler.Machine{
			ID: id, Provider: m.Provider, EnvHash: m.EnvHash, Size: m.Labels[provider.LabelSize], Draining: m.Draining,
			Capacity:  scheduler.Resources{CPU: m.CPU, Memory: m.Memory},
			CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt,
		}
		switch {
		case m.State == provider.Running && m.Status != nil && m.Status.Ready && m.Addr != "":
			sm.State = scheduler.Running
		case m.State == provider.Running || m.State == provider.Starting:
			sm.State = scheduler.Starting
		case m.State == provider.Stopped:
			sm.State = scheduler.Stopped
		default:
			sm.State = scheduler.Gone
		}
		if st := m.Status; st != nil {
			sm.Reserved = scheduler.Resources{CPU: st.UsedCPU, Memory: st.UsedMemory}
			sm.Used = st.Measured
			sm.Pressure = st.Pressure
			sm.Sessions = len(st.Sessions)
			sm.WarmRepos = st.WarmRepos
			sm.Draining = sm.Draining || st.Draining
			if st.IdleSince > 0 {
				sm.IdleSince = time.Unix(st.IdleSince, 0)
			}
		}
		for lid, p := range m.pending {
			if now.After(p.expires) {
				delete(m.pending, lid)
				continue
			}
			counted := false
			if m.Status != nil {
				for _, se := range m.Status.Sessions {
					if se.Lease == lid {
						counted = true
					}
				}
			}
			if !counted {
				sm.Reserved = sm.Reserved.Add(p.res)
				sm.IdleSince = time.Time{}
			}
		}
		f.Machines = append(f.Machines, sm)
	}
	for id, c := range o.creating {
		if _, known := o.machines[id]; known {
			continue
		}
		f.Machines = append(f.Machines, scheduler.Machine{
			ID: id, Provider: c.prov, EnvHash: c.env, Size: c.size.Name, State: scheduler.Starting,
			Capacity: c.size.Capacity, CreatedAt: c.at, ExpiresAt: c.at.Add(o.cfg.MaxLifetime),
		})
	}
	sort.Slice(f.Machines, func(i, j int) bool { return f.Machines[i].ID < f.Machines[j].ID })
	for _, t := range o.queue {
		f.Queue = append(f.Queue, t)
	}
	sort.Slice(f.Queue, func(i, j int) bool { return f.Queue[i].Since.Before(f.Queue[j].Since) })
	return f
}

// ErrQueueTimeout means no capacity became available in time.
var ErrQueueTimeout = errors.New("no machine capacity available")

// Acquire reserves capacity for a session. The policy decides placement;
// while the task waits, the policy's fleet plan is executed (start, create,
// stop, destroy, drain). log receives progress messages for the user.
func (o *Orchestrator) Acquire(ctx context.Context, req Request, log io.Writer) (Lease, error) {
	now := time.Now()
	task := scheduler.Task{ID: newID(8), EnvHash: req.EnvHash, RepoID: req.RepoID, Key: req.Key, Request: req.Need, Limit: req.Limit, Since: now}
	o.mu.Lock()
	o.recentEnvs[req.EnvHash] = now
	o.images[req.EnvHash] = req.Image
	o.services[req.EnvHash] = req.Services
	o.saveRecentLocked()
	stale := time.Since(o.refreshed) > 30*time.Second
	o.mu.Unlock()
	if stale {
		if err := o.Refresh(ctx); err != nil {
			return Lease{}, err
		}
	}
	defer func() {
		o.mu.Lock()
		delete(o.queue, task.ID)
		o.mu.Unlock()
	}()
	deadline := now.Add(o.cfg.QueueTimeout)
	announced := map[string]bool{}
	refreshed := false
	for {
		o.mu.Lock()
		f := o.fleetLocked(time.Now())
		pl := o.policy.Place(f, task)
		switch pl.Kind {
		case scheduler.Assign:
			m := o.machines[pl.Machine]
			lease := Lease{
				ID: task.ID, MachineID: m.ID, Addr: m.Addr, Fingerprint: m.Fingerprint, Token: o.sessionToken(m.Machine, task.ID),
				Grant: pl.Grant, MemoryOvercommit: o.cfg.Policy.MemoryOvercommit,
			}
			m.pending[task.ID] = pendingLease{res: pl.Grant.Reserve, expires: time.Now().Add(2 * time.Minute)}
			o.mu.Unlock()
			return lease, nil
		case scheduler.Reject:
			o.mu.Unlock()
			return Lease{}, fmt.Errorf("cannot schedule: %s (needs %.1f CPU, %d bytes memory)", pl.Reason, req.Need.CPU, req.Need.Memory)
		}
		if !refreshed {
			// Placement failed on cached state: retry once with fresh state.
			o.mu.Unlock()
			refreshed = true
			if err := o.Refresh(ctx); err != nil {
				return Lease{}, err
			}
			continue
		}
		o.queue[task.ID] = task
		f = o.fleetLocked(time.Now())
		acts := o.policy.Plan(f)
		o.markLocked(acts)
		ch := o.changed
		o.mu.Unlock()
		for _, a := range acts {
			msg := ""
			switch a.Kind {
			case scheduler.Start:
				msg = "starting machine " + a.Machine
			case scheduler.Create:
				msg = fmt.Sprintf("creating %s machine for environment %s", a.Size.Name, a.EnvHash[:min(12, len(a.EnvHash))])
			}
			if msg != "" && !announced[msg] {
				fmt.Fprintf(log, "cloudsourced: %s\n", msg)
				announced[msg] = true
			}
		}
		o.execute(context.WithoutCancel(ctx), acts, false)
		if len(acts) == 0 && !announced["wait"] {
			fmt.Fprintf(log, "cloudsourced: waiting for machine capacity\n")
			announced["wait"] = true
		}
		if time.Now().After(deadline) {
			return Lease{}, ErrQueueTimeout
		}
		select {
		case <-ch:
		case <-time.After(time.Second):
			if err := o.Refresh(ctx); err != nil {
				return Lease{}, err
			}
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		}
	}
}

// markLocked records the immediate state effects of actions so that
// concurrent decisions see them (capacity on its way, machines going down).
func (o *Orchestrator) markLocked(acts []scheduler.Action) {
	for i, a := range acts {
		switch a.Kind {
		case scheduler.Create:
			id := newID(6)
			acts[i].Machine = id
			o.creating[id] = creation{prov: a.Size.Provider, env: a.EnvHash, size: a.Size, at: time.Now()}
		case scheduler.Start:
			if m, ok := o.machines[a.Machine]; ok {
				m.State = provider.Starting
				m.Status = nil
				o.creating[a.Machine] = creation{prov: m.Provider, env: m.EnvHash, at: time.Now()}
			}
		case scheduler.Stop, scheduler.Destroy:
			if m, ok := o.machines[a.Machine]; ok {
				m.Draining = true
			}
		case scheduler.Drain:
			if m, ok := o.machines[a.Machine]; ok {
				m.Draining = true
			}
		case scheduler.Undrain:
			if m, ok := o.machines[a.Machine]; ok {
				m.Draining = false
			}
		}
	}
}

// execute performs actions. Create and Start always run in the background;
// Stop and Destroy run synchronously when wait is set.
func (o *Orchestrator) execute(ctx context.Context, acts []scheduler.Action, wait bool) {
	var wg sync.WaitGroup
	for _, a := range acts {
		a := a
		switch a.Kind {
		case scheduler.Create:
			o.mu.Lock()
			image := o.images[a.EnvHash]
			o.mu.Unlock()
			go o.createMachine(ctx, a.Machine, a.EnvHash, image, a.Size)
		case scheduler.Start:
			go o.startMachine(ctx, a.Machine)
		case scheduler.Stop, scheduler.Destroy:
			run := func() {
				if a.Kind == scheduler.Stop {
					o.stopMachine(ctx, a.Machine, a.Reason)
				} else {
					o.destroyMachine(ctx, a.Machine, a.Reason)
				}
			}
			if wait {
				wg.Add(1)
				go func() { defer wg.Done(); run() }()
			} else {
				go run()
			}
		}
	}
	wg.Wait()
}

func (o *Orchestrator) stopMachine(ctx context.Context, id, reason string) {
	o.log.Info("stopping machine", "id", id, "reason", reason)
	p, err := o.provOf(id)
	if err != nil {
		o.log.Error("stop", "id", id, "err", err)
		return
	}
	o.setState(id, provider.Stopped)
	if err := p.Stop(ctx, id); err != nil {
		o.log.Error("stop", "id", id, "err", err)
	}
	o.mu.Lock()
	if m, ok := o.machines[id]; ok {
		m.Draining = false
	}
	o.mu.Unlock()
}

func (o *Orchestrator) destroyMachine(ctx context.Context, id, reason string) {
	o.log.Info("destroying machine", "id", id, "reason", reason)
	p, err := o.provOf(id)
	if err != nil {
		o.log.Error("destroy", "id", id, "err", err)
		return
	}
	o.setState(id, provider.Failed)
	if err := p.Destroy(ctx, id); err != nil {
		o.log.Error("destroy", "id", id, "err", err)
		return
	}
	o.mu.Lock()
	delete(o.machines, id)
	o.mu.Unlock()
}

func (o *Orchestrator) setState(id string, st provider.State) {
	o.mu.Lock()
	if m, ok := o.machines[id]; ok {
		m.State = st
		m.Status = nil
		m.Addr = ""
	}
	cb := o.OnMachineDown
	o.mu.Unlock()
	if cb != nil {
		cb(id)
	}
}

// Unreachable reports that a leased machine could not be reached; its
// cached state is dropped so the next placement re-probes it.
func (o *Orchestrator) Unreachable(leaseID, machineID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if m, ok := o.machines[machineID]; ok {
		delete(m.pending, leaseID)
		m.Status = nil
	}
	o.refreshed = time.Time{}
	o.notify()
}

// Release drops a pending lease (the session has ended or never started).
func (o *Orchestrator) Release(leaseID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, m := range o.machines {
		delete(m.pending, leaseID)
	}
	o.notify()
}

func (o *Orchestrator) createMachine(ctx context.Context, id, envHash, image string, size scheduler.Size) {
	defer func() {
		o.mu.Lock()
		delete(o.creating, id)
		o.notify()
		o.mu.Unlock()
	}()
	if err := o.CreateMachine(ctx, id, envHash, image, size); err != nil {
		o.log.Error("create machine", "id", id, "err", err)
	}
}

// CreateMachine provisions a new machine of size and waits until it is ready.
func (o *Orchestrator) CreateMachine(ctx context.Context, id, envHash, image string, size scheduler.Size) error {
	o.mu.Lock()
	cfg := o.cfg
	services := o.services[envHash]
	o.mu.Unlock()
	// Whole seconds: the expiry travels as Unix seconds and is an input of
	// the derived certificate.
	expires := time.Unix(time.Now().Add(cfg.MaxLifetime).Unix(), 0)
	seed := o.master.MachineSeed(id)
	ident, err := auth.MachineIdentity(seed, id, expires)
	if err != nil {
		return err
	}
	provision, _ := json.Marshal(cfg.Provision)
	spec := provider.MachineSpec{
		ID: id, Owner: cfg.Owner, Image: image, EnvHash: envHash, Size: size.Name,
		CPU: size.Capacity.CPU, Memory: size.Capacity.Memory, Swap: cfg.Swap,
		ExpiresAt: expires, Finger: ident.Fingerprint, DepsTTL: cfg.DepsTTL,
		IdleStop: cfg.Policy.IdleStop, Services: services,
		Env: map[string]string{
			"CLOUDSOURCED_MACHINE_ID": id,
			"CLOUDSOURCED_ENV_HASH":   envHash,
			"CLOUDSOURCED_EXPIRES_AT": strconv.FormatInt(expires.Unix(), 10),
			"CLOUDSOURCED_CPU":        strconv.FormatFloat(size.Capacity.CPU, 'f', -1, 64),
			"CLOUDSOURCED_MEMORY":     strconv.FormatInt(size.Capacity.Memory, 10),
			"CLOUDSOURCED_PROVISION":  string(provision),
			auth.EnvSignerPub:         o.master.SignerPublic(),
			auth.EnvFingerprint:       ident.Fingerprint,
		},
		SecretEnv: map[string]string{
			auth.EnvTLSSeed: auth.EncodeKey(seed),
		},
	}
	if cfg.BuildTTL > 0 {
		spec.Env["CLOUDSOURCED_BUILD_TTL"] = cfg.BuildTTL.String()
	}
	prov, ok := o.provs[size.Provider]
	if !ok {
		return fmt.Errorf("size %s: unknown provider %q", size.Name, size.Provider)
	}
	if o.EnsureImage != nil {
		if err := o.EnsureImage(ctx, prov, envHash); err != nil {
			return err
		}
	}
	o.log.Info("creating machine", "id", id, "provider", size.Provider, "size", size.Name, "env", envHash[:min(12, len(envHash))], "image", image)
	m, err := prov.Create(ctx, spec)
	if err != nil {
		return err
	}
	m.Fingerprint = ident.Fingerprint
	o.mu.Lock()
	o.machines[id] = &Machine{Machine: m, Provider: size.Provider, pending: map[string]pendingLease{}}
	o.mu.Unlock()
	return o.waitReady(ctx, id)
}

func (o *Orchestrator) startMachine(ctx context.Context, id string) {
	defer func() {
		o.mu.Lock()
		delete(o.creating, id)
		o.notify()
		o.mu.Unlock()
	}()
	o.log.Info("starting machine", "id", id)
	p, err := o.provOf(id)
	if err != nil {
		o.log.Error("start machine", "id", id, "err", err)
		return
	}
	m, err := p.Start(ctx, id)
	if err != nil {
		o.log.Error("start machine", "id", id, "err", err)
		return
	}
	o.mu.Lock()
	if mm, ok := o.machines[id]; ok {
		mm.Machine = m
		mm.Status = nil
		mm.Draining = false
	}
	o.mu.Unlock()
	if err := o.waitReady(ctx, id); err != nil {
		o.log.Error("machine did not become ready", "id", id, "err", err)
	}
}

// waitReady polls the machine until its server reports ready.
func (o *Orchestrator) waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(10 * time.Minute)
	p, perr := o.provOf(id)
	if perr != nil {
		return perr
	}
	for time.Now().Before(deadline) {
		list, err := p.List(ctx, o.cfg.Owner)
		if err == nil {
			for _, pm := range list {
				if pm.ID != id {
					continue
				}
				if pm.State == provider.Stopped || pm.State == provider.Failed {
					return fmt.Errorf("machine %s stopped during startup", id)
				}
				pm.Fingerprint = o.fingerprint(pm)
				o.mu.Lock()
				if m := o.machines[id]; m != nil {
					m.Machine = pm
				}
				o.mu.Unlock()
				if pm.Addr == "" {
					continue
				}
				st, err := o.ProbeFunc(ctx, pm.Addr, pm.Fingerprint, o.probeToken(id))
				if err == nil {
					o.mu.Lock()
					if m := o.machines[id]; m != nil {
						m.Status, m.StatusAt = &st, time.Now()
					}
					o.notify()
					o.mu.Unlock()
					if st.Ready {
						o.log.Info("machine ready", "id", id, "addr", pm.Addr)
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("machine %s not ready after 10m", id)
}

// Snapshot returns the current machines.
func (o *Orchestrator) Snapshot() []Machine {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Machine
	for _, m := range o.machines {
		c := *m
		c.pending = nil
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Reconcile enforces hard invariants (expired and failed machines are
// destroyed, expiring busy machines drain, orphaned volumes are deleted)
// and executes the policy's fleet plan.
func (o *Orchestrator) Reconcile(ctx context.Context) error {
	if err := o.Refresh(ctx); err != nil {
		return err
	}
	now := time.Now()
	o.mu.Lock()
	var invariant []scheduler.Action
	for _, m := range o.machines {
		expired := !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
		busy := m.Status != nil && len(m.Status.Sessions) > 0
		switch {
		case m.State == provider.Failed:
			invariant = append(invariant, scheduler.Action{Kind: scheduler.Destroy, Machine: m.ID, Reason: "failed"})
		case expired && m.State == provider.Running && busy:
			m.Draining = true
		case expired && m.State != provider.Starting:
			invariant = append(invariant, scheduler.Action{Kind: scheduler.Destroy, Machine: m.ID, Reason: "expired"})
		}
	}
	for env, used := range o.recentEnvs {
		if now.Sub(used) > 24*time.Hour {
			delete(o.recentEnvs, env)
		}
	}
	o.markLocked(invariant)
	o.mu.Unlock()
	o.execute(ctx, invariant, true)

	o.mu.Lock()
	acts := o.policy.Plan(o.fleetLocked(time.Now()))
	o.markLocked(acts)
	o.mu.Unlock()
	o.execute(context.WithoutCancel(ctx), acts, true)
	return o.reapVolumes(ctx)
}

// reapVolumes deletes expired dependency caches and orphaned cache volumes
// on every provider.
func (o *Orchestrator) reapVolumes(ctx context.Context) error {
	for name, p := range o.provs {
		if err := o.reapProvider(ctx, p); err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
	}
	return nil
}

func (o *Orchestrator) reapProvider(ctx context.Context, p provider.Provider) error {
	vols, err := p.Volumes(ctx, o.cfg.Owner)
	if err != nil {
		return err
	}
	list, err := p.List(ctx, o.cfg.Owner)
	if err != nil {
		return err
	}
	alive := map[string]bool{}
	for _, m := range list {
		alive["cs-cache-"+m.ID] = true
	}
	now := time.Now()
	for _, v := range vols {
		expired := !v.ExpiresAt.IsZero() && !now.Before(v.ExpiresAt)
		orphan := v.Kind == "cache" && !alive[v.Name]
		if expired || orphan {
			if err := p.DeleteVolume(ctx, v.Name); err == nil {
				o.log.Info("deleted volume", "provider", p.Name(), "name", v.Name, "expired", expired, "orphan", orphan)
			}
		}
	}
	return nil
}

// DestroyAll removes every machine and volume of the owner on every provider.
func (o *Orchestrator) DestroyAll(ctx context.Context) error {
	for name, p := range o.provs {
		list, err := p.List(ctx, o.cfg.Owner)
		if err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
		for _, m := range list {
			if err := p.Destroy(ctx, m.ID); err != nil {
				return fmt.Errorf("provider %s: %w", name, err)
			}
		}
		vols, err := p.Volumes(ctx, o.cfg.Owner)
		if err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
		for _, v := range vols {
			p.DeleteVolume(ctx, v.Name)
		}
	}
	o.mu.Lock()
	o.machines = map[string]*Machine{}
	o.recentEnvs = map[string]time.Time{}
	o.saveRecentLocked()
	o.mu.Unlock()
	return nil
}

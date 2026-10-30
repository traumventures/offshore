// Package orchestrator assigns sessions to machines and manages the fleet:
// cache-affine scheduling, scale up, warm pool, idle stop, lifetime
// rotation and cleanup of expired resources.
package orchestrator

import (
	"context"
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
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// Config is the fleet policy.
type Config struct {
	Owner         string
	MachineCPU    float64
	MachineMemory int64
	MinMachines   int
	MaxMachines   int
	IdleStop      time.Duration
	MaxLifetime   time.Duration
	QueueTimeout  time.Duration
	CPUOvercommit float64
	Provision     []string
	BuildTTL      time.Duration
	DepsTTL       time.Duration
	// ExpiryMargin: machines expiring sooner do not receive new sessions.
	ExpiryMargin time.Duration
}

// Request asks for capacity for one session.
type Request struct {
	EnvHash string
	Image   string
	RepoID  string
	CPU     float64
	Memory  int64
}

// Lease is a reservation on a machine.
type Lease struct {
	ID          string
	MachineID   string
	Addr        string
	Fingerprint string
	Token       string
}

// Machine is the orchestrator view of a machine.
type Machine struct {
	provider.Machine
	Token    string
	Status   *proto.Status
	StatusAt time.Time
	pending  map[string]pendingLease
}

type pendingLease struct {
	cpu     float64
	mem     int64
	expires time.Time
}

type secret struct {
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
}

// Orchestrator manages one owner's fleet.
type Orchestrator struct {
	cfg      Config
	prov     provider.Provider
	stateDir string
	log      *slog.Logger

	mu         sync.Mutex
	machines   map[string]*Machine
	refreshed  time.Time
	creating   map[string]int       // env hash -> machines being created
	waiting    map[string]int       // env hash -> waiting requests
	recentEnvs map[string]time.Time // env hash -> last use (warm pool)
	images     map[string]string    // env hash -> image
	changed    chan struct{}
}

// New creates an orchestrator.
func New(cfg Config, prov provider.Provider, stateDir string, log *slog.Logger) (*Orchestrator, error) {
	if err := os.MkdirAll(filepath.Join(stateDir, "machines"), 0o700); err != nil {
		return nil, err
	}
	if cfg.ExpiryMargin == 0 {
		cfg.ExpiryMargin = 15 * time.Minute
		if cfg.MaxLifetime > 0 && cfg.MaxLifetime < time.Hour {
			cfg.ExpiryMargin = cfg.MaxLifetime / 4
		}
	}
	o := &Orchestrator{
		cfg: cfg, prov: prov, stateDir: stateDir, log: log,
		machines: map[string]*Machine{}, creating: map[string]int{}, waiting: map[string]int{},
		recentEnvs: map[string]time.Time{}, images: map[string]string{}, changed: make(chan struct{}),
	}
	o.loadRecent()
	return o, nil
}

// SetConfig updates the fleet policy.
func (o *Orchestrator) SetConfig(cfg Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cfg.ExpiryMargin == 0 {
		cfg.ExpiryMargin = o.cfg.ExpiryMargin
	}
	o.cfg = cfg
}

func (o *Orchestrator) notify() {
	close(o.changed)
	o.changed = make(chan struct{})
}

func (o *Orchestrator) secretPath(id string) string {
	return filepath.Join(o.stateDir, "machines", id+".json")
}

func (o *Orchestrator) loadSecret(id string) (secret, error) {
	var s secret
	b, err := os.ReadFile(o.secretPath(id))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func (o *Orchestrator) recentPath() string { return filepath.Join(o.stateDir, "recent-envs.json") }

type recentEnv struct {
	Image string `json:"image"`
	Used  int64  `json:"used"`
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
	}
}

func (o *Orchestrator) saveRecentLocked() {
	m := map[string]recentEnv{}
	for h, t := range o.recentEnvs {
		m[h] = recentEnv{Image: o.images[h], Used: t.Unix()}
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
	list, err := o.prov.List(ctx, o.cfg.Owner)
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
	for _, pm := range list {
		seen[pm.ID] = true
		m, ok := o.machines[pm.ID]
		if !ok {
			m = &Machine{pending: map[string]pendingLease{}}
			if s, err := o.loadSecret(pm.ID); err == nil {
				m.Token = s.Token
			}
			o.machines[pm.ID] = m
		}
		m.Machine = pm
		if pm.State != provider.Running {
			m.Status = nil
		}
		if pm.State == provider.Running && pm.Addr != "" && m.Token != "" {
			wg.Add(1)
			go func(id, addr, fp, token string) {
				defer wg.Done()
				st, err := Probe(ctx, addr, fp, token)
				if err != nil {
					results <- probe{id, nil}
					return
				}
				results <- probe{id, &st}
			}(pm.ID, pm.Addr, pm.Fingerprint, m.Token)
		}
	}
	for id := range o.machines {
		if !seen[id] && o.machines[id].State != provider.Starting {
			delete(o.machines, id)
		}
	}
	o.mu.Unlock()
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

// score implements rendezvous hashing: each repository has a stable
// preference order over machines, so its builds land on warm caches.
func score(repoID, machineID string) string {
	return manifest.HashBytes([]byte(repoID + "\x00" + machineID))
}

func (o *Orchestrator) usable(m *Machine, envHash string, now time.Time) bool {
	return m.State == provider.Running && m.Status != nil && m.Status.Ready && !m.Status.Draining &&
		m.EnvHash == envHash && m.Addr != "" && m.Token != "" &&
		(m.ExpiresAt.IsZero() || m.ExpiresAt.Sub(now) > o.cfg.ExpiryMargin)
}

func (o *Orchestrator) free(m *Machine, now time.Time) (cpu float64, mem int64) {
	cpu = m.CPU*o.cfg.CPUOvercommit - m.Status.UsedCPU
	mem = m.Memory - m.Status.UsedMemory
	for id, p := range m.pending {
		if now.After(p.expires) {
			delete(m.pending, id)
			continue
		}
		// Pending leases that already show up as sessions are not counted twice.
		counted := false
		for _, s := range m.Status.Sessions {
			if s.Lease == id {
				counted = true
			}
		}
		if !counted {
			cpu -= p.cpu
			mem -= p.mem
		}
	}
	return cpu, mem
}

// ErrQueueTimeout means no capacity became available in time.
var ErrQueueTimeout = errors.New("no machine capacity available")

// Acquire reserves capacity for a session, starting or creating machines
// as needed. log receives progress messages for the user.
func (o *Orchestrator) Acquire(ctx context.Context, req Request, log io.Writer) (Lease, error) {
	o.mu.Lock()
	o.recentEnvs[req.EnvHash] = time.Now()
	o.images[req.EnvHash] = req.Image
	o.saveRecentLocked()
	if req.Memory > o.cfg.MachineMemory {
		o.mu.Unlock()
		return Lease{}, fmt.Errorf("session needs %d bytes memory, machines have %d", req.Memory, o.cfg.MachineMemory)
	}
	o.waiting[req.EnvHash]++
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.waiting[req.EnvHash]--
		o.mu.Unlock()
	}()
	deadline := time.Now().Add(o.cfg.QueueTimeout)
	announced := false
	for {
		o.mu.Lock()
		stale := time.Since(o.refreshed) > 2*time.Second
		o.mu.Unlock()
		if stale {
			if err := o.Refresh(ctx); err != nil {
				return Lease{}, err
			}
		}
		lease, action, err := o.tryPlace(req)
		if err != nil {
			return Lease{}, err
		}
		if lease != nil {
			return *lease, nil
		}
		switch action.kind {
		case "start":
			fmt.Fprintf(log, "cloudsourced: starting machine %s\n", action.id)
			go o.startMachine(context.WithoutCancel(ctx), action.id, req.EnvHash)
		case "create":
			fmt.Fprintf(log, "cloudsourced: creating machine for environment %s\n", req.EnvHash[:12])
			go o.createMachine(context.WithoutCancel(ctx), req.EnvHash, req.Image)
		default:
			if !announced {
				fmt.Fprintf(log, "cloudsourced: waiting for machine capacity\n")
				announced = true
			}
		}
		if time.Now().After(deadline) {
			return Lease{}, ErrQueueTimeout
		}
		o.mu.Lock()
		ch := o.changed
		o.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(time.Second):
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		}
	}
}

type action struct {
	kind string // "", "start", "create"
	id   string
}

func (o *Orchestrator) tryPlace(req Request) (*Lease, action, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	var cands []*Machine
	for _, m := range o.machines {
		if o.usable(m, req.EnvHash, now) {
			cands = append(cands, m)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		return score(req.RepoID, cands[i].ID) > score(req.RepoID, cands[j].ID)
	})
	for _, m := range cands {
		cpu, mem := o.free(m, now)
		if mem >= req.Memory && cpu >= req.CPU {
			id := newID(8)
			m.pending[id] = pendingLease{cpu: req.CPU, mem: req.Memory, expires: now.Add(2 * time.Minute)}
			return &Lease{ID: id, MachineID: m.ID, Addr: m.Addr, Fingerprint: m.Fingerprint, Token: m.Token}, action{}, nil
		}
	}
	// No capacity: count machines that will become available.
	active := 0
	starting := 0
	var stopped []*Machine
	for _, m := range o.machines {
		switch {
		case m.State == provider.Running || m.State == provider.Starting:
			active++
			if m.EnvHash == req.EnvHash && (m.State == provider.Starting || m.Status == nil || !m.Status.Ready) {
				starting++
			}
		case m.State == provider.Stopped && m.EnvHash == req.EnvHash && (m.ExpiresAt.IsZero() || m.ExpiresAt.Sub(now) > o.cfg.ExpiryMargin):
			stopped = append(stopped, m)
		}
	}
	active += o.totalCreatingLocked()
	starting += o.creating[req.EnvHash]
	perMachine := 1
	if req.Memory > 0 {
		perMachine = max(1, int(o.cfg.MachineMemory/req.Memory))
	}
	if o.waiting[req.EnvHash] <= starting*perMachine {
		return nil, action{}, nil // capacity is on its way
	}
	if len(stopped) > 0 {
		sort.Slice(stopped, func(i, j int) bool {
			return score(req.RepoID, stopped[i].ID) > score(req.RepoID, stopped[j].ID)
		})
		m := stopped[0]
		m.State = provider.Starting
		return nil, action{kind: "start", id: m.ID}, nil
	}
	if active < o.cfg.MaxMachines {
		o.creating[req.EnvHash]++
		return nil, action{kind: "create"}, nil
	}
	if len(o.machines) >= o.cfg.MaxMachines {
		// Make room: destroy an idle machine of another environment.
		for _, m := range o.machines {
			if m.EnvHash != req.EnvHash && m.State == provider.Stopped {
				m.State = provider.Failed
				go o.prov.Destroy(context.Background(), m.ID)
				break
			}
		}
	}
	return nil, action{}, nil
}

func (o *Orchestrator) totalCreatingLocked() int {
	n := 0
	for _, c := range o.creating {
		n += c
	}
	return n
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

func (o *Orchestrator) createMachine(ctx context.Context, envHash, image string) {
	defer func() {
		o.mu.Lock()
		o.creating[envHash]--
		o.notify()
		o.mu.Unlock()
	}()
	if _, err := o.CreateMachine(ctx, envHash, image); err != nil {
		o.log.Error("create machine", "err", err)
	}
}

// CreateMachine provisions a new machine and waits until it is ready.
func (o *Orchestrator) CreateMachine(ctx context.Context, envHash, image string) (string, error) {
	o.mu.Lock()
	cfg := o.cfg
	o.mu.Unlock()
	id := newID(6)
	ident, err := transport.NewIdentity("cs-"+id, cfg.MaxLifetime)
	if err != nil {
		return "", err
	}
	token := newID(32)
	b, _ := json.Marshal(secret{Token: token, Fingerprint: ident.Fingerprint})
	if err := os.WriteFile(o.secretPath(id), b, 0o600); err != nil {
		return "", err
	}
	spec := provider.MachineSpec{
		ID: id, Owner: cfg.Owner, Image: image, EnvHash: envHash, CPU: cfg.MachineCPU, Memory: cfg.MachineMemory,
		ExpiresAt: time.Now().Add(cfg.MaxLifetime), Token: token, CertPEM: ident.CertPEM, KeyPEM: ident.KeyPEM,
		Finger: ident.Fingerprint, Provision: cfg.Provision, BuildTTL: cfg.BuildTTL, DepsTTL: cfg.DepsTTL,
	}
	o.log.Info("creating machine", "id", id, "env", envHash[:12], "image", image)
	m, err := o.prov.Create(ctx, spec)
	if err != nil {
		os.Remove(o.secretPath(id))
		return "", err
	}
	o.mu.Lock()
	o.machines[id] = &Machine{Machine: m, Token: token, pending: map[string]pendingLease{}}
	o.mu.Unlock()
	return id, o.waitReady(ctx, id)
}

func (o *Orchestrator) startMachine(ctx context.Context, id, envHash string) {
	o.mu.Lock()
	o.creating[envHash]++
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.creating[envHash]--
		o.notify()
		o.mu.Unlock()
	}()
	o.log.Info("starting machine", "id", id)
	m, err := o.prov.Start(ctx, id)
	if err != nil {
		o.log.Error("start machine", "id", id, "err", err)
		return
	}
	o.mu.Lock()
	if mm, ok := o.machines[id]; ok {
		mm.Machine = m
		mm.Status = nil
	}
	o.mu.Unlock()
	if err := o.waitReady(ctx, id); err != nil {
		o.log.Error("machine did not become ready", "id", id, "err", err)
	}
}

// waitReady polls the machine until its server reports ready.
func (o *Orchestrator) waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		list, err := o.prov.List(ctx, o.cfg.Owner)
		if err == nil {
			for _, pm := range list {
				if pm.ID != id {
					continue
				}
				if pm.State == provider.Stopped || pm.State == provider.Failed {
					return fmt.Errorf("machine %s stopped during startup", id)
				}
				o.mu.Lock()
				m := o.machines[id]
				if m != nil {
					m.Machine = pm
				}
				token := ""
				if m != nil {
					token = m.Token
				}
				o.mu.Unlock()
				if pm.Addr == "" || token == "" {
					continue
				}
				st, err := Probe(ctx, pm.Addr, pm.Fingerprint, token)
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
		c.Token = ""
		c.pending = nil
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Reconcile applies the fleet policy once: destroys expired or failed
// machines and orphaned volumes, stops idle machines and keeps the warm
// pool filled.
func (o *Orchestrator) Reconcile(ctx context.Context) error {
	if err := o.Refresh(ctx); err != nil {
		return err
	}
	now := time.Now()
	o.mu.Lock()
	cfg := o.cfg
	var destroy, stop []string
	running := map[string]int{}
	stoppedByEnv := map[string][]string{}
	for _, m := range o.machines {
		expired := !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
		switch m.State {
		case provider.Running:
			busy := m.Status != nil && len(m.Status.Sessions) > 0
			if expired && !busy {
				destroy = append(destroy, m.ID)
				continue
			}
			if expired {
				continue
			}
			running[m.EnvHash]++
		case provider.Stopped:
			if expired {
				destroy = append(destroy, m.ID)
			} else {
				stoppedByEnv[m.EnvHash] = append(stoppedByEnv[m.EnvHash], m.ID)
			}
		case provider.Failed:
			destroy = append(destroy, m.ID)
		}
	}
	// Idle stop, keeping min_machines per recently used environment.
	for _, m := range o.machines {
		if m.State != provider.Running || m.Status == nil || !m.Status.Ready || len(m.Status.Sessions) > 0 || len(m.pending) > 0 {
			continue
		}
		if m.Status.IdleSince == 0 || now.Sub(time.Unix(m.Status.IdleSince, 0)) < cfg.IdleStop {
			continue
		}
		if running[m.EnvHash] <= o.warmTargetLocked(m.EnvHash, now) {
			continue
		}
		running[m.EnvHash]--
		stop = append(stop, m.ID)
	}
	// Warm pool.
	type warm struct{ env, image, start string }
	var fill []warm
	for env, used := range o.recentEnvs {
		if now.Sub(used) > 24*time.Hour {
			delete(o.recentEnvs, env)
			continue
		}
		for running[env]+o.creating[env] < o.warmTargetLocked(env, now) && len(o.machines)+o.totalCreatingLocked()+len(fill) < cfg.MaxMachines+len(stoppedByEnv[env]) {
			w := warm{env: env, image: o.images[env]}
			if s := stoppedByEnv[env]; len(s) > 0 {
				w.start, stoppedByEnv[env] = s[0], s[1:]
			} else if len(o.machines)+o.totalCreatingLocked()+len(fill) >= cfg.MaxMachines {
				break
			}
			fill = append(fill, w)
			running[env]++
		}
	}
	o.mu.Unlock()

	for _, id := range destroy {
		o.log.Info("destroying machine", "id", id, "reason", "expired or failed")
		if err := o.prov.Destroy(ctx, id); err != nil {
			o.log.Error("destroy", "id", id, "err", err)
			continue
		}
		os.Remove(o.secretPath(id))
	}
	for _, id := range stop {
		o.log.Info("stopping idle machine", "id", id)
		if err := o.prov.Stop(ctx, id); err != nil {
			o.log.Error("stop", "id", id, "err", err)
		}
	}
	for _, w := range fill {
		if w.start != "" {
			go o.startMachine(context.WithoutCancel(ctx), w.start, w.env)
		} else if w.image != "" {
			o.mu.Lock()
			o.creating[w.env]++
			o.mu.Unlock()
			go o.createMachine(context.WithoutCancel(ctx), w.env, w.image)
		}
	}
	return o.reapVolumes(ctx)
}

func (o *Orchestrator) warmTargetLocked(env string, now time.Time) int {
	if used, ok := o.recentEnvs[env]; ok && now.Sub(used) < 24*time.Hour {
		return o.cfg.MinMachines
	}
	return 0
}

// reapVolumes deletes expired dependency caches and orphaned cache volumes.
func (o *Orchestrator) reapVolumes(ctx context.Context) error {
	vols, err := o.prov.Volumes(ctx, o.cfg.Owner)
	if err != nil {
		return err
	}
	list, err := o.prov.List(ctx, o.cfg.Owner)
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
			if err := o.prov.DeleteVolume(ctx, v.Name); err == nil {
				o.log.Info("deleted volume", "name", v.Name, "expired", expired, "orphan", orphan)
			}
		}
	}
	return nil
}

// DestroyAll removes every machine and volume of the owner.
func (o *Orchestrator) DestroyAll(ctx context.Context) error {
	list, err := o.prov.List(ctx, o.cfg.Owner)
	if err != nil {
		return err
	}
	for _, m := range list {
		if err := o.prov.Destroy(ctx, m.ID); err != nil {
			return err
		}
		os.Remove(o.secretPath(m.ID))
	}
	vols, err := o.prov.Volumes(ctx, o.cfg.Owner)
	if err != nil {
		return err
	}
	for _, v := range vols {
		o.prov.DeleteVolume(ctx, v.Name)
	}
	o.mu.Lock()
	o.machines = map[string]*Machine{}
	o.recentEnvs = map[string]time.Time{}
	o.saveRecentLocked()
	o.mu.Unlock()
	return nil
}

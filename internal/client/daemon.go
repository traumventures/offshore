// Package client implements the local daemon (file watching, manifests,
// machine connections, embedded orchestrator) and its RPC interface.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/envbuild"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/provider"
	_ "github.com/traumventures/cloudsourced/internal/provider/localdocker"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// StateDir returns the daemon state directory.
func StateDir() string {
	if d := os.Getenv("CLOUDSOURCED_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "cloudsourced")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "state", "cloudsourced")
}

// SocketPath returns the daemon socket path.
func SocketPath() string { return filepath.Join(StateDir(), "daemon.sock") }

// LogPath returns the daemon log path.
func LogPath() string { return filepath.Join(StateDir(), "daemon.log") }

// Daemon is the local client service.
type Daemon struct {
	log   *slog.Logger
	orch  *orchestrator.Orchestrator
	provs map[string]provider.Provider
	est   scheduler.Estimator

	mu     sync.Mutex
	roots  map[string]*root
	conns  map[string]*machineConn
	images map[string]bool // provider + "\x00" + env hash
	imgMu  map[string]*sync.Mutex
	plans  map[string]*envbuild.Plan // env hash -> plan
	stop   context.CancelFunc
}

func fleetConfig(u *config.User, r *config.Repo) orchestrator.Config {
	idle, lifetime, queue := u.Fleet.Durations()
	var provision []string
	for _, p := range u.Fleet.Provision {
		provision = append(provision, p.Run)
	}
	var sizes []scheduler.Size
	for _, sz := range u.Fleet.Sizes {
		mem, _ := config.ParseBytes(sz.Memory)
		sizes = append(sizes, scheduler.Size{Provider: sz.Provider, Name: sz.Name,
			Capacity: scheduler.Resources{CPU: sz.CPU, Memory: mem}, PricePerHour: sz.PricePerHour})
	}
	f := u.Fleet
	c := orchestrator.Config{
		Owner: f.Owner, Sizes: sizes, MaxLifetime: lifetime, QueueTimeout: queue,
		Provision: provision, Swap: f.Swap,
		BuildTTL: 24 * time.Hour, DepsTTL: 7 * 24 * time.Hour,
		Policy: scheduler.Config{
			CPUOvercommit: f.CPUOvercommit, MemoryOvercommit: f.MemoryOvercommit,
			MaxCPUPressure: f.MaxCPUPressure, MaxMemoryPressure: f.MaxMemoryPressure,
			SoftLimit: f.SoftLimit, Swap: f.Swap, LimitJobs: f.LimitJobs,
			MinMachines: f.MinMachines, MaxMachines: f.MaxMachines, IdleStop: idle,
			ProviderMax: map[string]int{},
		},
	}
	for name, opts := range u.Providers {
		switch v := opts["max_machines"].(type) {
		case int64:
			c.Policy.ProviderMax[name] = int(v)
		case float64:
			c.Policy.ProviderMax[name] = int(v)
		}
	}
	if r != nil {
		c.BuildTTL, _ = config.ParseDuration(r.Cache.Build.TTL)
		c.DepsTTL, _ = config.ParseDuration(r.Cache.Deps.TTL)
	}
	// The build cache never outlives a machine, and machines never outlive
	// max_lifetime; cap the build TTL accordingly.
	if c.BuildTTL <= 0 || c.BuildTTL > lifetime {
		c.BuildTTL = lifetime
	}
	return c
}

// NewDaemon creates the daemon from the user configuration.
func NewDaemon(log *slog.Logger) (*Daemon, error) {
	u, err := config.LoadUser()
	if err != nil {
		return nil, err
	}
	provs := map[string]provider.Provider{}
	for _, name := range u.ProviderNames() {
		opts := map[string]any{"binary": os.Getenv("CLOUDSOURCED_SERVER_BINARY")}
		for k, v := range u.Providers[name] {
			opts[k] = v
		}
		p, err := provider.Open(u.ProviderBackend(name), opts)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		provs[name] = p
	}
	orch, err := orchestrator.New(fleetConfig(u, nil), provs, StateDir(), log)
	if err != nil {
		return nil, err
	}
	var est scheduler.Estimator = scheduler.Static{}
	if u.Fleet.Estimator == "history" {
		est = scheduler.NewHistory(filepath.Join(StateDir(), "usage-history.json"))
	}
	d := &Daemon{
		log: log, orch: orch, provs: provs, est: est,
		roots: map[string]*root{}, conns: map[string]*machineConn{},
		images: map[string]bool{}, imgMu: map[string]*sync.Mutex{}, plans: map[string]*envbuild.Plan{},
	}
	orch.EnsureImage = func(ctx context.Context, p provider.Provider, envHash string) error {
		d.mu.Lock()
		plan := d.plans[envHash]
		d.mu.Unlock()
		if plan == nil {
			return fmt.Errorf("environment %s is unknown to this daemon", envHash)
		}
		return d.ensureImage(ctx, p, plan, io.Discard)
	}
	orch.OnMachineDown = d.dropConn
	return d, nil
}

// dropConn closes the connection to a machine that went down.
func (d *Daemon) dropConn(id string) {
	d.mu.Lock()
	mc, ok := d.conns[id]
	delete(d.conns, id)
	d.mu.Unlock()
	if ok {
		mc.close()
	}
}

// Serve listens on the daemon socket until ctx ends or a shutdown request.
func (d *Daemon) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	d.stop = cancel
	defer cancel()
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	// One daemon per state directory: the lock is held for the whole
	// process lifetime, so a new daemon never races a shutting down one
	// for the socket.
	lock, err := os.OpenFile(filepath.Join(StateDir(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	locked := false
	for i := 0; i < 100; i++ {
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			locked = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !locked {
		return errors.New("daemon already running")
	}
	sock := SocketPath()
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go d.reconcileLoop(ctx)
	d.log.Info("daemon started", "socket", sock, "pid", os.Getpid())
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		go d.handle(ctx, c)
	}
	d.mu.Lock()
	for _, mc := range d.conns {
		mc.close()
	}
	for _, r := range d.roots {
		r.close()
	}
	d.mu.Unlock()
	d.log.Info("daemon stopped")
	return nil
}

func (d *Daemon) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		if err := d.orch.Reconcile(ctx); err != nil && ctx.Err() == nil {
			d.log.Warn("reconcile", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Request is a daemon RPC request.
type Request struct {
	Op    string   `json:"op"` // prepare | release | sync | status | down | reconcile | shutdown | ping
	Root  string   `json:"root,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	Tool  string   `json:"tool,omitempty"`
	Args  []string `json:"args,omitempty"`
	Lease string   `json:"lease,omitempty"`
	// Usage reports what a finished session consumed (op "release").
	UsageKey string           `json:"usage_key,omitempty"`
	Usage    *scheduler.Usage `json:"usage,omitempty"`
}

// Prepared is the result of a prepare request.
type Prepared struct {
	Machine     string            `json:"machine"`
	Addr        string            `json:"addr"`
	Fingerprint string            `json:"fingerprint"`
	Token       string            `json:"token"`
	Lease       string            `json:"lease"`
	RepoID      string            `json:"repo_id"`
	EnvHash     string            `json:"env_hash"`
	Roots       map[string]uint64 `json:"roots"`
	BuildTTL    string            `json:"build_ttl"`
	Grant       scheduler.Grant   `json:"grant"`
	UsageKey    string            `json:"usage_key"`
	// MemoryOvercommit is passed to the server as admission safety net.
	MemoryOvercommit float64 `json:"memory_overcommit"`
}

// Response is one line of a daemon RPC response.
type Response struct {
	Progress string          `json:"progress,omitempty"`
	Error    string          `json:"error,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Done     bool            `json:"done,omitempty"`
}

type progressWriter struct {
	enc *json.Encoder
	mu  *sync.Mutex
}

func (p progressWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(b), p.enc.Encode(Response{Progress: string(b)})
}

func (d *Daemon) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	var req Request
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		return
	}
	enc := json.NewEncoder(c)
	var mu sync.Mutex
	progress := progressWriter{enc: enc, mu: &mu}
	// A disconnecting proxy cancels its request.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		io.Copy(io.Discard, c)
		cancel()
	}()
	result, err := d.dispatch(rctx, req, progress)
	mu.Lock()
	defer mu.Unlock()
	resp := Response{Done: true}
	if err != nil {
		resp.Error = err.Error()
	} else if result != nil {
		resp.Result, _ = json.Marshal(result)
	}
	enc.Encode(resp)
}

func (d *Daemon) dispatch(ctx context.Context, req Request, progress io.Writer) (any, error) {
	switch req.Op {
	case "ping":
		return map[string]int{"pid": os.Getpid()}, nil
	case "prepare":
		return d.prepare(ctx, req, progress)
	case "release":
		d.orch.Release(req.Lease)
		if req.Usage != nil && req.UsageKey != "" {
			d.est.Observe(req.UsageKey, *req.Usage)
		}
		return nil, nil
	case "sync":
		r := d.rootByPath(req.Root)
		if r == nil {
			return nil, nil
		}
		v, err := r.refresh()
		return map[string]uint64{"version": v}, err
	case "status":
		if err := d.orch.Refresh(ctx); err != nil {
			return nil, err
		}
		return d.orch.Snapshot(), nil
	case "reconcile":
		return nil, d.orch.Reconcile(ctx)
	case "down":
		d.mu.Lock()
		for id, mc := range d.conns {
			mc.close()
			delete(d.conns, id)
		}
		d.mu.Unlock()
		return nil, d.orch.DestroyAll(ctx)
	case "shutdown":
		d.stop()
		return nil, nil
	}
	return nil, fmt.Errorf("unknown op %q", req.Op)
}

func (d *Daemon) rootByPath(p string) *root {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.roots[p]
}

// rootFor returns the tracked root for path, creating it on first use.
func (d *Daemon) rootFor(path string, rules manifest.Rules) (*root, error) {
	d.mu.Lock()
	r, ok := d.roots[path]
	d.mu.Unlock()
	if ok {
		r.setRules(rules)
		return r, nil
	}
	r, err := newRoot(path, rules, d.log)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.roots[path]; ok {
		r.close()
		return existing, nil
	}
	d.roots[path] = r
	return r, nil
}

// RepoID identifies a repository for cache affinity and build slots.
func RepoID(owner, root string) string {
	return manifest.HashBytes([]byte(owner + "\x00" + root))[:20]
}

// ensureImages makes the environment available on every provider that
// may host it, in parallel.
func (d *Daemon) ensureImages(ctx context.Context, plan *envbuild.Plan, progress io.Writer) error {
	d.mu.Lock()
	d.plans[plan.Hash] = plan
	d.mu.Unlock()
	errs := make(chan error, len(d.provs))
	for _, p := range d.provs {
		go func(p provider.Provider) { errs <- d.ensureImage(ctx, p, plan, progress) }(p)
	}
	var first error
	for range d.provs {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (d *Daemon) ensureImage(ctx context.Context, p provider.Provider, plan *envbuild.Plan, progress io.Writer) error {
	key := p.Name() + "\x00" + plan.Hash
	d.mu.Lock()
	if d.images[key] {
		d.mu.Unlock()
		return nil
	}
	m, ok := d.imgMu[key]
	if !ok {
		m = &sync.Mutex{}
		d.imgMu[key] = m
	}
	d.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	d.mu.Lock()
	done := d.images[key]
	d.mu.Unlock()
	if done {
		return nil
	}
	spec, dir, err := plan.Context()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	logf, err := os.OpenFile(filepath.Join(StateDir(), "env-build.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "=== %s environment %s\n", time.Now().Format(time.RFC3339), plan.Tag)
	start := time.Now()
	// Announce only when a build actually produces output.
	var once sync.Once
	w := writerFunc(func(b []byte) (int, error) {
		once.Do(func() {
			fmt.Fprintf(progress, "cloudsourced: preparing environment image %s on %s (log: %s)\n", plan.Tag, p.Name(), logf.Name())
		})
		return logf.Write(b)
	})
	err = p.EnsureImage(ctx, spec, w)
	if err != nil {
		return fmt.Errorf("environment image: %w (see %s)", err, logf.Name())
	}
	d.log.Info("environment image ready", "provider", p.Name(), "tag", plan.Tag, "duration", time.Since(start).Round(time.Millisecond))
	d.mu.Lock()
	d.images[key] = true
	d.mu.Unlock()
	return nil
}

func (d *Daemon) prepare(ctx context.Context, req Request, progress io.Writer) (*Prepared, error) {
	repo, err := config.LoadRepo(req.Root)
	if err != nil {
		return nil, err
	}
	u, err := config.LoadUser()
	if err != nil {
		return nil, err
	}
	d.orch.SetConfig(fleetConfig(u, repo))
	plan, err := envbuild.Resolve(repo)
	if err != nil {
		return nil, err
	}
	if err := d.ensureImages(ctx, plan, progress); err != nil {
		return nil, err
	}
	// Track all roots and bring their manifests up to date.
	paths := append([]string{repo.Root}, repo.ExtraRootPaths()...)
	roots := map[string]*root{}
	versions := map[string]uint64{}
	for _, p := range paths {
		r, err := d.rootFor(p, rulesFor(repo, p))
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", p, err)
		}
		v, err := r.refresh()
		if err != nil {
			return nil, err
		}
		roots[p], versions[p] = r, v
	}
	repoID := RepoID(u.Fleet.Owner, repo.Root)
	key := UsageKey(repoID, req.Tool, req.Args)
	need := d.est.Estimate(key, scheduler.Resources{CPU: repo.Resources.CPU, Memory: repo.MemoryBytes()})
	if max := repo.MaxMemoryBytes(); max > 0 && need.Memory > max {
		need.Memory = max
	}
	var lease orchestrator.Lease
	for attempt := 1; ; attempt++ {
		lease, err = d.orch.Acquire(ctx, orchestrator.Request{
			EnvHash: plan.Hash, Image: plan.Tag, Services: plan.Services, RepoID: repoID,
			Key: key, Need: need, Limit: scheduler.Resources{Memory: repo.MaxMemoryBytes()},
		}, progress)
		if err != nil {
			return nil, err
		}
		err = d.attachAll(ctx, lease, repo, roots)
		if err == nil {
			break
		}
		// The machine may have stopped or moved since it was last probed.
		d.orch.Unreachable(lease.ID, lease.MachineID)
		if attempt == 3 || ctx.Err() != nil {
			return nil, err
		}
		d.log.Warn("machine unreachable, retrying placement", "machine", lease.MachineID, "err", err)
	}
	ttl := fleetConfig(u, repo).BuildTTL
	return &Prepared{
		Machine: lease.MachineID, Addr: lease.Addr, Fingerprint: lease.Fingerprint, Token: lease.Token,
		Lease: lease.ID, RepoID: repoID, EnvHash: plan.Hash, Roots: versions, BuildTTL: ttl.String(),
		Grant: lease.Grant, UsageKey: key, MemoryOvercommit: u.Fleet.MemoryOvercommit,
	}, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func (d *Daemon) attachAll(ctx context.Context, lease orchestrator.Lease, repo *config.Repo, roots map[string]*root) error {
	actx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	mc, err := d.machineConn(actx, lease.MachineID, lease.Addr, lease.Fingerprint, lease.Token)
	if err != nil {
		return err
	}
	for p, r := range roots {
		var wl []string
		if p == repo.Root {
			wl = repo.Cache.Build.Paths
		}
		if _, err := mc.attach(actx, r, wl); err != nil {
			mc.close()
			d.mu.Lock()
			if d.conns[lease.MachineID] == mc {
				delete(d.conns, lease.MachineID)
			}
			d.mu.Unlock()
			return fmt.Errorf("attach %s: %w", p, err)
		}
	}
	return nil
}

// UsageKey is the unit of resource learning: repository, tool and
// subcommand (e.g. "cargo test" and "cargo check" differ a lot).
func UsageKey(repoID, tool string, args []string) string {
	sub := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			sub = a
			break
		}
	}
	release := ""
	for _, a := range args {
		if a == "--release" || a == "-r" {
			release = "/release"
		}
	}
	return repoID + "/" + tool + "/" + sub + release
}

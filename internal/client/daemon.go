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
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/envbuild"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/provider"
	_ "github.com/traumventures/cloudsourced/internal/provider/localdocker"
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
	log  *slog.Logger
	orch *orchestrator.Orchestrator
	prov provider.Provider

	mu     sync.Mutex
	roots  map[string]*root
	conns  map[string]*machineConn
	images map[string]bool
	imgMu  map[string]*sync.Mutex
	stop   context.CancelFunc
}

func fleetConfig(u *config.User, r *config.Repo) orchestrator.Config {
	idle, lifetime, queue := u.Fleet.Durations()
	mem, _ := config.ParseBytes(u.Fleet.MachineMemory)
	var provision []string
	for _, p := range u.Fleet.Provision {
		provision = append(provision, p.Run)
	}
	c := orchestrator.Config{
		Owner: u.Fleet.Owner, MachineCPU: u.Fleet.MachineCPU, MachineMemory: mem,
		MinMachines: u.Fleet.MinMachines, MaxMachines: u.Fleet.MaxMachines,
		IdleStop: idle, MaxLifetime: lifetime, QueueTimeout: queue,
		CPUOvercommit: u.Fleet.CPUOvercommit, Provision: provision,
		BuildTTL: 24 * time.Hour, DepsTTL: 7 * 24 * time.Hour,
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
	prov, err := provider.Open(u.Fleet.Backend, map[string]any{
		"command": u.Docker.Command,
		"host":    u.Docker.Host,
		"binary":  os.Getenv("CLOUDSOURCED_SERVER_BINARY"),
	})
	if err != nil {
		return nil, err
	}
	orch, err := orchestrator.New(fleetConfig(u, nil), prov, StateDir(), log)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		log: log, orch: orch, prov: prov,
		roots: map[string]*root{}, conns: map[string]*machineConn{},
		images: map[string]bool{}, imgMu: map[string]*sync.Mutex{},
	}, nil
}

// Serve listens on the daemon socket until ctx ends or a shutdown request.
func (d *Daemon) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	d.stop = cancel
	defer cancel()
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	sock := SocketPath()
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return errors.New("daemon already running")
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
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
	Op    string `json:"op"` // prepare | release | sync | status | down | reconcile | shutdown | ping
	Root  string `json:"root,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
	Tool  string `json:"tool,omitempty"`
	Lease string `json:"lease,omitempty"`
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
		go func() {
			time.Sleep(100 * time.Millisecond)
			d.stop()
		}()
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

func (d *Daemon) ensureImage(ctx context.Context, plan *envbuild.Plan, progress io.Writer) error {
	d.mu.Lock()
	if d.images[plan.Hash] {
		d.mu.Unlock()
		return nil
	}
	m, ok := d.imgMu[plan.Hash]
	if !ok {
		m = &sync.Mutex{}
		d.imgMu[plan.Hash] = m
	}
	d.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	d.mu.Lock()
	done := d.images[plan.Hash]
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
	announce := time.AfterFunc(time.Second, func() {
		fmt.Fprintf(progress, "cloudsourced: building environment image %s (log: %s)\n", plan.Tag, logf.Name())
	})
	err = d.prov.EnsureImage(ctx, spec, logf)
	announce.Stop()
	if err != nil {
		return fmt.Errorf("environment image: %w (see %s)", err, logf.Name())
	}
	d.log.Info("environment image ready", "tag", plan.Tag, "duration", time.Since(start).Round(time.Millisecond))
	d.mu.Lock()
	d.images[plan.Hash] = true
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
	if err := d.ensureImage(ctx, plan, progress); err != nil {
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
	lease, err := d.orch.Acquire(ctx, orchestrator.Request{
		EnvHash: plan.Hash, Image: plan.Tag, RepoID: repoID,
		CPU: repo.Resources.CPU, Memory: repo.MemoryBytes(),
	}, progress)
	if err != nil {
		return nil, err
	}
	mc, err := d.machineConn(ctx, lease.MachineID, lease.Addr, lease.Fingerprint, lease.Token)
	if err != nil {
		d.orch.Release(lease.ID)
		return nil, err
	}
	for p, r := range roots {
		var wl []string
		if p == repo.Root {
			wl = repo.Cache.Build.Paths
		}
		if _, err := mc.attach(ctx, r, wl); err != nil {
			d.orch.Release(lease.ID)
			return nil, fmt.Errorf("attach %s: %w", p, err)
		}
	}
	ttl := fleetConfig(u, repo).BuildTTL
	return &Prepared{
		Machine: lease.MachineID, Addr: lease.Addr, Fingerprint: lease.Fingerprint, Token: lease.Token,
		Lease: lease.ID, RepoID: repoID, EnvHash: plan.Hash, Roots: versions, BuildTTL: ttl.String(),
	}, nil
}

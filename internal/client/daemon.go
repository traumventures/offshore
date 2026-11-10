// Package client implements the local daemon (file watching, manifests,
// machine connections, local or remote fleet) and its RPC interface.
package client

import (
	"bufio"
	"context"
	"crypto/tls"
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
	"github.com/traumventures/cloudsourced/internal/fleet"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/runlog"
	"github.com/traumventures/cloudsourced/internal/scheduler"
	"github.com/traumventures/cloudsourced/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
	fleet fleet.Fleet
	local *fleet.Local // nil when the control plane is remote
	// reload applies fleet configuration changes to local (embedded mode).
	reload *fleet.Reloader

	outputs *outputsMgr

	mu    sync.Mutex
	roots map[string]*root
	conns map[string]*machineConn
	stop  context.CancelFunc

	metrics *daemonMetrics
}

// NewDaemon creates the daemon from the client configuration and, in
// embedded mode, the fleet configuration.
func NewDaemon(log *slog.Logger) (*Daemon, error) {
	c, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	for _, w := range c.Warnings {
		log.Warn("client configuration", "warning", w)
	}
	d := &Daemon{log: log, roots: map[string]*root{}, conns: map[string]*machineConn{}}
	d.metrics = newDaemonMetrics(d)
	if d.outputs, err = newOutputsMgr(d); err != nil {
		return nil, err
	}
	if o := c.Orchestrator; o.Mode == "remote" {
		key, err := config.ResolveRef(o.APIKey, c.Dir())
		if err != nil {
			return nil, fmt.Errorf("orchestrator.api_key: %w", err)
		}
		var certs []tls.Certificate
		if o.ClientCert != "" {
			abs := func(p string) string {
				if filepath.IsAbs(p) {
					return p
				}
				return filepath.Join(c.Dir(), p)
			}
			cert, err := tls.LoadX509KeyPair(abs(o.ClientCert), abs(o.ClientKey))
			if err != nil {
				return nil, fmt.Errorf("orchestrator.client_cert: %w", err)
			}
			certs = append(certs, cert)
		}
		r, err := fleet.NewRemote(o.URL, o.Fingerprint, key, certs...)
		if err != nil {
			return nil, err
		}
		d.fleet = r
		log.Info("using remote control plane", "url", o.URL)
		return d, nil
	}
	path := config.FleetConfigPath()
	fc, err := config.LoadFleet(path)
	if err != nil {
		return nil, err
	}
	for _, w := range fc.Warnings {
		log.Warn("fleet configuration", "warning", w)
	}
	l, err := fleet.NewLocal(fc, StateDir(), log)
	if err != nil {
		return nil, err
	}
	l.OnMachineDown(d.dropConn)
	d.fleet, d.local = l, l
	d.reload = fleet.NewReloader(path, config.LoadFleet, l, log)
	log.Info("embedded orchestrator", "fleet", fc.Fleet.ID, "config", path)
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
	if d.local != nil {
		go d.local.Run(ctx, fleet.ReconcileInterval(15*time.Second))
	}
	d.log.Info("daemon started", "socket", sock, "pid", os.Getpid())
	// Before serving requests, so no proxy writes eager outputs below a
	// mount point that is about to be mounted.
	d.outputs.remountSaved()
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
	d.outputs.unmountAll()
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

// Request is a daemon RPC request.
type Request struct {
	Op    string   `json:"op"` // prepare | release | sync | status | metrics | down | reconcile | shutdown | ping | outputs-mount | outputs-unmount | outputs-status | outputs-hint
	Root  string   `json:"root,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	Tool  string   `json:"tool,omitempty"`
	Args  []string `json:"args,omitempty"`
	Lease string   `json:"lease,omitempty"`
	// Usage reports what a finished session consumed (op "release").
	UsageKey string           `json:"usage_key,omitempty"`
	Usage    *scheduler.Usage `json:"usage,omitempty"`
	// Files are output entries already stored in the outputs content
	// cache (op "outputs-hint").
	Files []manifest.Entry `json:"files,omitempty"`
	// Run is recorded in the run log (op "release" and "record").
	Run *runlog.Run `json:"run,omitempty"`
	// TraceParent continues the caller's trace (W3C traceparent).
	TraceParent string `json:"traceparent,omitempty"`
	// Since and Until bound the events of op "events" (zero: open).
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`
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
	// Placement and preparation details for the run log.
	Provider     string         `json:"provider,omitempty"`
	Size         string         `json:"size,omitempty"`
	PricePerHour float64        `json:"price_per_hour,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	Timings      runlog.Timings `json:"timings"`
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
	rctx, cancel := context.WithCancel(telemetry.Extract(ctx, req.TraceParent))
	defer cancel()
	go func() {
		io.Copy(io.Discard, c)
		cancel()
	}()
	start := time.Now()
	result, err := d.dispatch(rctx, req, progress)
	d.metrics.request(req.Op, start, err)
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
		// A finished remote run replaced the build slot's content.
		d.outputs.released(req.Lease, req.Usage != nil)
		d.record(req.Run)
		if req.Lease == "" {
			return nil, nil
		}
		ctx, span := telemetry.Start(ctx, "daemon.release", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("cloudsourced.lease", req.Lease)))
		err := d.fleet.Release(ctx, req.Lease, req.UsageKey, req.Usage)
		telemetry.End(span, err)
		return nil, err
	case "events":
		src, ok := d.fleet.(fleet.EventSource)
		if !ok {
			return nil, errors.New("the fleet keeps no event log")
		}
		return src.Events(ctx, req.Since, req.Until)
	case "record":
		d.record(req.Run)
		return nil, nil
	case "outputs-mount":
		repo, err := config.LoadRepo(req.Root)
		if err != nil {
			return nil, err
		}
		return d.outputs.mount(repo.Root, repo)
	case "outputs-unmount":
		return nil, d.outputs.unmount(req.Root, false)
	case "outputs-status":
		return d.outputs.status(), nil
	case "outputs-hint":
		d.outputs.hint(req.Root, req.Files)
		return nil, nil
	case "sync":
		r := d.rootByPath(req.Root)
		if r == nil {
			return nil, nil
		}
		v, err := r.refresh()
		return map[string]uint64{"version": v}, err
	case "status":
		return d.fleet.Status(ctx)
	case "reconcile":
		if d.reload != nil {
			// Explicit reload of the fleet configuration (embedded mode).
			if err := d.reload.Reload(); err != nil {
				return nil, fmt.Errorf("fleet configuration not reloaded: %w", err)
			}
		}
		return nil, d.fleet.Reconcile(ctx)
	case "down":
		d.mu.Lock()
		for id, mc := range d.conns {
			mc.close()
			delete(d.conns, id)
		}
		d.mu.Unlock()
		return nil, d.fleet.DestroyAll(ctx)
	case "shutdown":
		d.stop()
		return nil, nil
	case "metrics":
		return d.metricsText(ctx)
	}
	return nil, fmt.Errorf("unknown op %q", req.Op)
}

// record appends a run to the run log of `cloudsourced why`.
func (d *Daemon) record(r *runlog.Run) {
	if r == nil {
		return
	}
	d.metrics.run(r)
	if err := runlog.Append(StateDir(), *r); err != nil {
		d.log.Warn("run log", "err", err)
	}
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

// RepoID identifies a repository for cache affinity and build slots; user
// is the client's user identity (config "user").
func RepoID(user, root string) string {
	return manifest.HashBytes([]byte(user + "\x00" + root))[:20]
}

func (d *Daemon) prepare(ctx context.Context, req Request, progress io.Writer) (_ *Prepared, err error) {
	ctx, span := telemetry.Start(ctx, "daemon.prepare", trace.WithSpanKind(trace.SpanKindServer))
	defer func() { telemetry.End(span, err) }()
	repo, err := config.LoadRepo(req.Root)
	if err != nil {
		return nil, err
	}
	c, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	if d.reload != nil {
		d.reload.Check() // the fleet configuration file changed: apply it
	}
	_, sp := telemetry.Start(ctx, "daemon.env_resolve")
	plan, err := envbuild.Resolve(repo)
	telemetry.End(sp, err)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("cloudsourced.env_hash", plan.Hash))
	var tm runlog.Timings
	mark := time.Now()
	lap := func() int64 {
		now := time.Now()
		ms := runlog.Ms(now.Sub(mark))
		mark = now
		return ms
	}
	env := fleet.Env{Hash: plan.Hash, Tag: plan.Tag, Context: plan.Context}
	ictx, sp := telemetry.Start(ctx, "daemon.image_ensure", trace.WithAttributes(attribute.String("cloudsourced.image", plan.Tag)))
	err = d.fleet.EnsureEnv(ictx, env, progress)
	telemetry.End(sp, err)
	if err != nil {
		return nil, err
	}
	tm.Env = lap()
	// Track all roots and bring their manifests up to date.
	paths := append([]string{repo.Root}, repo.ExtraRootPaths()...)
	roots := map[string]*root{}
	versions := map[string]uint64{}
	_, sp = telemetry.Start(ctx, "daemon.manifest_refresh", trace.WithAttributes(attribute.Int("cloudsourced.roots", len(paths))))
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
	sp.End()
	tm.Sync = lap()
	repoID := RepoID(c.User, repo.Root)
	key := UsageKey(repoID, req.Tool, req.Args)
	span.SetAttributes(attribute.String("cloudsourced.repo_id", repoID), attribute.String("cloudsourced.usage_key", key))
	ectx, sp := telemetry.Start(ctx, "daemon.estimate")
	need, err := d.fleet.Estimate(ectx, key, scheduler.Resources{CPU: repo.Resources.CPU, Memory: repo.MemoryBytes()})
	telemetry.End(sp, err)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Float64("cloudsourced.need.cpu", need.CPU), attribute.Int64("cloudsourced.need.memory", need.Memory))
	if max := repo.MaxMemoryBytes(); max > 0 && need.Memory > max {
		need.Memory = max
	}
	buildTTL, depsTTL := repo.CacheTTLs()
	var lease orchestrator.Lease
	for attempt := 1; ; attempt++ {
		lease, err = d.fleet.Acquire(ctx, orchestrator.Request{
			EnvHash: plan.Hash, Image: plan.Tag, Services: plan.Services, RepoID: repoID,
			Key: key, Need: need, Limit: scheduler.Resources{Memory: repo.MaxMemoryBytes()},
			BuildTTL: buildTTL, DepsTTL: depsTTL,
		}, progress)
		if err != nil {
			return nil, err
		}
		tm.Queue += lap()
		actx, sp := telemetry.Start(ctx, "daemon.attach", trace.WithAttributes(
			attribute.String("cloudsourced.machine", lease.MachineID), attribute.Int("cloudsourced.attempt", attempt)))
		err = d.attachAll(actx, lease, repo, roots)
		telemetry.End(sp, err)
		tm.Attach += lap()
		if err == nil {
			break
		}
		// The machine may have stopped or moved since it was last probed.
		d.fleet.Unreachable(ctx, lease.ID, lease.MachineID)
		if attempt == 3 || ctx.Err() != nil {
			return nil, err
		}
		d.log.Warn("machine unreachable, retrying placement", "machine", lease.MachineID, "err", err)
	}
	d.outputs.prepared(repo.Root, lease.ID, outputsState{RepoID: repoID, Machine: lease.MachineID,
		Addr: lease.Addr, Fingerprint: lease.Fingerprint, Token: lease.Token})
	if repo.Outputs.Mode == config.OutputsFuse {
		if _, err := d.outputs.mount(repo.Root, repo); err != nil {
			fmt.Fprintf(progress, "cloudsourced: outputs mount failed, using eager outputs only: %v\n", err)
		}
	}
	span.SetAttributes(attribute.String("cloudsourced.lease", lease.ID), attribute.String("cloudsourced.machine", lease.MachineID),
		attribute.String("cloudsourced.provider", lease.Provider), attribute.String("cloudsourced.size", lease.Size))
	ttl := lease.BuildTTL
	if ttl <= 0 {
		ttl = buildTTL // control plane without request-scoped TTLs; the server caps it
	}
	return &Prepared{
		Machine: lease.MachineID, Addr: lease.Addr, Fingerprint: lease.Fingerprint, Token: lease.Token,
		Lease: lease.ID, RepoID: repoID, EnvHash: plan.Hash, Roots: versions, BuildTTL: ttl.String(),
		Grant: lease.Grant, UsageKey: key, MemoryOvercommit: lease.MemoryOvercommit,
		Provider: lease.Provider, Size: lease.Size, PricePerHour: lease.PricePerHour, Reason: lease.Reason, Timings: tm,
	}, nil
}

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

package pool

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/image"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/provider"
	_ "github.com/traumventures/cloudsourced/internal/provider/awsec2"
	_ "github.com/traumventures/cloudsourced/internal/provider/localdocker"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// Local runs the orchestrator in this process over the configured providers.
type Local struct {
	log      *slog.Logger
	stateDir string
	orch     *orchestrator.Orchestrator
	provs    map[string]provider.Provider
	est      scheduler.Estimator

	cfgMu sync.Mutex
	cfg   *config.PoolConfig // applied pool configuration

	mu      sync.Mutex
	envs    map[string]Env // env hash -> source, for machines created later
	pipe    image.Pipeline
	waiters map[string]map[*syncWriter]bool // env hash -> progress of waiting acquires
	logMu   sync.Mutex                      // env-build.log
}

var _ Pool = (*Local)(nil)

// NewLocal opens every configured provider and creates the orchestrator.
func NewLocal(u *config.PoolConfig, stateDir string, log *slog.Logger) (*Local, error) {
	provs := map[string]provider.Provider{}
	for _, name := range u.ProviderNames() {
		opts := map[string]any{"binary": os.Getenv("CLOUDSOURCED_SERVER_BINARY"), "owner": u.Pool.ID}
		for k, v := range u.Providers[name] {
			opts[k] = v
		}
		p, err := provider.Open(u.ProviderType(name), opts)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		provs[name] = p
	}
	orch, err := orchestrator.New(Config(u), provs, stateDir, log)
	if err != nil {
		return nil, err
	}
	var est scheduler.Estimator = scheduler.Static{}
	if u.Pool.Estimator == "history" {
		est = scheduler.NewHistory(filepath.Join(stateDir, "usage-history.json"))
	}
	l := &Local{
		log: log, stateDir: stateDir, orch: orch, provs: provs, est: est, cfg: u,
		envs: map[string]Env{}, waiters: map[string]map[*syncWriter]bool{},
	}
	orch.PrepareImage = l.prepareImage
	return l, nil
}

// Reconfigure applies a reloaded pool configuration: sizes, policy,
// lifetimes and provisioning take effect for subsequent decisions. The pool
// id, the providers and the estimator are fixed for the life of the process;
// changing them needs a restart (reported in the returned warnings).
func (l *Local) Reconfigure(u *config.PoolConfig) (warnings []string) {
	l.cfgMu.Lock()
	old := l.cfg
	l.cfg = u
	l.cfgMu.Unlock()
	if old != nil {
		if old.Pool.ID != u.Pool.ID {
			warnings = append(warnings, fmt.Sprintf("pool.id changed from %q to %q: restart to apply", old.Pool.ID, u.Pool.ID))
		}
		if !reflect.DeepEqual(old.Providers, u.Providers) {
			warnings = append(warnings, "providers changed: restart to apply")
		}
		if old.Pool.Estimator != u.Pool.Estimator {
			warnings = append(warnings, "pool.estimator changed: restart to apply")
		}
	}
	c := Config(u)
	if old != nil {
		c.Owner = old.Pool.ID // the providers still label resources with it
	}
	l.orch.SetConfig(c)
	return warnings
}

// PoolConfig returns the applied pool configuration.
func (l *Local) PoolConfig() *config.PoolConfig {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	return l.cfg
}

// OnMachineDown registers a callback for machines that stop or disappear.
func (l *Local) OnMachineDown(fn func(id string)) { l.orch.OnMachineDown = fn }

// Register makes an environment known without building it, e.g. after a
// control plane restart. Images are built when a machine needs them.
func (l *Local) Register(env Env) {
	l.mu.Lock()
	l.envs[env.Hash] = env
	l.mu.Unlock()
}

// Known reports whether the environment source is available.
func (l *Local) Known(hash string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.envs[hash]
	return ok
}

// ReconcileIntervalEnv overrides the reconcile interval (e.g. 1s in tests).
const ReconcileIntervalEnv = "CLOUDSOURCED_RECONCILE_INTERVAL"

// ReconcileInterval returns $CLOUDSOURCED_RECONCILE_INTERVAL, else def.
func ReconcileInterval(def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(ReconcileIntervalEnv)); err == nil && d > 0 {
		return d
	}
	return def
}

// Run reconciles the pool periodically until ctx ends.
func (l *Local) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := l.orch.Reconcile(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("reconcile", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// EnsureEnv registers the environment and builds it eagerly for providers
// whose machines use the local image store (fast path: the first build is
// reported to the user right away). Remote registries receive the image
// lazily, right before a machine is created there (prepareImage).
func (l *Local) EnsureEnv(ctx context.Context, env Env, progress io.Writer) error {
	l.Register(env)
	var eager []string
	for _, name := range sortedNames(l.provs) {
		if h, ok := l.provs[name].(provider.ImageHost); ok && h.ImageRegistry().Local() {
			eager = append(eager, name)
		}
	}
	pw := &syncWriter{w: progress}
	errs := make(chan error, len(eager))
	for _, name := range eager {
		go func(name string) {
			_, err := l.ensureImage(ctx, name, "", env, pw)
			errs <- err
		}(name)
	}
	var first error
	for range eager {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func sortedNames(provs map[string]provider.Provider) []string {
	names := make([]string, 0, len(provs))
	for n := range provs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// prepareImage is the orchestrator hook run before a machine is created:
// it builds and pushes the image for that provider only. Its progress goes
// to the acquires waiting for the environment.
func (l *Local) prepareImage(ctx context.Context, provName string, _ provider.Provider, envHash, size string) (string, error) {
	l.mu.Lock()
	env, ok := l.envs[envHash]
	l.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("environment %s is unknown", envHash)
	}
	w := writerFunc(func(b []byte) (int, error) {
		l.mu.Lock()
		ws := make([]*syncWriter, 0, len(l.waiters[envHash]))
		for sw := range l.waiters[envHash] {
			ws = append(ws, sw)
		}
		l.mu.Unlock()
		for _, sw := range ws {
			sw.Write(b)
		}
		return len(b), nil
	})
	ref, err := l.ensureImage(ctx, provName, size, env, w)
	if err != nil {
		fmt.Fprintf(w, "cloudsourced: %v\n", err)
	}
	return ref, err
}

// target returns the image target of a provider (false: the provider
// manages images itself and runs the environment tag).
func (l *Local) target(name, size string) (image.Target, bool) {
	h, ok := l.provs[name].(provider.ImageHost)
	if !ok {
		return image.Target{}, false
	}
	platform := h.ImagePlatform(size)
	if a := l.sizeArch(name, size); a != "" {
		platform = image.Platform("linux/" + a)
	}
	return image.Target{Name: name, Platform: platform, Registry: h.ImageRegistry(), Builder: h.ImageBuilder()}, true
}

// sizeArch returns the configured architecture of a pool size ("": not
// configured). The environment of a machine of this size is built for it.
func (l *Local) sizeArch(prov, size string) string {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	if l.cfg == nil {
		return ""
	}
	for _, sz := range l.cfg.Pool.Sizes {
		if sz.Provider == prov && sz.Name == size {
			return config.MachineArch(sz.Arch)
		}
	}
	return ""
}

// ensureImage makes env available to machines of a provider and returns
// the reference they run. Build and push output goes to env-build.log; the
// first output is announced on progress.
func (l *Local) ensureImage(ctx context.Context, name, size string, env Env, progress io.Writer) (string, error) {
	t, ok := l.target(name, size)
	if !ok {
		return "", nil
	}
	if ref, ok := l.pipe.Ready(t, env.Hash); ok {
		return ref, nil
	}
	if err := os.MkdirAll(l.stateDir, 0o700); err != nil {
		return "", err
	}
	logPath := filepath.Join(l.stateDir, "env-build.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer logf.Close()
	start := time.Now()
	// Announce only when a build or push actually produces output.
	var once sync.Once
	w := writerFunc(func(b []byte) (int, error) {
		once.Do(func() {
			l.logMu.Lock()
			fmt.Fprintf(logf, "=== %s environment %s on %s (%s)\n", time.Now().Format(time.RFC3339), env.Tag, name, t.Platform)
			l.logMu.Unlock()
			fmt.Fprintf(progress, "cloudsourced: preparing environment image %s on %s (log: %s)\n", env.Tag, name, logPath)
		})
		l.logMu.Lock()
		defer l.logMu.Unlock()
		return logf.Write(b)
	})
	src := image.Source{Hash: env.Hash, Tag: env.Tag, Context: env.Context}
	ref, err := l.pipe.Ensure(ctx, src, t, w)
	l.imageBuilt(name, start, err)
	if err != nil {
		return "", fmt.Errorf("environment image on %s: %w (see %s)", name, err, logPath)
	}
	l.log.Info("environment image ready", "provider", name, "tag", env.Tag, "ref", ref, "duration", time.Since(start).Round(time.Millisecond))
	return ref, nil
}

// syncWriter serializes writes of concurrent producers to one writer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return len(b), nil
	}
	return s.w.Write(b)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// Acquire places a session. While it waits, image preparation for new
// machines of its environment reports on its progress writer.
func (l *Local) Acquire(ctx context.Context, req orchestrator.Request, progress io.Writer) (orchestrator.Lease, error) {
	pw := &syncWriter{w: progress}
	l.mu.Lock()
	if l.waiters[req.EnvHash] == nil {
		l.waiters[req.EnvHash] = map[*syncWriter]bool{}
	}
	l.waiters[req.EnvHash][pw] = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.waiters[req.EnvHash], pw)
		if len(l.waiters[req.EnvHash]) == 0 {
			delete(l.waiters, req.EnvHash)
		}
		l.mu.Unlock()
		pw.mu.Lock()
		pw.w = nil // the caller's writer is gone after return
		pw.mu.Unlock()
	}()
	return l.orch.Acquire(ctx, req, pw)
}

func (l *Local) Release(ctx context.Context, leaseID, usageKey string, usage *scheduler.Usage) error {
	l.orch.Release(leaseID)
	l.orch.Finish(ctx, leaseID, usageKey, usage)
	if usage != nil && usageKey != "" {
		l.est.Observe(usageKey, *usage)
	}
	return nil
}

func (l *Local) Unreachable(_ context.Context, leaseID, machineID string) error {
	l.orch.Unreachable(leaseID, machineID)
	return nil
}

func (l *Local) Estimate(_ context.Context, key string, fallback scheduler.Resources) (scheduler.Resources, error) {
	return l.est.Estimate(key, fallback), nil
}

func (l *Local) Status(ctx context.Context) ([]orchestrator.Machine, error) {
	if err := l.orch.Refresh(ctx); err != nil {
		return nil, err
	}
	return l.orch.Snapshot(), nil
}

func (l *Local) Reconcile(ctx context.Context) error { return l.orch.Reconcile(ctx) }

func (l *Local) DestroyAll(ctx context.Context) error { return l.orch.DestroyAll(ctx) }

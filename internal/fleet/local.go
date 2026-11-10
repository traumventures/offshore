package fleet

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
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
	cfg   *config.FleetConfig // applied fleet configuration

	mu     sync.Mutex
	envs   map[string]Env  // env hash -> source, for machines created later
	images map[string]bool // provider + "\x00" + env hash
	imgMu  map[string]*sync.Mutex
}

var _ Fleet = (*Local)(nil)

// NewLocal opens every configured provider and creates the orchestrator.
func NewLocal(u *config.FleetConfig, stateDir string, log *slog.Logger) (*Local, error) {
	provs := map[string]provider.Provider{}
	for _, name := range u.ProviderNames() {
		opts := map[string]any{"binary": os.Getenv("CLOUDSOURCED_SERVER_BINARY"), "owner": u.Fleet.ID}
		for k, v := range u.Providers[name] {
			opts[k] = v
		}
		p, err := provider.Open(u.ProviderBackend(name), opts)
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
	if u.Fleet.Estimator == "history" {
		est = scheduler.NewHistory(filepath.Join(stateDir, "usage-history.json"))
	}
	l := &Local{
		log: log, stateDir: stateDir, orch: orch, provs: provs, est: est, cfg: u,
		envs: map[string]Env{}, images: map[string]bool{}, imgMu: map[string]*sync.Mutex{},
	}
	orch.EnsureImage = func(ctx context.Context, p provider.Provider, envHash string) error {
		l.mu.Lock()
		env, ok := l.envs[envHash]
		l.mu.Unlock()
		if !ok {
			return fmt.Errorf("environment %s is unknown", envHash)
		}
		return l.ensureImage(ctx, p, env, io.Discard)
	}
	return l, nil
}

// Reconfigure applies a reloaded fleet configuration: sizes, policy,
// lifetimes and provisioning take effect for subsequent decisions. The fleet
// id, the providers and the estimator are fixed for the life of the process;
// changing them needs a restart (reported in the returned warnings).
func (l *Local) Reconfigure(u *config.FleetConfig) (warnings []string) {
	l.cfgMu.Lock()
	old := l.cfg
	l.cfg = u
	l.cfgMu.Unlock()
	if old != nil {
		if old.Fleet.ID != u.Fleet.ID {
			warnings = append(warnings, fmt.Sprintf("fleet.id changed from %q to %q: restart to apply", old.Fleet.ID, u.Fleet.ID))
		}
		if !reflect.DeepEqual(old.Providers, u.Providers) {
			warnings = append(warnings, "providers changed: restart to apply")
		}
		if old.Fleet.Estimator != u.Fleet.Estimator {
			warnings = append(warnings, "fleet.estimator changed: restart to apply")
		}
	}
	c := Config(u)
	if old != nil {
		c.Owner = old.Fleet.ID // the providers still label resources with it
	}
	l.orch.SetConfig(c)
	return warnings
}

// FleetConfig returns the applied fleet configuration.
func (l *Local) FleetConfig() *config.FleetConfig {
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

// Run reconciles the fleet periodically until ctx ends.
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

// EnsureEnv builds the environment on all providers in parallel.
func (l *Local) EnsureEnv(ctx context.Context, env Env, progress io.Writer) error {
	l.Register(env)
	errs := make(chan error, len(l.provs))
	for _, p := range l.provs {
		go func(p provider.Provider) { errs <- l.ensureImage(ctx, p, env, progress) }(p)
	}
	var first error
	for range l.provs {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (l *Local) ensureImage(ctx context.Context, p provider.Provider, env Env, progress io.Writer) error {
	key := p.Name() + "\x00" + env.Hash
	l.mu.Lock()
	if l.images[key] {
		l.mu.Unlock()
		return nil
	}
	m, ok := l.imgMu[key]
	if !ok {
		m = &sync.Mutex{}
		l.imgMu[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	l.mu.Lock()
	done := l.images[key]
	l.mu.Unlock()
	if done {
		return nil
	}
	spec, dir, err := env.Context()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(l.stateDir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(l.stateDir, "env-build.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "=== %s environment %s on %s\n", time.Now().Format(time.RFC3339), env.Tag, p.Name())
	start := time.Now()
	// Announce only when a build actually produces output.
	var once sync.Once
	w := writerFunc(func(b []byte) (int, error) {
		once.Do(func() {
			fmt.Fprintf(progress, "cloudsourced: preparing environment image %s on %s (log: %s)\n", env.Tag, p.Name(), logf.Name())
		})
		return logf.Write(b)
	})
	err = p.EnsureImage(ctx, spec, w)
	l.imageBuilt(p, start, err)
	if err != nil {
		return fmt.Errorf("environment image on %s: %w (see %s)", p.Name(), err, logf.Name())
	}
	l.log.Info("environment image ready", "provider", p.Name(), "tag", env.Tag, "duration", time.Since(start).Round(time.Millisecond))
	l.mu.Lock()
	l.images[key] = true
	l.mu.Unlock()
	return nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func (l *Local) Acquire(ctx context.Context, req orchestrator.Request, progress io.Writer) (orchestrator.Lease, error) {
	return l.orch.Acquire(ctx, req, progress)
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

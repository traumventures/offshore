// Package supervisor is "offshore supervisor", the supervisor on VM machines. It runs
// as root (systemd), reads the machine's public spec written by the
// provider's boot script, fetches the secret server environment once from
// the provider's secret channel, creates the machine's containers
// (machine.Layout) and starts them on every boot. It terminates the
// machine at expiry and stops it when it has been idle and unobserved by
// any orchestrator for too long (machine.Decide), the fallback for an
// offline control plane.
package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/traumventures/offshore/internal/clock"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/machine"
)

// Default locations on the host.
const (
	SpecPath = "/etc/offshore/machine.json"
	StateDir = "/var/lib/offshore"
)

// Spec is the public machine description on the host. It contains no
// secret values.
type Spec struct {
	Provider  string            `json:"provider"`
	Layout    machine.Layout    `json:"layout"`
	ExpiresAt int64             `json:"expires_at"`                  // unix seconds (0: never)
	IdleStop  int64             `json:"idle_stop_seconds,omitempty"` // 0: never stop itself
	Params    map[string]string `json:"params,omitempty"`            // provider specific (region, secret name, ...)
}

// Expires returns the expiry time (zero: none).
func (s Spec) Expires() time.Time {
	if s.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(s.ExpiresAt, 0)
}

// IdleStopAfter returns the idle stop duration.
func (s Spec) IdleStopAfter() time.Duration { return time.Duration(s.IdleStop) * time.Second }

// LoadSpec reads a spec file.
func LoadSpec(path string) (Spec, error) {
	var s Spec
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.Layout.ID == "" || s.Layout.Image == "" {
		return s, fmt.Errorf("%s: incomplete machine layout", path)
	}
	return s, nil
}

// Platform is the provider side of the supervisor.
type Platform interface {
	// FetchSecrets reads the secret server environment from the provider's
	// secret channel; DeleteSecrets removes it there once it is persisted.
	FetchSecrets(ctx context.Context) (map[string]string, error)
	DeleteSecrets(ctx context.Context) error
	// Prepare runs before images are pulled (e.g. registry login).
	Prepare(ctx context.Context, docker Runner) error
	// StopSelf stops this machine, keeping its disk.
	StopSelf(ctx context.Context) error
}

// SelfTerminator is implemented by platforms on which powering the host
// off does not end the instance (on OCI a shutdown only stops it): at
// expiry the supervisor terminates the instance through the provider API
// instead of calling Shutdown.
type SelfTerminator interface {
	TerminateSelf(ctx context.Context) error
}

// DiskGrower is implemented by platforms whose disk the provider can grow
// while the machine runs (provider.DiskResizer): the supervisor grows the
// partition and file system to the disk every check.
type DiskGrower interface {
	GrowDisk(ctx context.Context) error
}

// PlatformFactory creates a platform for a spec.
type PlatformFactory func(ctx context.Context, spec Spec) (Platform, error)

var (
	platMu    sync.Mutex
	platforms = map[string]PlatformFactory{}
)

// RegisterPlatform registers the supervisor side of a provider.
func RegisterPlatform(provider string, f PlatformFactory) {
	platMu.Lock()
	defer platMu.Unlock()
	platforms[provider] = f
}

// OpenPlatform returns the platform of a spec's provider.
func OpenPlatform(ctx context.Context, spec Spec) (Platform, error) {
	platMu.Lock()
	f, ok := platforms[spec.Provider]
	platMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("supervisor: unsupported provider %q", spec.Provider)
	}
	return f(ctx, spec)
}

// Runner runs the Docker CLI with extra environment entries.
type Runner func(ctx context.Context, env []string, stdin []byte, args ...string) ([]byte, error)

// DockerCLI returns a Runner for the docker binary.
func DockerCLI(bin string) Runner {
	return func(ctx context.Context, env []string, stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), env...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return out.Bytes(), fmt.Errorf("docker %s: %s", args[0], msg)
		}
		return out.Bytes(), nil
	}
}

// Supervisor manages one machine on its host.
type Supervisor struct {
	Spec     Spec
	Platform Platform
	StateDir string
	Docker   Runner
	Log      *slog.Logger
	Every    time.Duration       // lifecycle check interval
	Shutdown func() error        // powers the host off (terminates the instance)
	Clock    clock.Clock         // idle and expiry decisions (nil: real time)
	Sleep    func(time.Duration) // for tests
}

func (a *Supervisor) now() time.Time {
	if a.Clock != nil {
		return a.Clock.Now()
	}
	return time.Now()
}

func (a *Supervisor) sleep(ctx context.Context, d time.Duration) {
	if a.Sleep != nil {
		a.Sleep(d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (a *Supervisor) secretPath() string { return filepath.Join(a.StateDir, "secret-env.json") }

// Secrets returns the secret environment. The provider's channel is read
// once; the values are kept root-only on the instance disk (restarts after
// stop/start) and deleted from the channel.
func (a *Supervisor) Secrets(ctx context.Context) (map[string]string, error) {
	if b, err := os.ReadFile(a.secretPath()); err == nil {
		var m map[string]string
		if err := json.Unmarshal(b, &m); err == nil {
			return m, nil
		}
	}
	var m map[string]string
	wait := time.Second
	for {
		var err error
		m, err = a.Platform.FetchSecrets(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		a.Log.Warn("fetching machine secrets", "err", err, "retry", wait)
		a.sleep(ctx, wait)
		wait = min(wait*2, time.Minute)
	}
	if err := os.MkdirAll(a.StateDir, 0o700); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(m)
	if err := writeFileSync(a.secretPath(), b, 0o600); err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		err := a.Platform.DeleteSecrets(ctx)
		if err == nil {
			break
		}
		a.Log.Warn("deleting machine secrets from the provider", "err", err)
		if i == 4 || ctx.Err() != nil {
			break // the provider deletes them on destroy at the latest
		}
		a.sleep(ctx, time.Duration(i+1)*2*time.Second)
	}
	return m, nil
}

func writeFileSync(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *Supervisor) exists(ctx context.Context, kind, name string) bool {
	_, err := a.Docker(ctx, nil, nil, kind, "inspect", name)
	return err == nil
}

// waitDocker waits for the Docker daemon.
func (a *Supervisor) waitDocker(ctx context.Context) error {
	for i := 0; ; i++ {
		_, err := a.Docker(ctx, nil, nil, "info", "--format", "{{.ServerVersion}}")
		if err == nil {
			return nil
		}
		if i >= 120 || ctx.Err() != nil {
			return fmt.Errorf("docker not available: %w", err)
		}
		a.sleep(ctx, time.Second)
	}
}

// EnsureContainers creates missing containers (pulling their images) and
// starts all of them in boot order. The secrets are only needed (and
// fetched) when the machine container is created.
func (a *Supervisor) EnsureContainers(ctx context.Context) error {
	l := a.Spec.Layout
	if l.StatusDir != "" {
		if err := os.MkdirAll(l.StatusDir, 0o755); err != nil {
			return err
		}
	}
	prepared := false
	var secrets map[string]string
	for _, c := range l.Containers(nil, machine.SecretsFromProcessEnv) {
		if a.exists(ctx, "container", c.Name) {
			continue
		}
		if !a.exists(ctx, "image", c.Image) {
			if !prepared {
				if err := a.Platform.Prepare(ctx, a.Docker); err != nil {
					return err
				}
				prepared = true
			}
			a.Log.Info("pulling image", "image", c.Image)
			if _, err := a.Docker(ctx, nil, nil, "pull", "--quiet", c.Image); err != nil {
				return err
			}
		}
		if c.Name == l.Name() {
			var err error
			if secrets, err = a.Secrets(ctx); err != nil {
				return err
			}
			for _, v := range []string{l.CacheVolume, l.DepsVolume} {
				if _, err := a.Docker(ctx, nil, nil, "volume", "create", v); err != nil {
					return err
				}
			}
		}
		a.Log.Info("creating container", "name", c.Name)
		if _, err := a.Docker(ctx, l.SecretEnv(secrets), nil, c.Create...); err != nil {
			return err
		}
	}
	for _, name := range l.StartOrder() {
		if _, err := a.Docker(ctx, nil, nil, "start", name); err != nil {
			return err
		}
	}
	a.Log.Info("machine containers started", "machine", l.ID)
	return nil
}

// status reads the server's published status (nil when unavailable).
func (a *Supervisor) status() *machine.HostStatus {
	if a.Spec.Layout.StatusDir == "" {
		return nil
	}
	st, err := machine.ReadHostStatus(filepath.Join(a.Spec.Layout.StatusDir, filepath.Base(machine.HostStatusFile)))
	if err != nil {
		return nil
	}
	return &st
}

// Check makes one lifecycle decision and acts on it.
func (a *Supervisor) Check(ctx context.Context) machine.Decision {
	d := machine.Decide(a.now(), a.Spec.Expires(), a.Spec.IdleStopAfter(), a.status())
	switch d {
	case machine.Terminate:
		if t, ok := a.Platform.(SelfTerminator); ok {
			a.Log.Info("machine lifetime over, terminating the instance")
			if err := t.TerminateSelf(ctx); err == nil {
				break
			} else {
				a.Log.Error("terminating the instance, shutting down instead", "err", err)
			}
		} else {
			a.Log.Info("machine lifetime over, shutting down (the instance terminates)")
		}
		if err := a.Shutdown(); err != nil {
			a.Log.Error("shutdown", "err", err)
		}
	case machine.IdleStop:
		a.Log.Info("machine idle and not observed by any orchestrator, stopping it",
			"idle_stop", a.Spec.IdleStopAfter(), "unobserved", machine.UnobservedWindow(a.Spec.IdleStopAfter()))
		if err := a.Platform.StopSelf(ctx); err != nil {
			a.Log.Error("stopping the machine", "err", err)
		}
	}
	return d
}

// Run brings the machine up and enforces its lifecycle until ctx ends.
func (a *Supervisor) Run(ctx context.Context) error {
	if a.Every == 0 {
		a.Every = 15 * time.Second
	}
	if a.Check(ctx) == machine.Terminate {
		<-ctx.Done()
		return nil
	}
	if err := a.waitDocker(ctx); err != nil {
		return err
	}
	for i := 0; ; i++ {
		err := a.EnsureContainers(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i >= 5 {
			return err // systemd restarts the supervisor
		}
		a.Log.Warn("starting machine containers", "err", err)
		a.sleep(ctx, time.Duration(i+1)*5*time.Second)
	}
	t := time.NewTicker(a.Every)
	defer t.Stop()
	backoff := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if a.now().Before(backoff) {
			continue
		}
		if d := a.Check(ctx); d != machine.Keep {
			backoff = a.now().Add(time.Minute) // the machine is going down
			continue
		}
		if g, ok := a.Platform.(DiskGrower); ok {
			if err := g.GrowDisk(ctx); err != nil {
				a.Log.Warn("growing the disk", "err", err)
			}
		}
	}
}

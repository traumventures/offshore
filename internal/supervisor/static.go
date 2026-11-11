package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/traumventures/offshore/internal/machine"
)

// Static hosts (provider type "static") are fixed machines with sshd,
// Docker and systemd. The orchestrator runs `<binary> supervisor static
// <op>` on them over SSH: apply installs a machine (public spec, secret
// environment for the platform's channel, the supervisor unit), the other
// operations start, stop, destroy and describe it. One machine per host.

// StaticProvider is the provider name of static hosts.
const StaticProvider = "static"

// StaticUnit is the systemd unit of the supervisor on a static host.
const StaticUnit = "offshore-supervisor.service"

// StaticPayload is the input of "apply".
type StaticPayload struct {
	Spec    Spec              `json:"spec"`
	Secrets map[string]string `json:"secrets"`
}

// StaticStatus is the output of "status".
type StaticStatus struct {
	Spec   *Spec `json:"spec,omitempty"` // nil: no machine on the host
	Active bool  `json:"active"`         // the supervisor runs
	// Running reports whether the machine container runs.
	Running bool `json:"running"`
}

// StaticHost performs the host side of the static provider.
type StaticHost struct {
	SpecPath string // default SpecPath
	StateDir string // default StateDir
	UnitDir  string // default /etc/systemd/system
	Binary   string // this binary (the unit's ExecStart)
	Docker   Runner
	// Systemctl runs systemctl.
	Systemctl func(ctx context.Context, args ...string) error
}

func (h *StaticHost) specPath() string {
	if h.SpecPath != "" {
		return h.SpecPath
	}
	return SpecPath
}

func (h *StaticHost) stateDir() string {
	if h.StateDir != "" {
		return h.StateDir
	}
	return StateDir
}

func (h *StaticHost) unitDir() string {
	if h.UnitDir != "" {
		return h.UnitDir
	}
	return "/etc/systemd/system"
}

// staticSecretsPath is the platform's secret channel on the host.
func staticSecretsPath(stateDir string) string { return filepath.Join(stateDir, "static-secrets.json") }

// Apply installs a machine, replacing an earlier one.
func (h *StaticHost) Apply(ctx context.Context, p StaticPayload) error {
	if p.Spec.Layout.ID == "" {
		return errors.New("static: spec without machine id")
	}
	if cur, err := LoadSpec(h.specPath()); err == nil && cur.Layout.ID != p.Spec.Layout.ID {
		if err := h.Destroy(ctx); err != nil {
			return fmt.Errorf("static: remove previous machine %s: %w", cur.Layout.ID, err)
		}
	}
	if err := os.MkdirAll(h.stateDir(), 0o700); err != nil {
		return err
	}
	os.Remove(filepath.Join(h.stateDir(), "secret-env.json")) // a previous machine's
	sb, _ := json.Marshal(p.Secrets)
	if err := writeFileSync(staticSecretsPath(h.stateDir()), sb, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.specPath()), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(p.Spec)
	if err := writeFileSync(h.specPath(), b, 0o644); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=offshore supervisor
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service
[Service]
ExecStart=%s supervisor
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
`, h.Binary)
	if err := os.WriteFile(filepath.Join(h.unitDir(), StaticUnit), []byte(unit), 0o644); err != nil {
		return err
	}
	if err := h.Systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return h.Systemctl(ctx, "enable", "--now", StaticUnit)
}

// Start starts the supervisor (it starts the containers).
func (h *StaticHost) Start(ctx context.Context) error {
	return h.Systemctl(ctx, "start", StaticUnit)
}

// Stop stops the supervisor and the machine's containers; volumes stay.
func (h *StaticHost) Stop(ctx context.Context) error {
	if err := h.Systemctl(ctx, "stop", StaticUnit); err != nil {
		return err
	}
	spec, err := LoadSpec(h.specPath())
	if err != nil {
		return nil
	}
	for _, c := range spec.Layout.StartOrder() {
		h.Docker(ctx, nil, nil, "stop", c)
	}
	return nil
}

// Destroy removes the machine: supervisor, containers, volumes, spec and
// secrets. The host stays. The unit is stopped last: the supervisor itself
// destroys its machine at the end of its lifetime.
func (h *StaticHost) Destroy(ctx context.Context) error {
	if spec, err := LoadSpec(h.specPath()); err == nil {
		for _, c := range spec.Layout.StartOrder() {
			h.Docker(ctx, nil, nil, "rm", "-f", c)
		}
		for _, v := range []string{spec.Layout.CacheVolume, spec.Layout.DepsVolume} {
			if v != "" {
				h.Docker(ctx, nil, nil, "volume", "rm", "-f", v)
			}
		}
	}
	os.Remove(h.specPath())
	os.Remove(staticSecretsPath(h.stateDir()))
	os.Remove(filepath.Join(h.stateDir(), "secret-env.json"))
	h.Systemctl(ctx, "disable", "--now", StaticUnit)
	return nil
}

// Status describes the host's machine.
func (h *StaticHost) Status(ctx context.Context) StaticStatus {
	var st StaticStatus
	spec, err := LoadSpec(h.specPath())
	if err != nil {
		return st
	}
	st.Spec = &spec
	st.Active = h.Systemctl(ctx, "is-active", "--quiet", StaticUnit) == nil
	out, err := h.Docker(ctx, nil, nil, "inspect", "--format", "{{.State.Running}}", machine.Name(spec.Layout.ID))
	st.Running = err == nil && strings.TrimSpace(string(out)) == "true"
	return st
}

// staticPlatform is the supervisor platform of static hosts.
type staticPlatform struct {
	stateDir string
	host     *StaticHost
}

func (p *staticPlatform) FetchSecrets(context.Context) (map[string]string, error) {
	b, err := os.ReadFile(staticSecretsPath(p.stateDir))
	if err != nil {
		return nil, err
	}
	var m map[string]string
	return m, json.Unmarshal(b, &m)
}

func (p *staticPlatform) DeleteSecrets(context.Context) error {
	err := os.Remove(staticSecretsPath(p.stateDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Prepare does nothing: hosts are logged in to the image registry by
// their administrator.
func (p *staticPlatform) Prepare(context.Context, Runner) error { return nil }

// StopSelf stops the machine's containers (the host stays up).
func (p *staticPlatform) StopSelf(ctx context.Context) error { return p.host.Stop(ctx) }

// TerminateSelf removes the machine at the end of its lifetime instead of
// powering the host off.
func (p *staticPlatform) TerminateSelf(ctx context.Context) error { return p.host.Destroy(ctx) }

// NewStaticHost returns the host side for the real system.
func NewStaticHost(binary string) *StaticHost {
	return &StaticHost{Binary: binary, Docker: DockerCLI("docker"), Systemctl: func(ctx context.Context, args ...string) error {
		_, err := DockerCLI("systemctl")(ctx, nil, nil, args...)
		return err
	}}
}

func init() {
	RegisterPlatform(StaticProvider, func(_ context.Context, spec Spec) (Platform, error) {
		self, _ := os.Executable()
		return &staticPlatform{stateDir: StateDir, host: NewStaticHost(self)}, nil
	})
}

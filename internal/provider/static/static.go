// Package static is the provider for fixed hosts: machines you already
// run (bare metal, a homelab, reserved instances) with sshd, Docker and
// systemd. The orchestrator places one machine per host and drives the
// host's supervisor over SSH (`<binary> supervisor static <op>`); the
// binary is uploaded on first use. Hosts are never created or powered
// off: "create" installs a machine, "destroy" removes it, the host stays.
// Static hosts have no native object store: snapshots and global caches
// use the pool's [storage] section.
package static

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/image"
	"github.com/traumventures/offshore/internal/machine"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/supervisor"
)

// Host is one fixed host.
type Host struct {
	Name   string
	SSH    string // user@host[:port]
	Addr   string // host:port of the worker (default: ssh host:7443)
	CPU    float64
	Memory int64
	Arch   string // amd64 (default) or arm64
}

// Transport runs a shell command on a host.
type Transport interface {
	Run(ctx context.Context, h Host, cmd string, stdin []byte) ([]byte, error)
}

// Static is the provider.
type Static struct {
	Hosts     []Host
	Transport Transport
	Binary    string // local binary uploaded to the hosts
	Registry  string // repository of environment images
	Docker    []string

	binOnce sync.Once
	binHash string
	binErr  error
	binMu   sync.Mutex
	hasBin  map[string]bool // host name -> binary present

	builderOnce sync.Once
	builder     *image.DockerBuilder
}

var (
	_ provider.Provider      = (*Static)(nil)
	_ provider.ImageHost     = (*Static)(nil)
	_ provider.SecretChannel = (*Static)(nil)
)

func init() {
	provider.Register(supervisor.StaticProvider, open)
}

func open(opts map[string]any) (provider.Provider, error) {
	s := &Static{Docker: []string{"docker"}, hasBin: map[string]bool{}}
	s.Registry = provider.String(opts, "registry", "")
	if s.Registry == "" {
		return nil, errors.New("static: registry required (repository the hosts pull environment images from)")
	}
	if v := provider.StringList(opts["docker"]); len(v) > 0 {
		s.Docker = v
	}
	s.Binary = provider.String(opts, "binary", "")
	if s.Binary == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		s.Binary = self
	}
	hosts, err := parseHosts(opts["hosts"])
	if err != nil {
		return nil, err
	}
	s.Hosts = hosts
	key := provider.String(opts, "ssh_key", "file:~/.ssh/id_ed25519")
	known := provider.String(opts, "known_hosts", "~/.ssh/known_hosts")
	t, err := newSSH(key, known)
	if err != nil {
		return nil, err
	}
	s.Transport = t
	return s, nil
}

func parseHosts(v any) ([]Host, error) {
	list, ok := v.([]any)
	if !ok {
		if m, ok := v.([]map[string]any); ok {
			for _, x := range m {
				list = append(list, x)
			}
		}
	}
	if len(list) == 0 {
		return nil, errors.New("static: hosts required")
	}
	var out []Host
	names := map[string]bool{}
	for i, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("static: hosts[%d]: want a table", i)
		}
		h := Host{Name: provider.String(m, "name", ""), SSH: provider.String(m, "ssh", ""), Addr: provider.String(m, "addr", ""), Arch: provider.String(m, "arch", "amd64")}
		if h.SSH == "" {
			return nil, fmt.Errorf("static: hosts[%d].ssh required (user@host)", i)
		}
		if h.Name == "" {
			h.Name = h.SSH
		}
		if names[h.Name] {
			return nil, fmt.Errorf("static: duplicate host %q", h.Name)
		}
		names[h.Name] = true
		if h.Addr == "" {
			host := h.SSH[strings.LastIndex(h.SSH, "@")+1:]
			if hh, _, err := net.SplitHostPort(host); err == nil {
				host = hh
			}
			h.Addr = net.JoinHostPort(host, strconv.Itoa(machine.ServerPort))
		}
		switch c := m["cpu"].(type) {
		case int64:
			h.CPU = float64(c)
		case float64:
			h.CPU = c
		}
		if mem, ok := m["memory"].(string); ok {
			b, err := config.ParseBytes(mem)
			if err != nil {
				return nil, fmt.Errorf("static: hosts[%d].memory: %w", i, err)
			}
			h.Memory = b
		}
		if h.CPU <= 0 || h.Memory <= 0 {
			return nil, fmt.Errorf("static: hosts[%d]: cpu and memory required", i)
		}
		out = append(out, h)
	}
	return out, nil
}

func (s *Static) Name() string { return supervisor.StaticProvider }

func (s *Static) Caps() provider.Caps {
	return provider.Caps{FUSE: true, StopKeepsDisk: true, PollInterval: time.Second, PollMax: 5 * time.Second}
}

// hostBinary is where a host keeps the uploaded binary.
func (s *Static) hostBinary() (string, error) {
	s.binOnce.Do(func() {
		b, err := os.ReadFile(s.Binary)
		if err != nil {
			s.binErr = err
			return
		}
		sum := sha256.Sum256(b)
		s.binHash = hex.EncodeToString(sum[:8])
	})
	return "/var/lib/offshore/bin/offshore-" + s.binHash, s.binErr
}

// ensureBinary uploads the binary to a host unless present.
func (s *Static) ensureBinary(ctx context.Context, h Host) (string, error) {
	bin, err := s.hostBinary()
	if err != nil {
		return "", err
	}
	s.binMu.Lock()
	have := s.hasBin[h.Name]
	s.binMu.Unlock()
	if have {
		return bin, nil
	}
	if _, err := s.Transport.Run(ctx, h, "test -x "+bin, nil); err != nil {
		data, err := os.ReadFile(s.Binary)
		if err != nil {
			return "", err
		}
		cmd := fmt.Sprintf("mkdir -p /var/lib/offshore/bin && cat > %[1]s.tmp && chmod 755 %[1]s.tmp && mv %[1]s.tmp %[1]s", bin)
		if out, err := s.Transport.Run(ctx, h, cmd, data); err != nil {
			return "", fmt.Errorf("static: upload binary to %s: %v: %s", h.Name, err, out)
		}
	}
	s.binMu.Lock()
	s.hasBin[h.Name] = true
	s.binMu.Unlock()
	return bin, nil
}

func (s *Static) op(ctx context.Context, h Host, op string, stdin []byte) ([]byte, error) {
	bin, err := s.ensureBinary(ctx, h)
	if err != nil {
		return nil, err
	}
	out, err := s.Transport.Run(ctx, h, bin+" supervisor static "+op, stdin)
	if err != nil {
		return out, fmt.Errorf("static: %s on %s: %v: %s", op, h.Name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type hostState struct {
	host Host
	st   supervisor.StaticStatus
	err  error
}

// states describes every host in parallel.
func (s *Static) states(ctx context.Context) []hostState {
	out := make([]hostState, len(s.Hosts))
	var wg sync.WaitGroup
	for i, h := range s.Hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].host = h
			b, err := s.op(ctx, h, "status", nil)
			if err == nil {
				err = json.Unmarshal(b, &out[i].st)
			}
			out[i].err = err
		}()
	}
	wg.Wait()
	return out
}

// hostOf returns the host running machine id.
func (s *Static) hostOf(ctx context.Context, id string) (Host, error) {
	for _, st := range s.states(ctx) {
		if st.err == nil && st.st.Spec != nil && st.st.Spec.Layout.ID == id {
			return st.host, nil
		}
	}
	return Host{}, fmt.Errorf("static: machine %s not found", id)
}

// Create implements provider.Provider: it installs the machine on a free
// host with enough capacity.
func (s *Static) Create(ctx context.Context, spec provider.MachineSpec) (provider.Machine, error) {
	var host *Host
	for _, st := range s.states(ctx) {
		if st.err != nil || st.st.Spec != nil {
			continue
		}
		if st.host.CPU >= spec.CPU && st.host.Memory >= spec.Memory && archOK(st.host.Arch, spec.Arch) {
			h := st.host
			host = &h
			break
		}
	}
	if host == nil {
		return provider.Machine{}, fmt.Errorf("static: no free host with %.0f CPU and %d bytes memory", spec.CPU, spec.Memory)
	}
	bin, err := s.ensureBinary(ctx, *host)
	if err != nil {
		return provider.Machine{}, err
	}
	now := time.Now()
	labels := map[string]string{
		provider.LabelPool: spec.Owner, provider.LabelMachine: spec.ID, provider.LabelKind: "machine",
		provider.LabelEnvHash: spec.EnvHash, provider.LabelCreatedAt: strconv.FormatInt(now.Unix(), 10),
		provider.LabelExpiresAt: strconv.FormatInt(spec.ExpiresAt.Unix(), 10), provider.LabelFinger: spec.Finger,
		provider.LabelSize: spec.Size, provider.LabelArch: host.Arch,
		provider.LabelCPU:    strconv.FormatFloat(host.CPU, 'f', -1, 64),
		provider.LabelMemory: strconv.FormatInt(host.Memory, 10), "offshore.host": host.Name,
	}
	hs := supervisor.Spec{
		Provider: supervisor.StaticProvider,
		Layout: machine.Layout{
			ID: spec.ID, Image: spec.Image, Binary: bin,
			Publish:     strconv.Itoa(machine.ServerPort) + ":" + machine.ServerPortSpec(),
			CacheVolume: "cs-cache-" + spec.ID, DepsVolume: "cs-deps",
			StatusDir: "/run/offshore/status",
			Labels:    labels, SvcLabels: map[string]string{provider.LabelKind: "service", provider.LabelMachine: spec.ID},
			Env: spec.Env, SecretKeys: provider.SortedKeys(spec.SecretEnv), Services: spec.Services,
		},
		ExpiresAt: spec.ExpiresAt.Unix(),
		IdleStop:  int64(spec.IdleStop.Seconds()),
	}
	payload, _ := json.Marshal(supervisor.StaticPayload{Spec: hs, Secrets: spec.SecretEnv})
	if _, err := s.op(ctx, *host, "apply", payload); err != nil {
		s.op(context.Background(), *host, "destroy", nil)
		return provider.Machine{}, err
	}
	return provider.Machine{ID: spec.ID, State: provider.Starting, EnvHash: spec.EnvHash, CPU: host.CPU, Memory: host.Memory,
		CreatedAt: now, ExpiresAt: spec.ExpiresAt, Fingerprint: spec.Finger, Labels: labels}, nil
}

func archOK(host, want string) bool {
	if want == "" {
		want = "amd64"
	}
	return host == want
}

// Start implements provider.Provider.
func (s *Static) Start(ctx context.Context, id string) (provider.Machine, error) {
	h, err := s.hostOf(ctx, id)
	if err != nil {
		return provider.Machine{}, err
	}
	if _, err := s.op(ctx, h, "start", nil); err != nil {
		return provider.Machine{}, err
	}
	for _, st := range s.states(ctx) {
		if st.host.Name == h.Name && st.st.Spec != nil {
			return toMachine(st), nil
		}
	}
	return provider.Machine{}, fmt.Errorf("static: machine %s vanished", id)
}

// Stop implements provider.Provider (containers stop, volumes stay).
func (s *Static) Stop(ctx context.Context, id string) error {
	h, err := s.hostOf(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.op(ctx, h, "stop", nil)
	return err
}

// Destroy implements provider.Provider (the host stays).
func (s *Static) Destroy(ctx context.Context, id string) error {
	h, err := s.hostOf(ctx, id)
	if err != nil {
		return nil // already gone
	}
	_, err = s.op(ctx, h, "destroy", nil)
	return err
}

// List implements provider.Provider.
func (s *Static) List(ctx context.Context, owner string) ([]provider.Machine, error) {
	var out []provider.Machine
	var errs []string
	for _, st := range s.states(ctx) {
		if st.err != nil {
			errs = append(errs, st.err.Error())
			continue
		}
		if st.st.Spec == nil || (owner != "" && st.st.Spec.Layout.Labels[provider.LabelPool] != owner) {
			continue
		}
		out = append(out, toMachine(st))
	}
	if len(errs) == len(s.Hosts) && len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

func toMachine(st hostState) provider.Machine {
	spec := st.st.Spec
	l := spec.Layout.Labels
	m := provider.Machine{ID: spec.Layout.ID, EnvHash: l[provider.LabelEnvHash], Fingerprint: l[provider.LabelFinger], Labels: l,
		CPU: st.host.CPU, Memory: st.host.Memory}
	if n, err := strconv.ParseInt(l[provider.LabelCreatedAt], 10, 64); err == nil {
		m.CreatedAt = time.Unix(n, 0)
	}
	m.ExpiresAt = spec.Expires()
	switch {
	case st.st.Active && st.st.Running:
		m.State, m.Addr = provider.Running, st.host.Addr
	case st.st.Active:
		m.State = provider.Starting
	default:
		m.State = provider.Stopped
	}
	return m
}

// PutSecrets implements provider.SecretChannel: secrets travel with the
// apply payload over SSH; nothing is stored elsewhere.
func (s *Static) PutSecrets(context.Context, string, map[string]string) error { return nil }

// DeleteSecrets implements provider.SecretChannel (the host's supervisor
// removes them after reading).
func (s *Static) DeleteSecrets(context.Context, string) error { return nil }

// ImagePlatform implements provider.ImageHost.
func (s *Static) ImagePlatform(string) image.Platform {
	if len(s.Hosts) > 0 && s.Hosts[0].Arch == "arm64" {
		return image.Platform("linux/arm64")
	}
	return image.LinuxAMD64
}

// ImageRegistry implements provider.ImageHost.
func (s *Static) ImageRegistry() image.Registry {
	return &image.Plain{Repository: s.Registry, CLI: s.Docker}
}

// ImageBuilder implements provider.ImageHost.
func (s *Static) ImageBuilder() image.Builder {
	s.builderOnce.Do(func() { s.builder = &image.DockerBuilder{CLI: s.Docker} })
	return s.builder
}

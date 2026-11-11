// Package localdocker runs machines as Docker containers on the local host.
// It is the reference backend for development and tests: provisioning,
// FUSE, sessions, caches and lifecycle are real; only the machine is a
// container instead of a VM.
package localdocker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/image"
	"github.com/traumventures/cloudsourced/internal/machine"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/store"
)

var serverPort = machine.ServerPortSpec()

func init() {
	provider.Register("local-docker", func(opts map[string]any) (provider.Provider, error) {
		d := &Docker{Command: []string{"docker"}, Host: "127.0.0.1"}
		if v := provider.StringList(opts["command"]); len(v) > 0 {
			d.Command = v
		}
		if v, ok := opts["host"].(string); ok && v != "" {
			d.Host = v
		}
		if v, ok := opts["binary"].(string); ok {
			d.Binary = v
		}
		if d.Binary == "" {
			self, err := os.Executable()
			if err != nil {
				return nil, err
			}
			d.Binary = self
		}
		return d, nil
	})
}

// Docker is the local-docker provider.
type Docker struct {
	Command []string // docker CLI invocation
	Host    string   // address of published ports
	Binary  string   // static cloudsourced binary mounted into machines

	builderOnce sync.Once
	builder     *image.DockerBuilder
}

var _ provider.ImageHost = (*Docker)(nil)

func (d *Docker) Name() string { return "local-docker" }

func (d *Docker) Caps() provider.Caps {
	return provider.Caps{FUSE: true, StopKeepsDisk: true}
}

func (d *Docker) run(ctx context.Context, stdout io.Writer, args ...string) error {
	argv := append(append([]string{}, d.Command[1:]...), args...)
	cmd := exec.CommandContext(ctx, d.Command[0], argv...)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	if stdout == nil {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return nil
}

func (d *Docker) output(ctx context.Context, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := d.run(ctx, &out, args...)
	return out.Bytes(), err
}

// ImagePlatform is the platform of the local Docker daemon.
func (d *Docker) ImagePlatform(string) image.Platform { return image.Native() }

// ImageRegistry is the local daemon itself: machines run local images.
func (d *Docker) ImageRegistry() image.Registry { return image.DockerDaemon{CLI: d.Command} }

// ImageBuilder builds with the provider's Docker CLI.
func (d *Docker) ImageBuilder() image.Builder {
	d.builderOnce.Do(func() { d.builder = &image.DockerBuilder{CLI: d.Command} })
	return d.builder
}

func labels(kv map[string]string) []string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, "--label", k+"="+kv[k])
	}
	return out
}

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// Create starts a new machine container.
func (d *Docker) Create(ctx context.Context, spec provider.MachineSpec) (provider.Machine, error) {
	now := time.Now()
	common := map[string]string{
		provider.LabelPool:      spec.Owner,
		provider.LabelMachine:   spec.ID,
		provider.LabelCreatedAt: unix(now),
		provider.LabelExpiresAt: unix(spec.ExpiresAt),
	}
	volLabels := map[string]string{provider.LabelKind: "cache"}
	for k, v := range common {
		volLabels[k] = v
	}
	cacheVol := "cs-cache-" + spec.ID
	if err := d.run(ctx, nil, append(append([]string{"volume", "create"}, labels(volLabels)...), cacheVol)...); err != nil {
		return provider.Machine{}, err
	}
	depsVol, err := d.depsVolume(ctx, spec)
	if err != nil {
		return provider.Machine{}, err
	}
	// The pool's directory store: one volume shared by all machines, not
	// a cache volume (no pool label, never reaped; objects expire in it).
	if err := d.run(ctx, nil, "volume", "create", "--label", provider.LabelKind+"=store", storeVolume(spec.Owner)); err != nil {
		return provider.Machine{}, err
	}
	mLabels := map[string]string{
		provider.LabelKind:    "machine",
		provider.LabelEnvHash: spec.EnvHash,
		provider.LabelCPU:     strconv.FormatFloat(spec.CPU, 'f', -1, 64),
		provider.LabelMemory:  strconv.FormatInt(spec.Memory, 10),
		provider.LabelFinger:  spec.Finger,
		provider.LabelSize:    spec.Size,
	}
	for k, v := range common {
		mLabels[k] = v
	}
	svcLabels := map[string]string{provider.LabelKind: "service"}
	for k, v := range common {
		svcLabels[k] = v
	}
	l := machine.Layout{
		ID: spec.ID, Image: spec.Image, Binary: d.Binary,
		Publish:     d.Host + "::" + serverPort,
		CacheVolume: cacheVol, DepsVolume: depsVol, StoreVolume: storeVolume(spec.Owner),
		CPU: spec.CPU, Memory: spec.Memory, Swap: spec.Swap,
		Labels: mLabels, SvcLabels: svcLabels,
		Env: spec.Env, SecretKeys: provider.SortedKeys(spec.SecretEnv), Services: spec.Services,
	}
	// The machine is a local container: the secret environment never
	// leaves this host. The CLI may run through sudo, which drops its
	// environment, so secrets are passed inline.
	for i, c := range l.Containers(spec.SecretEnv, machine.SecretsInline) {
		err := d.run(ctx, nil, c.Create...)
		if err == nil {
			err = d.run(ctx, nil, "start", c.Name)
		}
		if err != nil {
			d.Destroy(context.Background(), spec.ID)
			if i == 0 {
				d.run(context.Background(), nil, "volume", "rm", "-f", cacheVol)
				return provider.Machine{}, err
			}
			return provider.Machine{}, fmt.Errorf("service %s: %w", c.Name, err)
		}
	}
	return d.inspect(ctx, spec.ID)
}

// services lists the service containers of a machine.
func (d *Docker) services(ctx context.Context, id string) []string {
	out, err := d.output(ctx, "ps", "-aq", "--filter", "label="+provider.LabelMachine+"="+id, "--filter", "label="+provider.LabelKind+"=service")
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// depsVolume returns the newest unexpired shared dependency cache volume,
// creating one if necessary.
func (d *Docker) depsVolume(ctx context.Context, spec provider.MachineSpec) (string, error) {
	vols, err := d.Volumes(ctx, spec.Owner)
	if err != nil {
		return "", err
	}
	now := time.Now()
	best := ""
	var bestCreated time.Time
	for _, v := range vols {
		if v.Kind != "deps" || !now.Before(v.ExpiresAt) {
			continue
		}
		if v.CreatedAt.After(bestCreated) {
			best, bestCreated = v.Name, v.CreatedAt
		}
	}
	if best != "" {
		return best, nil
	}
	ttl := spec.DepsTTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	name := fmt.Sprintf("cs-deps-%s-%d", sanitize(spec.Owner), now.Unix())
	l := map[string]string{
		provider.LabelPool:      spec.Owner,
		provider.LabelKind:      "deps",
		provider.LabelCreatedAt: unix(now),
		provider.LabelExpiresAt: unix(now.Add(ttl)),
	}
	if err := d.run(ctx, nil, append(append([]string{"volume", "create"}, labels(l)...), name)...); err != nil {
		return "", err
	}
	return name, nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

type inspectResult struct {
	Name   string
	Config struct {
		Labels map[string]string
	}
	State struct {
		Status   string
		ExitCode int
	}
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string
		}
	}
}

func (d *Docker) inspect(ctx context.Context, id string) (provider.Machine, error) {
	ms, err := d.inspectNames(ctx, []string{"cs-" + id})
	if err != nil {
		return provider.Machine{}, err
	}
	if len(ms) == 0 {
		return provider.Machine{}, fmt.Errorf("machine %s not found", id)
	}
	return ms[0], nil
}

func (d *Docker) inspectNames(ctx context.Context, names []string) ([]provider.Machine, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out, err := d.output(ctx, append([]string{"inspect", "--type", "container"}, names...)...)
	if err != nil {
		return nil, err
	}
	var res []inspectResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	var ms []provider.Machine
	for _, r := range res {
		l := r.Config.Labels
		m := provider.Machine{ID: l[provider.LabelMachine], EnvHash: l[provider.LabelEnvHash], Labels: l, Fingerprint: l[provider.LabelFinger]}
		m.CPU, _ = strconv.ParseFloat(l[provider.LabelCPU], 64)
		m.Memory, _ = strconv.ParseInt(l[provider.LabelMemory], 10, 64)
		if n, err := strconv.ParseInt(l[provider.LabelCreatedAt], 10, 64); err == nil {
			m.CreatedAt = time.Unix(n, 0)
		}
		if n, err := strconv.ParseInt(l[provider.LabelExpiresAt], 10, 64); err == nil {
			m.ExpiresAt = time.Unix(n, 0)
		}
		switch r.State.Status {
		case "running":
			m.State = provider.Running
		case "created", "restarting":
			m.State = provider.Starting
		case "exited", "paused":
			m.State = provider.Stopped
		default:
			m.State = provider.Failed
		}
		if b := r.NetworkSettings.Ports[serverPort]; len(b) > 0 && m.State == provider.Running {
			host := b[0].HostIP
			if host == "" || host == "0.0.0.0" {
				host = d.Host
			}
			m.Addr = host + ":" + b[0].HostPort
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// Start restarts a stopped machine.
func (d *Docker) Start(ctx context.Context, id string) (provider.Machine, error) {
	if err := d.run(ctx, nil, "start", machine.Name(id)); err != nil {
		return provider.Machine{}, err
	}
	// Services join the machine's (new) network namespace, so they start
	// after it (the boot order of machine.Layout).
	if svcs := d.services(ctx, id); len(svcs) > 0 {
		if err := d.run(ctx, nil, append([]string{"start"}, svcs...)...); err != nil {
			return provider.Machine{}, err
		}
	}
	return d.inspect(ctx, id)
}

// Stop stops a machine, keeping its cache volume.
func (d *Docker) Stop(ctx context.Context, id string) error {
	if svcs := d.services(ctx, id); len(svcs) > 0 {
		d.run(ctx, nil, append([]string{"stop", "-t", "5"}, svcs...)...)
	}
	return d.run(ctx, nil, "stop", "-t", "10", "cs-"+id)
}

// Destroy removes the machine, its services and its cache volume.
func (d *Docker) Destroy(ctx context.Context, id string) error {
	if svcs := d.services(ctx, id); len(svcs) > 0 {
		d.run(ctx, nil, append([]string{"rm", "-f", "-v"}, svcs...)...)
	}
	err := d.run(ctx, nil, "rm", "-f", "-v", "cs-"+id)
	if err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}
	err = d.run(ctx, nil, "volume", "rm", "-f", "cs-cache-"+id)
	if err != nil && !strings.Contains(err.Error(), "no such volume") {
		return err
	}
	return nil
}

// List returns all machines of owner.
func (d *Docker) List(ctx context.Context, owner string) ([]provider.Machine, error) {
	out, err := d.output(ctx, "ps", "-a", "--filter", "label="+provider.LabelPool+"="+owner,
		"--filter", "label="+provider.LabelKind+"=machine", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	names := strings.Fields(string(out))
	return d.inspectNames(ctx, names)
}

// Volumes lists cache volumes of owner.
func (d *Docker) Volumes(ctx context.Context, owner string) ([]provider.Volume, error) {
	// One listing with the labels: inspecting the listed names separately
	// fails as a whole when a volume disappears in between (a concurrent
	// destroy), which made reconciles fail.
	out, err := d.output(ctx, "volume", "ls", "--filter", "label="+provider.LabelPool+"="+owner, "--format", "{{.Name}}\t{{.Labels}}")
	if err != nil {
		return nil, err
	}
	type volInfo struct {
		Name   string
		Labels map[string]string
	}
	var res []volInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, labels, _ := strings.Cut(line, "\t")
		if name == "" {
			continue
		}
		r := volInfo{Name: name, Labels: map[string]string{}}
		for _, kv := range strings.Split(labels, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				r.Labels[k] = v
			}
		}
		res = append(res, r)
	}
	var vs []provider.Volume
	for _, r := range res {
		v := provider.Volume{Name: r.Name, Kind: r.Labels[provider.LabelKind]}
		if n, err := strconv.ParseInt(r.Labels[provider.LabelCreatedAt], 10, 64); err == nil {
			v.CreatedAt = time.Unix(n, 0)
		}
		if n, err := strconv.ParseInt(r.Labels[provider.LabelExpiresAt], 10, 64); err == nil {
			v.ExpiresAt = time.Unix(n, 0)
		}
		vs = append(vs, v)
	}
	return vs, nil
}

// DeleteVolume removes a volume. Volumes in use are left alone.
func (d *Docker) DeleteVolume(ctx context.Context, name string) error {
	err := d.run(ctx, nil, "volume", "rm", name)
	if err != nil && strings.Contains(err.Error(), "in use") {
		return errors.New("volume in use")
	}
	return err
}

func storeVolume(pool string) string { return "cs-store-" + sanitize(pool) }

var _ provider.StoreSource = (*Docker)(nil)

// NativeStore implements provider.StoreSource: a volume shared by the
// pool's machines, mounted at machine.StoreDir.
func (d *Docker) NativeStore(_ context.Context, pool string) (store.Config, bool, error) {
	return store.Config{Name: d.Name(), Dir: machine.StoreDir, Prefix: "cache"}, true, nil
}

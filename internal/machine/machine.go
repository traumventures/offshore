// Package machine is the single definition of a machine's container
// layout: the worker container (FUSE, capabilities, private
// cgroup namespace, published QUIC port, server binary, cache volumes,
// environment) and its service containers joined to the machine's network
// namespace, plus the order in which they are created and started.
//
// The local-docker provider applies the layout with the Docker CLI on the
// developer's host; the supervisor ("offshore supervisor") applies the same
// layout on VM hosts.
package machine

import (
	"sort"
	"strconv"

	"github.com/traumventures/offshore/internal/provider"
)

// Paths inside the machine container.
const (
	BinaryPath = "/opt/offshore/bin/offshore"
	CacheDir   = "/cache"
	DepsDir    = "/deps"
	// StoreDir is where a directory store shared by the machines appears
	// (local-docker's native store).
	StoreDir      = "/store"
	HostStatusDir = "/run/offshore-supervisor"
	// HostStatusEnv names the file the server publishes its status to for
	// the supervisor (see HostStatus).
	HostStatusEnv  = "OFFSHORE_HOST_STATUS"
	HostStatusFile = HostStatusDir + "/status.json"
	// ServerPort is the QUIC port of the server inside the container.
	ServerPort = 7443
)

// Layout describes the containers of one machine. It holds no secret
// values: SecretKeys only names the secret variables, whose values are
// supplied when the containers are created.
type Layout struct {
	ID          string             `json:"id"`
	Image       string             `json:"image"`
	Binary      string             `json:"binary"`  // host path of the static server binary
	Publish     string             `json:"publish"` // docker -p value for the server port
	CacheVolume string             `json:"cache_volume"`
	DepsVolume  string             `json:"deps_volume"`
	StoreVolume string             `json:"store_volume,omitempty"`   // shared directory store (empty: none)
	StatusDir   string             `json:"status_dir,omitempty"`     // host directory for the status file (empty: none)
	CPU         float64            `json:"cpu,omitempty"`            // container CPU limit (0: none)
	Memory      int64              `json:"memory,omitempty"`         // container memory limit (0: none)
	Swap        bool               `json:"swap,omitempty"`           // sessions may swap beyond Memory
	Labels      map[string]string  `json:"labels,omitempty"`         // machine container labels
	SvcLabels   map[string]string  `json:"service_labels,omitempty"` // service container labels
	Env         map[string]string  `json:"env,omitempty"`
	SecretKeys  []string           `json:"secret_keys,omitempty"`
	Services    []provider.Service `json:"services,omitempty"`
}

// Container is one container of a machine.
type Container struct {
	Name   string
	Image  string
	Create []string // arguments after "docker" that create the container
}

// Name returns the machine container name.
func (l Layout) Name() string { return Name(l.ID) }

// Name returns the machine container name of a machine id.
func Name(id string) string { return "cs-" + id }

// ServiceName returns the container name of a service.
func ServiceName(id, svc string) string { return Name(id) + "-svc-" + svc }

// ServerPortSpec is the server port in docker's port/proto notation.
func ServerPortSpec() string { return strconv.Itoa(ServerPort) + "/udp" }

// Secrets controls how secret variables appear in the create arguments.
type Secrets int

const (
	// SecretsFromProcessEnv passes "-e KEY": the Docker CLI takes the value
	// from its own environment, so values never appear in argument lists.
	SecretsFromProcessEnv Secrets = iota
	// SecretsInline passes "-e KEY=VALUE" (needed when the CLI runs through
	// a wrapper such as sudo that drops the environment).
	SecretsInline
)

// Containers returns the machine's containers in creation and start order:
// the machine first, then its services (which join its network namespace).
func (l Layout) Containers(secret map[string]string, mode Secrets) []Container {
	name := l.Name()
	args := []string{"create", "--name", name, "--hostname", name}
	args = append(args, labelArgs(l.Labels)...)
	args = append(args,
		"--device", "/dev/fuse",
		"--cap-add", "SYS_ADMIN",
		"--security-opt", "apparmor=unconfined",
		"--cgroupns", "private",
		"-p", l.Publish,
		"-v", l.Binary+":"+BinaryPath+":ro",
		"-v", l.CacheVolume+":"+CacheDir,
		"-v", l.DepsVolume+":"+DepsDir,
	)
	if l.StoreVolume != "" {
		args = append(args, "-v", l.StoreVolume+":"+StoreDir)
	}
	env := map[string]string{}
	for k, v := range l.Env {
		env[k] = v
	}
	if l.StatusDir != "" {
		args = append(args, "-v", l.StatusDir+":"+HostStatusDir)
		env[HostStatusEnv] = HostStatusFile
	}
	args = append(args, "--entrypoint", BinaryPath)
	for _, k := range provider.SortedKeys(env) {
		args = append(args, "-e", k+"="+env[k])
	}
	keys := append([]string{}, l.SecretKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		if mode == SecretsInline {
			args = append(args, "-e", k+"="+secret[k])
		} else {
			args = append(args, "-e", k)
		}
	}
	if l.CPU > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(l.CPU, 'f', -1, 64))
	}
	if l.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(l.Memory, 10))
		if l.Swap {
			args = append(args, "--memory-swap", "-1")
		}
	}
	args = append(args, l.Image, "worker")
	out := []Container{{Name: name, Image: l.Image, Create: args}}
	for _, svc := range l.Services {
		sn := ServiceName(l.ID, svc.Name)
		a := []string{"create", "--name", sn, "--network", "container:" + name}
		a = append(a, labelArgs(l.SvcLabels)...)
		for _, k := range provider.SortedKeys(svc.Env) {
			a = append(a, "-e", k+"="+svc.Env[k])
		}
		a = append(a, svc.Image)
		a = append(a, svc.Command...)
		out = append(out, Container{Name: sn, Image: svc.Image, Create: a})
	}
	return out
}

// StartOrder lists the container names in the order they must be started
// on every boot (services join the machine's new network namespace).
func (l Layout) StartOrder() []string {
	names := []string{l.Name()}
	for _, svc := range l.Services {
		names = append(names, ServiceName(l.ID, svc.Name))
	}
	return names
}

// SecretEnv returns secret as KEY=VALUE entries for the Docker CLI process
// environment (SecretsFromProcessEnv).
func (l Layout) SecretEnv(secret map[string]string) []string {
	var out []string
	for _, k := range provider.SortedKeys(secret) {
		out = append(out, k+"="+secret[k])
	}
	return out
}

func labelArgs(kv map[string]string) []string {
	var out []string
	for _, k := range provider.SortedKeys(kv) {
		out = append(out, "--label", k+"="+kv[k])
	}
	return out
}

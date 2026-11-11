// Package provider defines how machines are created, stopped and destroyed
// on an infrastructure backend.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/image"
)

// Label keys attached to every resource.
const (
	LabelPool      = "offshore.pool"
	LabelMachine   = "offshore.machine"
	LabelEnvHash   = "offshore.env-hash"
	LabelCreatedAt = "offshore.created-at"
	LabelExpiresAt = "offshore.expires-at"
	LabelKind      = "offshore.kind"
	LabelCPU       = "offshore.cpu"
	LabelMemory    = "offshore.memory"
	LabelFinger    = "offshore.fingerprint"
	LabelSize      = "offshore.size"
	LabelArch      = "offshore.arch" // machine architecture (amd64, arm64)
)

// MachineSpec describes a machine to create. Providers treat the server
// environment as opaque: Env is public, SecretEnv must be delivered
// confidentially (never in plain instance metadata such as user data).
type MachineSpec struct {
	ID      string
	Owner   string
	Image   string // environment image reference
	EnvHash string
	Size    string // machine type name
	// Arch is the machine architecture ("amd64", "arm64"; empty: amd64);
	// Image is built for it.
	Arch string
	// InstanceTypes are the provider machine types providing Size, tried
	// in order (empty: Size itself).
	InstanceTypes []string
	CPU           float64
	Memory        int64
	// Zone places the machine (empty: the provider's default zone; with
	// Volume: the volume's zone).
	Zone string
	// Volume is the id of an available cache volume (Volumes part) the
	// machine gets as its cache disk at creation; empty: the cache lives
	// on the machine's own disk.
	Volume string
	// CacheTTL: a cache volume's content unused for longer is wiped when
	// it is attached. Without Volume the cache lives on the root disk or,
	// if the instance type has one, an instance-store NVMe disk.
	CacheTTL  time.Duration
	Swap      bool // allow sessions to swap
	ExpiresAt time.Time
	Finger    string        // expected server certificate fingerprint (public)
	DepsTTL   time.Duration // lifetime of shared dependency cache volumes
	IdleStop  time.Duration // machine stops itself after this long idle and unobserved (0: never)
	Services  []Service
	Env       map[string]string // OFFSHORE_* variables of the worker
	SecretEnv map[string]string // secret OFFSHORE_* variables
}

// SortedKeys returns the keys of m in order.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Service is an auxiliary container sharing the machine's network.
type Service struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Env     map[string]string `json:"env,omitempty"`
	Command []string          `json:"command,omitempty"`
}

// State of a machine.
type State string

const (
	Running  State = "running"
	Stopped  State = "stopped"
	Starting State = "starting"
	Failed   State = "failed"
)

// Machine is a machine known to a provider.
type Machine struct {
	ID          string
	State       State
	Addr        string // host:port of the server, empty when not running
	EnvHash     string
	CPU         float64
	Memory      int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Fingerprint string
	// Zone is the machine's availability zone (aws-ec2) or domain (oci);
	// volumes attach only within it.
	Zone   string
	Labels map[string]string
}

// Caps describes what a provider supports.
type Caps struct {
	FUSE          bool
	StopKeepsDisk bool
	// PollInterval is how often the orchestrator polls a starting machine
	// for readiness (0: 500ms); with PollMax > PollInterval the interval
	// backs off up to PollMax.
	PollInterval time.Duration
	PollMax      time.Duration
}

// Provider manages machines on one backend.
type Provider interface {
	Name() string
	Caps() Caps
	// Create provisions and starts a new machine.
	Create(ctx context.Context, spec MachineSpec) (Machine, error)
	// Start restarts a stopped machine (its disk is kept).
	Start(ctx context.Context, id string) (Machine, error)
	// Stop stops a machine; compute is released, the disk is kept.
	Stop(ctx context.Context, id string) error
	// Destroy removes a machine and its disk.
	Destroy(ctx context.Context, id string) error
	// List returns all machines of owner.
	List(ctx context.Context, owner string) ([]Machine, error)
}

// ImageSpec describes an environment image to build.
type ImageSpec = image.Spec

// ImageHost is implemented by providers whose machines run the environment
// image: the image pipeline builds it for the platform and pushes it to the
// registry right before a machine is created. Providers that do not
// implement it get the environment tag and must have the image already.
type ImageHost interface {
	// ImagePlatform is the platform machines of the named size run.
	ImagePlatform(size string) image.Platform
	// ImageRegistry is where machines pull the image from.
	ImageRegistry() image.Registry
	// ImageBuilder builds images for this provider (its Docker CLI).
	ImageBuilder() image.Builder
}

// ErrCapacity marks errors meaning the provider has no capacity or quota
// for a machine right now (every instance type of a size failed with a
// capacity, limit or quota error). The orchestrator downgrades such a
// provider for a cooldown.
var ErrCapacity = errors.New("no capacity")

// Factory creates a provider.
type Factory func(opts map[string]any) (Provider, error)

var (
	regMu    sync.Mutex
	registry = map[string]Factory{}
)

// Register adds a backend.
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[name] = f
}

// Open creates the provider for a backend name.
func Open(name string, opts map[string]any) (Provider, error) {
	regMu.Lock()
	f, ok := registry[name]
	regMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (available: %v)", name, Names())
	}
	return f(opts)
}

// Names lists registered backends.
func Names() []string {
	regMu.Lock()
	defer regMu.Unlock()
	var out []string
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// StringList converts a decoded option ([]string or []any) to []string.
func StringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// String returns a string option or def.
func String(opts map[string]any, key, def string) string {
	if s, ok := opts[key].(string); ok && s != "" {
		return s
	}
	return def
}

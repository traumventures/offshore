// Package provider defines how machines are created, stopped and destroyed
// on an infrastructure backend.
package provider

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Label keys attached to every resource.
const (
	LabelOwner     = "cloudsourced.owner"
	LabelFleet     = "cloudsourced.fleet"
	LabelMachine   = "cloudsourced.machine"
	LabelEnvHash   = "cloudsourced.env-hash"
	LabelCreatedAt = "cloudsourced.created-at"
	LabelExpiresAt = "cloudsourced.expires-at"
	LabelKind      = "cloudsourced.kind"
	LabelCPU       = "cloudsourced.cpu"
	LabelMemory    = "cloudsourced.memory"
	LabelFinger    = "cloudsourced.fingerprint"
	LabelSize      = "cloudsourced.size"
)

// MachineSpec describes a machine to create. Providers treat the server
// environment as opaque: Env is public, SecretEnv must be delivered
// confidentially (never in plain instance metadata such as user data).
type MachineSpec struct {
	ID        string
	Owner     string
	Image     string // environment image reference
	EnvHash   string
	Size      string // machine type name
	CPU       float64
	Memory    int64
	Swap      bool // allow sessions to swap
	ExpiresAt time.Time
	Finger    string        // expected server certificate fingerprint (public)
	DepsTTL   time.Duration // lifetime of shared dependency cache volumes
	IdleStop  time.Duration // machine stops itself after this long idle and unobserved (0: never)
	Services  []Service
	Env       map[string]string // CLOUDSOURCED_* variables of the machine server
	SecretEnv map[string]string // secret CLOUDSOURCED_* variables
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
	Labels      map[string]string
}

// Volume is a cache volume.
type Volume struct {
	Name      string
	Kind      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Caps describes what a provider supports.
type Caps struct {
	FUSE          bool
	StopKeepsDisk bool
	Spot          bool
}

// Provider manages machines on one backend.
type Provider interface {
	Name() string
	Caps() Caps
	// EnsureImage makes the environment image available, building it from
	// the given build context if necessary. Build output goes to log.
	EnsureImage(ctx context.Context, img ImageSpec, log io.Writer) error
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
	// Volumes lists cache volumes of owner; DeleteVolume removes one.
	Volumes(ctx context.Context, owner string) ([]Volume, error)
	DeleteVolume(ctx context.Context, name string) error
}

// ImageSpec describes an environment image to build.
type ImageSpec struct {
	Tag        string
	ContextDir string
	Dockerfile string // path inside ContextDir
	BaseTag    string // optional user base image built first
	BaseDir    string
	BaseFile   string
}

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

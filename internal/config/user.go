package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// User is the per-user configuration: where and how machines run.
type User struct {
	Fleet  Fleet  `toml:"fleet"`
	Docker Docker `toml:"docker"` // shorthand for a single local-docker provider
	// Providers are named provider instances, e.g. [providers.local] with
	// backend = "local-docker" and [providers.aws] with backend = "aws-ec2".
	// Every other key of the table is passed to the backend as an option.
	Providers map[string]map[string]any `toml:"providers"`
}

// Backends known to the configuration.
var Backends = []string{"local-docker", "aws-ec2"}

// Fleet configures machine provisioning and scaling.
type Fleet struct {
	Backend       string          `toml:"backend"` // default backend when no [providers] are given
	Owner         string          `toml:"owner"`
	MachineCPU    float64         `toml:"machine_cpu"`
	MachineMemory string          `toml:"machine_memory"`
	MinMachines   int             `toml:"min_machines"` // warm pool per recently used environment
	MaxMachines   int             `toml:"max_machines"`
	IdleStop      string          `toml:"idle_stop"`
	MaxLifetime   string          `toml:"max_lifetime"`
	QueueTimeout  string          `toml:"queue_timeout"`
	Provision     []ProvisionStep `toml:"provision"`
	Sizes         []Size          `toml:"size"`

	// Scheduling. Overcommitting CPU only slows tasks down; overcommitting
	// memory needs soft limits and swap, otherwise tasks may be OOM killed.
	Policy            string  `toml:"policy"`    // "default"
	Estimator         string  `toml:"estimator"` // "history" (learn from runs) | "static" (repo config)
	CPUOvercommit     float64 `toml:"cpu_overcommit"`
	MemoryOvercommit  float64 `toml:"memory_overcommit"`
	MaxCPUPressure    float64 `toml:"max_cpu_pressure"`    // 0..1, PSI some avg10; 0 = off
	MaxMemoryPressure float64 `toml:"max_memory_pressure"` // 0..1; 0 = off
	SoftLimit         float64 `toml:"soft_limit"`          // memory.high = need * soft_limit; 0 = off
	Swap              bool    `toml:"swap"`
	LimitJobs         bool    `toml:"limit_jobs"` // pass reserved CPU as build parallelism
}

// Size is a machine type a provider may create.
type Size struct {
	Provider     string  `toml:"provider"` // provider instance; optional with a single provider
	Name         string  `toml:"name"`
	CPU          float64 `toml:"cpu"`
	Memory       string  `toml:"memory"`
	PricePerHour float64 `toml:"price_per_hour"`
}

// ProvisionStep is a machine level provisioning command, executed once per
// machine before it accepts sessions.
type ProvisionStep struct {
	Run string `toml:"run"`
}

// Docker configures the local-docker backend.
type Docker struct {
	// Command is the docker CLI invocation, e.g. ["sudo", "-n", "docker"].
	Command []string `toml:"command"`
	// Host is the address used to reach published ports.
	Host string `toml:"host"`
}

// UserConfigPath returns the path of the user configuration file.
func UserConfigPath() string {
	if p := os.Getenv("CLOUDSOURCED_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "cloudsourced", "config.toml")
}

// LoadUser loads the user configuration, applying defaults when it is absent.
func LoadUser() (*User, error) {
	var u User
	data, err := os.ReadFile(UserConfigPath())
	if err == nil {
		if err := decodeStrict(data, &u); err != nil {
			return nil, fmt.Errorf("%s: %w", UserConfigPath(), err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	u.applyDefaults()
	if err := u.Validate(); err != nil {
		return nil, err
	}
	return &u, nil
}

func (u *User) applyDefaults() {
	f := &u.Fleet
	if f.Backend == "" {
		f.Backend = "local-docker"
	}
	if f.Owner == "" {
		f.Owner = os.Getenv("USER")
		if f.Owner == "" {
			f.Owner = "default"
		}
	}
	if f.MachineCPU == 0 {
		f.MachineCPU = 8
	}
	if f.MachineMemory == "" {
		f.MachineMemory = "16G"
	}
	if f.MaxMachines == 0 {
		f.MaxMachines = 4
	}
	if f.IdleStop == "" {
		f.IdleStop = "20m"
	}
	if f.MaxLifetime == "" {
		f.MaxLifetime = "24h"
	}
	if f.QueueTimeout == "" {
		f.QueueTimeout = "2m"
	}
	if f.CPUOvercommit == 0 {
		f.CPUOvercommit = 2
	}
	if f.MemoryOvercommit == 0 {
		f.MemoryOvercommit = 1
	}
	if f.Policy == "" {
		f.Policy = "default"
	}
	if f.Estimator == "" {
		f.Estimator = "history"
	}
	if len(u.Docker.Command) == 0 {
		if c := os.Getenv("CLOUDSOURCED_DOCKER"); c != "" {
			u.Docker.Command = strings.Fields(c)
		} else {
			u.Docker.Command = []string{"docker"}
		}
	}
	if u.Docker.Host == "" {
		u.Docker.Host = "127.0.0.1"
	}
	if len(u.Providers) == 0 {
		// Legacy single-provider form: fleet.backend + [docker].
		opts := map[string]any{"backend": f.Backend}
		if f.Backend == "local-docker" {
			cmd := make([]any, len(u.Docker.Command))
			for i, c := range u.Docker.Command {
				cmd[i] = c
			}
			opts["command"], opts["host"] = cmd, u.Docker.Host
		}
		u.Providers = map[string]map[string]any{"local": opts}
	}
	if len(f.Sizes) == 0 {
		for _, name := range u.ProviderNames() {
			f.Sizes = append(f.Sizes, Size{Provider: name, Name: "default", CPU: f.MachineCPU, Memory: f.MachineMemory})
		}
	}
	if names := u.ProviderNames(); len(names) == 1 {
		for i := range f.Sizes {
			if f.Sizes[i].Provider == "" {
				f.Sizes[i].Provider = names[0]
			}
		}
	}
}

// ProviderNames returns the configured provider instances, sorted.
func (u *User) ProviderNames() []string {
	var out []string
	for n := range u.Providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ProviderBackend returns the backend of a provider instance.
func (u *User) ProviderBackend(name string) string {
	b, _ := u.Providers[name]["backend"].(string)
	return b
}

// Validate checks the user configuration.
func (u *User) Validate() error {
	var errs []string
	f := u.Fleet
	for _, name := range u.ProviderNames() {
		b := u.ProviderBackend(name)
		if !slices.Contains(Backends, b) {
			errs = append(errs, fmt.Sprintf("providers.%s.backend: unsupported %q (available: %s)", name, b, strings.Join(Backends, ", ")))
		}
	}
	if _, err := ParseBytes(f.MachineMemory); err != nil {
		errs = append(errs, "fleet.machine_memory: "+err.Error())
	}
	for name, v := range map[string]string{"idle_stop": f.IdleStop, "max_lifetime": f.MaxLifetime, "queue_timeout": f.QueueTimeout} {
		if _, err := ParseDuration(v); err != nil {
			errs = append(errs, "fleet."+name+": "+err.Error())
		}
	}
	if f.Policy != "default" {
		errs = append(errs, fmt.Sprintf("fleet.policy: unsupported %q (default)", f.Policy))
	}
	if f.Estimator != "history" && f.Estimator != "static" {
		errs = append(errs, fmt.Sprintf("fleet.estimator: unsupported %q (history, static)", f.Estimator))
	}
	if f.CPUOvercommit < 1 || f.MemoryOvercommit < 1 {
		errs = append(errs, "fleet: cpu_overcommit and memory_overcommit must be >= 1")
	}
	if f.MemoryOvercommit > 1 && !f.Swap && f.SoftLimit == 0 {
		errs = append(errs, "fleet.memory_overcommit > 1 needs swap = true or soft_limit, otherwise sessions are OOM killed instead of slowed down")
	}
	for i, sz := range f.Sizes {
		if sz.Name == "" || sz.CPU <= 0 {
			errs = append(errs, fmt.Sprintf("fleet.size[%d]: name and cpu > 0 required", i))
		}
		if _, err := ParseBytes(sz.Memory); err != nil {
			errs = append(errs, fmt.Sprintf("fleet.size[%d].memory: %v", i, err))
		}
		if _, ok := u.Providers[sz.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("fleet.size[%d].provider: %q is not a configured provider (%s)", i, sz.Provider, strings.Join(u.ProviderNames(), ", ")))
		}
	}
	if f.MinMachines < 0 || f.MaxMachines < 1 || f.MinMachines > f.MaxMachines {
		errs = append(errs, "fleet: need 0 <= min_machines <= max_machines and max_machines >= 1")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid %s:\n  %s", UserConfigPath(), strings.Join(errs, "\n  "))
	}
	return nil
}

// Durations returns the parsed fleet durations.
func (f Fleet) Durations() (idle, lifetime, queue time.Duration) {
	idle, _ = ParseDuration(f.IdleStop)
	lifetime, _ = ParseDuration(f.MaxLifetime)
	queue, _ = ParseDuration(f.QueueTimeout)
	return
}

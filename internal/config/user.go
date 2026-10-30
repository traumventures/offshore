package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// User is the per-user configuration: where and how machines run.
type User struct {
	Fleet  Fleet  `toml:"fleet"`
	Docker Docker `toml:"docker"`
}

// Fleet configures machine provisioning and scaling.
type Fleet struct {
	Backend       string          `toml:"backend"` // "local-docker"
	Owner         string          `toml:"owner"`
	MachineCPU    float64         `toml:"machine_cpu"`
	MachineMemory string          `toml:"machine_memory"`
	MinMachines   int             `toml:"min_machines"` // warm pool per recently used environment
	MaxMachines   int             `toml:"max_machines"`
	IdleStop      string          `toml:"idle_stop"`
	MaxLifetime   string          `toml:"max_lifetime"`
	QueueTimeout  string          `toml:"queue_timeout"`
	CPUOvercommit float64         `toml:"cpu_overcommit"`
	Provision     []ProvisionStep `toml:"provision"`
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
}

// Validate checks the user configuration.
func (u *User) Validate() error {
	var errs []string
	f := u.Fleet
	if f.Backend != "local-docker" {
		errs = append(errs, fmt.Sprintf("fleet.backend: unsupported %q (available: local-docker)", f.Backend))
	}
	if _, err := ParseBytes(f.MachineMemory); err != nil {
		errs = append(errs, "fleet.machine_memory: "+err.Error())
	}
	for name, v := range map[string]string{"idle_stop": f.IdleStop, "max_lifetime": f.MaxLifetime, "queue_timeout": f.QueueTimeout} {
		if _, err := ParseDuration(v); err != nil {
			errs = append(errs, "fleet."+name+": "+err.Error())
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

package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// FleetConfig is the fleet configuration: how machines are sized,
// scheduled and provisioned, and by which providers. It is read by the
// orchestrator, i.e. by the control plane or, in embedded mode, by the local
// daemon. It never contains client settings ([orchestrator], user).
type FleetConfig struct {
	// Path is the file the configuration was read from ("" = defaults only).
	Path  string `toml:"-"`
	Fleet Fleet  `toml:"fleet"`
	// Docker is the deprecated shorthand for a single local-docker provider.
	Docker Docker `toml:"docker"`
	// Providers are named provider instances, e.g. [providers.local] with
	// backend = "local-docker" and [providers.aws] with backend = "aws-ec2".
	// Every other key of the table is passed to the backend as an option.
	Providers map[string]map[string]any `toml:"providers"`
	// Warnings are deprecations and ignored sections found while loading.
	Warnings []string `toml:"-"`
}

// Backends known to the configuration.
var Backends = []string{"local-docker", "aws-ec2"}

// Fleet configures machine provisioning and scaling.
type Fleet struct {
	// ID names the fleet: machines and volumes carry it in the
	// cloudsourced.owner label, and an orchestrator only manages resources
	// of its own fleet. Default: the deprecated owner key, else $USER.
	ID string `toml:"id"`
	// Owner is the deprecated name of ID.
	Owner         string          `toml:"owner"`
	Backend       string          `toml:"backend"` // deprecated: default backend when no [providers] are given
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
	Policy            string  `toml:"policy"`    // "default" (= "binpack") | "classic"
	Estimator         string  `toml:"estimator"` // "history" (learn from runs) | "static" (repo config)
	CPUOvercommit     float64 `toml:"cpu_overcommit"`
	MemoryOvercommit  float64 `toml:"memory_overcommit"`
	MaxCPUPressure    float64 `toml:"max_cpu_pressure"`    // 0..1, PSI some avg10; 0 = off
	MaxMemoryPressure float64 `toml:"max_memory_pressure"` // 0..1; 0 = off
	SoftLimit         float64 `toml:"soft_limit"`          // memory.high = need * soft_limit; 0 = off
	Swap              bool    `toml:"swap"`
	LimitJobs         bool    `toml:"limit_jobs"` // pass reserved CPU as build parallelism
	// ConsolidateBelow drains the least loaded machine of an environment
	// when its machines are reserved below this fraction (default 0.5,
	// 0 = off). ConsolidateTarget caps the share the others may reach.
	ConsolidateBelow  *float64 `toml:"consolidate_below"`
	ConsolidateTarget float64  `toml:"consolidate_target"` // default 0.75
	// WarmPool keeps machines running at scheduled times, in addition to
	// min_machines: [[fleet.warm_pool]] days = ["mon", "tue"], from = "08:00",
	// to = "19:00", machines = 1, timezone = "Europe/Berlin".
	WarmPool []WarmPool `toml:"warm_pool"`
}

// WarmPool is a scheduled warm pool window.
type WarmPool struct {
	Days     []string `toml:"days"` // "mon".."sun"; empty = every day
	From     string   `toml:"from"` // "HH:MM"
	To       string   `toml:"to"`   // "HH:MM"; earlier than from spans midnight
	Machines int      `toml:"machines"`
	Timezone string   `toml:"timezone"` // IANA name; default local time
}

// Parse returns the weekdays, start and end offsets since midnight and the
// location of the window.
func (w WarmPool) Parse() (days []time.Weekday, from, to time.Duration, loc *time.Location, err error) {
	for _, d := range w.Days {
		wd, err := parseWeekday(d)
		if err != nil {
			return nil, 0, 0, nil, err
		}
		days = append(days, wd)
	}
	if from, err = parseClock(w.From); err != nil {
		return
	}
	if to, err = parseClock(w.To); err != nil {
		return
	}
	if from == to {
		err = fmt.Errorf("from and to are equal (%s)", w.From)
		return
	}
	loc = time.Local
	if w.Timezone != "" {
		loc, err = time.LoadLocation(w.Timezone)
	}
	return
}

func parseWeekday(s string) (time.Weekday, error) {
	l := strings.ToLower(strings.TrimSpace(s))
	for d := time.Sunday; d <= time.Saturday; d++ {
		name := strings.ToLower(d.String())
		if l == name || l == name[:3] {
			return d, nil
		}
	}
	return 0, fmt.Errorf("unknown weekday %q", s)
}

func parseClock(s string) (time.Duration, error) {
	var h, m int
	if n, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || n != 2 || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("invalid time of day %q (want HH:MM)", s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
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

// FleetConfigPath returns the fleet configuration file:
// $CLOUDSOURCED_FLEET_CONFIG, else the client configuration file (embedded
// mode and single-host control planes keep both in one file).
func FleetConfigPath() string {
	if p := os.Getenv("CLOUDSOURCED_FLEET_CONFIG"); p != "" {
		return p
	}
	return ClientConfigPath()
}

// LoadFleet loads the fleet configuration from path, applying defaults when
// the file is absent. Client sections in the file are ignored (see Warnings).
func LoadFleet(path string) (*FleetConfig, error) {
	f := &FleetConfig{Path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		var raw rawFile
		if err := decodeStrict(data, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		f.Fleet, f.Docker, f.Providers = raw.Fleet, raw.Docker, raw.Providers
		top, _ := topLevel(data)
		if clientSections(top) != nil {
			f.Warnings = append(f.Warnings, fmt.Sprintf("%s: %s ignored by the fleet (client settings)", path, strings.Join(clientSections(top), ", ")))
		}
		if top["docker"] {
			f.Warnings = append(f.Warnings, path+": [docker] is deprecated, use [providers.local] with backend = \"local-docker\"")
		}
		if raw.Fleet.Backend != "" {
			f.Warnings = append(f.Warnings, path+": fleet.backend is deprecated, use [providers.<name>] backend = ...")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if fl := &f.Fleet; fl.Owner != "" {
		switch {
		case fl.ID == "":
			fl.ID = fl.Owner
			f.Warnings = append(f.Warnings, path+": fleet.owner is deprecated, use fleet.id")
		case fl.ID != fl.Owner:
			return nil, fmt.Errorf("invalid %s:\n  fleet.id %q and deprecated fleet.owner %q differ", path, fl.ID, fl.Owner)
		}
	}
	f.applyDefaults()
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

func (u *FleetConfig) applyDefaults() {
	f := &u.Fleet
	if f.Backend == "" {
		f.Backend = "local-docker"
	}
	if f.ID == "" {
		f.ID = sanitizeID(defaultUser())
	}
	f.Owner = ""
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
	if f.ConsolidateBelow == nil {
		v := 0.5
		f.ConsolidateBelow = &v
	}
	if f.Estimator == "" {
		f.Estimator = "history"
	}
	if len(u.Providers) == 0 {
		// Legacy single-provider form: fleet.backend + [docker].
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
func (u *FleetConfig) ProviderNames() []string {
	var out []string
	for n := range u.Providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ProviderBackend returns the backend of a provider instance.
func (u *FleetConfig) ProviderBackend(name string) string {
	b, _ := u.Providers[name]["backend"].(string)
	return b
}

// Validate checks the fleet configuration.
func (u *FleetConfig) Validate() error {
	var errs []string
	if !validID(u.Fleet.ID) {
		errs = append(errs, fmt.Sprintf("fleet.id: %q must be 1-63 characters of letters, digits, '.', '_' and '-'", u.Fleet.ID))
	}
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
	if f.Policy != "default" && f.Policy != "binpack" && f.Policy != "classic" {
		errs = append(errs, fmt.Sprintf("fleet.policy: unsupported %q (default, binpack, classic)", f.Policy))
	}
	if f.ConsolidateBelow != nil && (*f.ConsolidateBelow < 0 || *f.ConsolidateBelow > 1) {
		errs = append(errs, "fleet.consolidate_below: must be between 0 (off) and 1")
	}
	if f.ConsolidateTarget < 0 || f.ConsolidateTarget > 1 {
		errs = append(errs, "fleet.consolidate_target: must be between 0 and 1")
	}
	for i, w := range f.WarmPool {
		if _, _, _, _, err := w.Parse(); err != nil {
			errs = append(errs, fmt.Sprintf("fleet.warm_pool[%d]: %v", i, err))
		}
		if w.Machines < 0 || w.Machines > f.MaxMachines {
			errs = append(errs, fmt.Sprintf("fleet.warm_pool[%d].machines: need 0 <= machines <= max_machines", i))
		}
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
		return fmt.Errorf("invalid %s:\n  %s", u.displayPath(), strings.Join(errs, "\n  "))
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

// sanitizeID turns a user name into a valid fleet id.
func sanitizeID(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			b[i] = '-'
		}
	}
	if len(b) > 63 {
		b = b[:63]
	}
	return string(b)
}

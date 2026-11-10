package config

import (
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// PoolConfig is the pool configuration: how machines are sized,
// scheduled and provisioned, and by which providers. It is read by the
// orchestrator, i.e. by the control plane or, in embedded mode, by the local
// daemon. It never contains client settings ([control_plane], user).
type PoolConfig struct {
	// Path is the file the configuration was read from ("" = defaults only).
	Path string `toml:"-"`
	Pool Pool   `toml:"pool"`
	// Providers are named provider instances, e.g. [providers.local] with
	// type = "local-docker", [providers.aws] with type = "aws-ec2" and
	// [providers.oci] with type = "oci".
	// Every other key of the table is passed to the provider as an option.
	Providers map[string]map[string]any `toml:"providers"`
	// Telemetry configures tracing of the orchestrator (control plane or
	// embedded) and, forwarded, of its machines; and the event log.
	Telemetry Telemetry `toml:"telemetry"`
	// Warnings are ignored sections found while loading.
	Warnings []string `toml:"-"`
}

// Backends known to the configuration.
var Backends = []string{"local-docker", "aws-ec2", "oci"}

// cloudBackend reports whether a provider type launches cloud instances
// with instance types (shapes) and spot (preemptible) capacity.
func cloudBackend(t string) bool { return t == "aws-ec2" || t == "oci" }

// Pool configures machine provisioning and scaling.
type Pool struct {
	// ID names the pool: machines and volumes carry it in the
	// cloudsourced.pool label, and an orchestrator only manages resources
	// of its own pool. Default: $USER.
	ID            string  `toml:"id"`
	MachineCPU    float64 `toml:"machine_cpu"`
	MachineMemory string  `toml:"machine_memory"`
	MinMachines   int     `toml:"min_machines"` // warm pool per recently used environment
	MaxMachines   int     `toml:"max_machines"`
	IdleStop      string  `toml:"idle_stop"`
	MaxLifetime   string  `toml:"max_lifetime"`
	QueueTimeout  string  `toml:"queue_timeout"`
	// Provision are the machine setup steps ([[machine.setup]]).
	Provision []ProvisionStep `toml:"-"`
	// Sizes are the machine types of all providers ([[providers.NAME.size]]).
	Sizes []Size `toml:"-"`

	// Scheduler holds the scheduling settings ([scheduler]).
	Scheduler `toml:"-"`
}

// Scheduler configures how sessions are placed on machines and when the
// pool consolidates. Overcommitting CPU only slows tasks down;
// overcommitting memory needs soft limits and swap, otherwise tasks may be
// OOM killed.
type Scheduler struct {
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
	// min_machines: [[scheduler.warm_pool]] days = ["mon", "tue"], from = "08:00",
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
	Provider     string  `toml:"-"` // the enclosing [providers.NAME]
	Name         string  `toml:"name"`
	CPU          float64 `toml:"cpu"`
	Memory       string  `toml:"memory"`
	PricePerHour float64 `toml:"price_per_hour"` // on-demand price
	// Arch is the machine architecture, "amd64" (default) or "arm64".
	// Environments are built per architecture; a repository pins its
	// architecture with resources.arch, and only sizes of that
	// architecture are created for it.
	Arch string `toml:"arch"`
	// InstanceTypes are the provider machine types that provide this size,
	// tried in order (aws-ec2 instance types, oci shapes; default: the
	// size name on aws-ec2, a flexible shape of the architecture on oci).
	InstanceTypes []string `toml:"instance_types"`
	// Spot launches machines of this size as spot instances (aws-ec2) or
	// preemptible instances (oci).
	// Spot machines can be interrupted with two minutes' notice: they
	// drain and running sessions get until the deadline.
	Spot bool `toml:"spot"`
	// SpotDiscount is the expected spot discount on price_per_hour used for
	// sizing and cost estimates (0..1, default 0.5 for spot sizes).
	SpotDiscount *float64 `toml:"spot_discount"`
	// SpotFallback launches on-demand when no spot capacity is available
	// for any instance type (default true).
	SpotFallback *bool `toml:"spot_fallback"`
}

// Machine architectures of sizes.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// DefaultSpotDiscount is the assumed spot discount when none is configured.
const DefaultSpotDiscount = 0.5

// MachineArch maps a repository architecture (resources.arch: "x86_64",
// "aarch64") or a size architecture to the size naming ("amd64",
// "arm64"). Empty means amd64.
func MachineArch(a string) string {
	switch a {
	case "", "x86_64", "amd64":
		return ArchAMD64
	case "aarch64", "arm64":
		return ArchARM64
	}
	return a
}

// EffectivePrice is the expected hourly price: the on-demand price, less
// the spot discount for spot sizes.
func (s Size) EffectivePrice() float64 {
	if !s.Spot {
		return s.PricePerHour
	}
	return s.PricePerHour * (1 - s.Discount())
}

// Discount returns the spot discount (0 for on-demand sizes).
func (s Size) Discount() float64 {
	if !s.Spot {
		return 0
	}
	if s.SpotDiscount != nil {
		return *s.SpotDiscount
	}
	return DefaultSpotDiscount
}

// Fallback reports whether a spot size falls back to on-demand.
func (s Size) Fallback() bool { return s.SpotFallback == nil || *s.SpotFallback }

// ProvisionStep is a machine level provisioning command, executed once per
// machine before it accepts sessions.
type ProvisionStep struct {
	Run string `toml:"run"`
}

// PoolConfigPath returns the pool configuration file:
// $CLOUDSOURCED_POOL_CONFIG, else the client configuration file (embedded
// mode and single-host control planes keep both in one file).
func PoolConfigPath() string {
	if p := os.Getenv("CLOUDSOURCED_POOL_CONFIG"); p != "" {
		return p
	}
	return ClientConfigPath()
}

// LoadPool loads the pool configuration from path, applying defaults when
// the file is absent. Client sections in the file are ignored (see Warnings).
//
// Schema:
//
//	[pool]                  id, machine_cpu/memory, min/max_machines, lifetimes
//	[scheduler]             policy, estimator, overcommit, pressure, consolidation
//	[[scheduler.warm_pool]] scheduled warm machines
//	[[machine.setup]]       run = "..." once per machine
//	[providers.NAME]        type = "local-docker" | "aws-ec2" | "oci", options
//	[[providers.NAME.size]] machine types of that provider
//	[telemetry]
func LoadPool(path string) (*PoolConfig, error) {
	f := &PoolConfig{Path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		var raw rawFile
		if err := decodeStrict(data, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		f.Pool, f.Telemetry = raw.Pool, raw.Telemetry
		f.Pool.Scheduler = raw.Scheduler
		f.Pool.Provision = raw.Machine.Setup
		if f.Providers, f.Pool.Sizes, err = splitProviders(raw.Providers); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		top, _ := topLevel(data)
		if clientSections(top) != nil {
			f.Warnings = append(f.Warnings, fmt.Sprintf("%s: %s ignored by the pool (client settings)", path, strings.Join(clientSections(top), ", ")))
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f.applyDefaults()
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// splitProviders separates the [[providers.NAME.size]] tables from the
// provider options.
func splitProviders(in map[string]map[string]any) (map[string]map[string]any, []Size, error) {
	var sizes []Size
	out := map[string]map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(in)) {
		opts := maps.Clone(in[name])
		if raw, ok := opts["size"]; ok {
			delete(opts, "size")
			data, err := toml.Marshal(map[string]any{"size": raw})
			if err != nil {
				return nil, nil, fmt.Errorf("providers.%s.size: %w", name, err)
			}
			var t struct {
				Size []Size `toml:"size"`
			}
			if err := decodeStrict(data, &t); err != nil {
				return nil, nil, fmt.Errorf("providers.%s.size: %w", name, err)
			}
			for _, sz := range t.Size {
				sz.Provider = name
				sizes = append(sizes, sz)
			}
		}
		out[name] = opts
	}
	if len(out) == 0 {
		out = nil
	}
	return out, sizes, nil
}

func (u *PoolConfig) applyDefaults() {
	f := &u.Pool
	if f.ID == "" {
		f.ID = sanitizeID(defaultUser())
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
	if f.ConsolidateBelow == nil {
		v := 0.5
		f.ConsolidateBelow = &v
	}
	if f.Estimator == "" {
		f.Estimator = "history"
	}
	if len(u.Providers) == 0 {
		// Without [providers]: one local-docker provider on this host.
		cmd := []any{"docker"}
		if c := os.Getenv("CLOUDSOURCED_DOCKER"); c != "" {
			cmd = nil
			for _, w := range strings.Fields(c) {
				cmd = append(cmd, w)
			}
		}
		u.Providers = map[string]map[string]any{"local": {"type": "local-docker", "command": cmd, "host": "127.0.0.1"}}
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
	for i := range f.Sizes {
		if f.Sizes[i].Arch == "" {
			f.Sizes[i].Arch = ArchAMD64
			if u.ProviderType(f.Sizes[i].Provider) == "local-docker" {
				f.Sizes[i].Arch = MachineArch(runtime.GOARCH) // machines run on this host
			}
		}
	}
}

// ProviderNames returns the configured provider instances, sorted.
func (u *PoolConfig) ProviderNames() []string {
	var out []string
	for n := range u.Providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ProviderType returns the type of a provider instance.
func (u *PoolConfig) ProviderType(name string) string {
	b, _ := u.Providers[name]["type"].(string)
	return b
}

// Validate checks the pool configuration.
func (u *PoolConfig) Validate() error {
	var errs []string
	if !validID(u.Pool.ID) {
		errs = append(errs, fmt.Sprintf("pool.id: %q must be 1-63 characters of letters, digits, '.', '_' and '-'", u.Pool.ID))
	}
	f := u.Pool
	for _, name := range u.ProviderNames() {
		b := u.ProviderType(name)
		if !slices.Contains(Backends, b) {
			errs = append(errs, fmt.Sprintf("providers.%s.type: unsupported %q (available: %s)", name, b, strings.Join(Backends, ", ")))
		}
	}
	if _, err := ParseBytes(f.MachineMemory); err != nil {
		errs = append(errs, "pool.machine_memory: "+err.Error())
	}
	for name, v := range map[string]string{"idle_stop": f.IdleStop, "max_lifetime": f.MaxLifetime, "queue_timeout": f.QueueTimeout} {
		if _, err := ParseDuration(v); err != nil {
			errs = append(errs, "pool."+name+": "+err.Error())
		}
	}
	if f.Policy != "default" && f.Policy != "binpack" && f.Policy != "classic" {
		errs = append(errs, fmt.Sprintf("pool.policy: unsupported %q (default, binpack, classic)", f.Policy))
	}
	if f.ConsolidateBelow != nil && (*f.ConsolidateBelow < 0 || *f.ConsolidateBelow > 1) {
		errs = append(errs, "pool.consolidate_below: must be between 0 (off) and 1")
	}
	if f.ConsolidateTarget < 0 || f.ConsolidateTarget > 1 {
		errs = append(errs, "pool.consolidate_target: must be between 0 and 1")
	}
	for i, w := range f.WarmPool {
		if _, _, _, _, err := w.Parse(); err != nil {
			errs = append(errs, fmt.Sprintf("pool.warm_pool[%d]: %v", i, err))
		}
		if w.Machines < 0 || w.Machines > f.MaxMachines {
			errs = append(errs, fmt.Sprintf("pool.warm_pool[%d].machines: need 0 <= machines <= max_machines", i))
		}
	}
	if f.Estimator != "history" && f.Estimator != "static" {
		errs = append(errs, fmt.Sprintf("pool.estimator: unsupported %q (history, static)", f.Estimator))
	}
	if f.CPUOvercommit < 1 || f.MemoryOvercommit < 1 {
		errs = append(errs, "pool: cpu_overcommit and memory_overcommit must be >= 1")
	}
	if f.MemoryOvercommit > 1 && !f.Swap && f.SoftLimit == 0 {
		errs = append(errs, "pool.memory_overcommit > 1 needs swap = true or soft_limit, otherwise sessions are OOM killed instead of slowed down")
	}
	for i, sz := range f.Sizes {
		if sz.Name == "" || sz.CPU <= 0 {
			errs = append(errs, fmt.Sprintf("pool.size[%d]: name and cpu > 0 required", i))
		}
		if _, err := ParseBytes(sz.Memory); err != nil {
			errs = append(errs, fmt.Sprintf("pool.size[%d].memory: %v", i, err))
		}
		if _, ok := u.Providers[sz.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("pool.size[%d].provider: %q is not a configured provider (%s)", i, sz.Provider, strings.Join(u.ProviderNames(), ", ")))
		}
		if sz.Arch != "" && sz.Arch != ArchAMD64 && sz.Arch != ArchARM64 {
			errs = append(errs, fmt.Sprintf("pool.size[%d].arch: unsupported %q (amd64, arm64)", i, sz.Arch))
		}
		if d := sz.SpotDiscount; d != nil && (*d < 0 || *d >= 1) {
			errs = append(errs, fmt.Sprintf("pool.size[%d].spot_discount: must be in [0, 1)", i))
		}
		if b := u.ProviderType(sz.Provider); b != "" && !cloudBackend(b) {
			if sz.Spot || len(sz.InstanceTypes) > 0 || sz.SpotDiscount != nil || sz.SpotFallback != nil {
				errs = append(errs, fmt.Sprintf("pool.size[%d]: spot, spot_discount, spot_fallback and instance_types need an aws-ec2 or oci provider (%s is %s)", i, sz.Provider, b))
			}
			if a := MachineArch(sz.Arch); b == "local-docker" && a != MachineArch(runtime.GOARCH) {
				errs = append(errs, fmt.Sprintf("pool.size[%d].arch: local-docker machines run on this host (%s), not %s", i, runtime.GOARCH, a))
			}
		}
	}
	if f.MinMachines < 0 || f.MaxMachines < 1 || f.MinMachines > f.MaxMachines {
		errs = append(errs, "pool: need 0 <= min_machines <= max_machines and max_machines >= 1")
	}
	errs = append(errs, u.Telemetry.validate()...)
	if len(errs) > 0 {
		return fmt.Errorf("invalid %s:\n  %s", u.displayPath(), strings.Join(errs, "\n  "))
	}
	return nil
}

// Durations returns the parsed pool durations.
func (f Pool) Durations() (idle, lifetime, queue time.Duration) {
	idle, _ = ParseDuration(f.IdleStop)
	lifetime, _ = ParseDuration(f.MaxLifetime)
	queue, _ = ParseDuration(f.QueueTimeout)
	return
}

// sanitizeID turns a user name into a valid pool id.
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

// Document renders the effective configuration in the file schema, with
// defaults applied.
func (u *PoolConfig) Document() (map[string]any, error) {
	section := func(v any) (map[string]any, error) {
		b, err := toml.Marshal(v)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		return m, toml.Unmarshal(b, &m)
	}
	pool, err := section(u.Pool)
	if err != nil {
		return nil, err
	}
	sched, err := section(u.Pool.Scheduler)
	if err != nil {
		return nil, err
	}
	tel, err := section(u.Telemetry)
	if err != nil {
		return nil, err
	}
	doc := map[string]any{"pool": pool, "scheduler": sched, "telemetry": tel}
	if len(u.Pool.Provision) > 0 {
		setup := make([]any, len(u.Pool.Provision))
		for i, p := range u.Pool.Provision {
			setup[i] = map[string]any{"run": p.Run}
		}
		doc["machine"] = map[string]any{"setup": setup}
	}
	providers := map[string]any{}
	for name, opts := range u.Providers {
		t := maps.Clone(opts)
		var sizes []any
		for _, sz := range u.Pool.Sizes {
			if sz.Provider == name {
				m, err := section(sz)
				if err != nil {
					return nil, err
				}
				sizes = append(sizes, m)
			}
		}
		if len(sizes) > 0 {
			t["size"] = sizes
		}
		providers[name] = t
	}
	doc["providers"] = providers
	return doc, nil
}

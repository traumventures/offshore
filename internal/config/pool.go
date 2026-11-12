package config

import (
	"fmt"
	"github.com/traumventures/offshore/internal/medium"
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
	// Volumes are the providers' cache volume settings
	// ([providers.NAME.volume]).
	Volumes map[string]VolumeConfig `toml:"-"`
	// Profiles override capability profiles of the providers' media
	// ([providers.NAME.media.MEDIUM]).
	Profiles map[string]map[string]ProfileConfig `toml:"-"`
	// Storage overrides the providers' native object stores.
	Storage Storage `toml:"storage"`
	// Telemetry configures tracing of the orchestrator (control plane or
	// embedded) and, forwarded, of its machines; and the event log.
	Telemetry Telemetry `toml:"telemetry"`
	// Writeback configures when workers commit build slots and write them
	// back to the store ([writeback]).
	Writeback Writeback `toml:"writeback"`
	// Warnings are ignored sections found while loading.
	Warnings []string `toml:"-"`
}

// Writeback configures build slot writeback on the workers: a slot is
// committed (and its pages written back to the store) once it is idle for
// dirty_expire (default "30s"), and right after a session that grew it by
// more than dirty_ratio of the machine's disk budget (default 0.1).
type Writeback struct {
	DirtyRatio  float64 `toml:"dirty_ratio"`
	DirtyExpire string  `toml:"dirty_expire"`
}

// Backends known to the configuration.
var Backends = []string{"local-docker", "aws-ec2", "oci", "static"}

// cloudBackend reports whether a provider type launches cloud instances
// with instance types (shapes).
func cloudBackend(t string) bool { return t == "aws-ec2" || t == "oci" }

// Pool configures machine provisioning and scaling.
type Pool struct {
	// ID names the pool: machines and volumes carry it in the
	// offshore.pool label, and an orchestrator only manages resources
	// of its own pool. Default: $USER.
	ID            string  `toml:"id"`
	MachineCPU    float64 `toml:"machine_cpu"`
	MachineMemory string  `toml:"machine_memory"`
	MinMachines   int     `toml:"min_machines"` // warm pool per recently used environment
	// MaxSpendPerHour is a circuit breaker: no machine is created or
	// started while the running and starting machines cost that much per
	// hour (0 = off). The pool has no machine count limit: the scheduler
	// bounds scale-out itself (docs/scheduling.md).
	MaxSpendPerHour float64 `toml:"max_spend_per_hour"`
	IdleStop        string  `toml:"idle_stop"`
	MaxLifetime     string  `toml:"max_lifetime"`
	QueueTimeout    string  `toml:"queue_timeout"`
	// Provision are the machine setup steps ([[machine.setup]]).
	Provision []ProvisionStep `toml:"-"`
	// Sizes are the machine types of all providers ([[providers.NAME.size]]).
	Sizes []Size `toml:"-"`

	// Scheduler holds the scheduling settings ([scheduler]).
	Scheduler `toml:"-"`
}

// Scheduler configures how sessions are placed on machines
// (docs/design-scheduler-v2.md). Overcommitting CPU only slows tasks down;
// overcommitting memory needs soft limits and swap, otherwise tasks may be
// OOM killed.
type Scheduler struct {
	// Policy is "default" (the only scheduler; the earlier policies kept
	// for comparison are gone).
	Policy string `toml:"policy"`
	// CostOptimization is the scheduler's knob, 0..1 (default 0.5): 1
	// optimises purely for cost, 0 purely for time to result.
	CostOptimization *float64 `toml:"cost_optimization"`
	// QueueConfidence is the quantile of the running sessions' remaining
	// times at which a wait for their capacity is scored: waiting wins
	// only when it finishes sooner with this probability (default 0.8,
	// 0 < x <= 1).
	QueueConfidence *float64 `toml:"queue_confidence"`
	// Egress are transfer prices and bandwidths from object stores to
	// providers ([[scheduler.egress]] from = "aws", to = "oci",
	// price_per_gb = 0.09, bandwidth = "50M" bytes/s).
	Egress []Egress `toml:"egress"`
	// DiskPricePerGBMonth prices extra disk of a resize (default 0.08).
	DiskPricePerGBMonth float64 `toml:"disk_price_per_gb_month"`
	// DiskBudget is how much of a machine's cache disk build slots and
	// machine caches may fill before the worker prunes: "90%" (default) of
	// the file system or a size ("150G"). Sizes may override it.
	DiskBudget string `toml:"disk_budget"`
	// SourceCache bounds the source contents a worker fetched from the
	// laptop and keeps as a cache (reclaimed least recently used beyond
	// it): a share of the disk budget ("12.5%", default) or a size ("20G").
	SourceCache       string  `toml:"source_cache"`
	Estimator         string  `toml:"estimator"` // "history" (learn from runs) | "static" (repo config)
	CPUOvercommit     float64 `toml:"cpu_overcommit"`
	MemoryOvercommit  float64 `toml:"memory_overcommit"`
	MaxCPUPressure    float64 `toml:"max_cpu_pressure"`    // 0..1, PSI some avg10; 0 = off
	MaxMemoryPressure float64 `toml:"max_memory_pressure"` // 0..1; 0 = off
	SoftLimit         float64 `toml:"soft_limit"`          // memory.high = need * soft_limit; 0 = off
	Swap              bool    `toml:"swap"`
	LimitJobs         bool    `toml:"limit_jobs"` // pass reserved CPU as build parallelism
	// WarmPool keeps machines running at scheduled times, in addition to
	// min_machines: [[scheduler.warm_pool]] days = ["mon", "tue"], from = "08:00",
	// to = "19:00", machines = 1, timezone = "Europe/Berlin".
	WarmPool []WarmPool `toml:"warm_pool"`
}

// Egress is the transfer price and bandwidth from a store to the machines
// of a provider. Stores are named like providers ("storage" for the pool's
// [storage] section); empty matches any.
type Egress struct {
	From       string  `toml:"from"`
	To         string  `toml:"to"`
	PricePerGB float64 `toml:"price_per_gb"`
	Bandwidth  string  `toml:"bandwidth"` // bytes per second, e.g. "100M"
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
	PricePerHour float64 `toml:"price_per_hour"` // hourly price
	// Disk is the machine disk ("200G"; empty = unknown). The scheduler
	// plans build slots against it (resize, prune, move).
	Disk string `toml:"disk,omitempty"`
	// DiskBudget overrides [scheduler] disk_budget for machines of this size.
	DiskBudget string `toml:"disk_budget,omitempty"`
	// Medium is where the machine's caches live: "local" (default; the
	// root disk, or the instance-store NVMe of instance types that have
	// one, whose size then is the disk) or "volume" (an attachable volume,
	// [providers.NAME.volume]).
	Medium string `toml:"medium,omitempty"`
	// Stack is the cache tier stack of the size, top (fastest) first, as
	// in anyfs: an ordered subset of "local", "volume", "store" ending
	// in "store", e.g. ["local", "volume", "store"] (the local disk caches
	// a cache volume over the object store). Empty: derived from medium
	// (["local", "store"] or ["volume", "store"]). A stack with "volume"
	// gives the size cache volumes (medium "volume"); the scheduler
	// estimates page-in, restore and thrashing from the composite profile.
	Stack []string `toml:"stack,omitempty"`
	// Arch is the machine architecture, "amd64" (default) or "arm64".
	// Environments are built per architecture; a repository pins its
	// architecture with resources.arch, and only sizes of that
	// architecture are created for it.
	Arch string `toml:"arch"`
	// InstanceTypes are the provider machine types that provide this size,
	// tried in order: a capacity error on one type moves on to the next
	// (aws-ec2 instance types, oci shapes; default: the size name on
	// aws-ec2, a flexible shape of the architecture on oci).
	InstanceTypes []string `toml:"instance_types"`
}

// TierStack returns the size's cache tier stack (top first): Stack, or
// derived from Medium.
func (sz Size) TierStack() []string {
	if len(sz.Stack) > 0 {
		return sz.Stack
	}
	if sz.Medium == MediumVolume {
		return []string{MediumVolume, medium.Store}
	}
	return []string{MediumLocal, medium.Store}
}

// HasVolume reports whether machines of the size get a cache volume
// (medium "volume" or a stack with "volume").
func (sz Size) HasVolume() bool {
	return slices.Contains(sz.TierStack(), MediumVolume)
}

// validateStack checks a size's stack: an ordered subset of local,
// volume, store ending in store, consistent with medium.
func (sz Size) validateStack() error {
	if len(sz.Stack) == 0 {
		return nil
	}
	order := map[string]int{MediumLocal: 0, MediumVolume: 1, medium.Store: 2}
	last := -1
	for _, t := range sz.Stack {
		o, ok := order[t]
		if !ok {
			return fmt.Errorf("unknown tier %q (local, volume, store)", t)
		}
		if o <= last {
			return fmt.Errorf("%q: tiers must be distinct, ordered local, volume, store", sz.Stack)
		}
		last = o
	}
	if sz.Stack[len(sz.Stack)-1] != medium.Store {
		return fmt.Errorf("%q: the bottom tier must be store", sz.Stack)
	}
	if len(sz.Stack) < 2 {
		return fmt.Errorf("%q: needs a tier above store", sz.Stack)
	}
	vol := slices.Contains(sz.Stack, MediumVolume)
	switch {
	case sz.Medium == MediumVolume && !vol:
		return fmt.Errorf("%q: medium volume needs volume in the stack", sz.Stack)
	case sz.Medium == MediumLocal && vol:
		return fmt.Errorf("%q: medium local conflicts with volume in the stack", sz.Stack)
	}
	return nil
}

// Machine architectures of sizes.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

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

// ProvisionStep is a machine level provisioning command, executed once per
// machine before it accepts sessions.
type ProvisionStep struct {
	Run string `toml:"run"`
}

// PoolConfigPath returns the pool configuration file:
// $OFFSHORE_POOL_CONFIG, else the client configuration file (embedded
// mode and single-host control planes keep both in one file).
func PoolConfigPath() string {
	if p := os.Getenv("OFFSHORE_POOL_CONFIG"); p != "" {
		return p
	}
	return ClientConfigPath()
}

// LoadPool loads the pool configuration from path, applying defaults when
// the file is absent. Client sections in the file are ignored (see Warnings).
//
// Schema:
//
//	[pool]                  id, machine_cpu/memory, min_machines, max_spend_per_hour, lifetimes
//	[scheduler]             policy, estimator, overcommit, pressure, consolidation
//	[[scheduler.warm_pool]] scheduled warm machines
//	[[machine.setup]]       run = "..." once per machine
//	[providers.NAME]        type = "local-docker" | "aws-ec2" | "oci" | "static", options
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
		f.Pool, f.Telemetry, f.Storage, f.Writeback = raw.Pool, raw.Telemetry, raw.Storage, raw.Writeback
		f.Pool.Scheduler = raw.Scheduler
		f.Pool.Provision = raw.Machine.Setup
		if f.Providers, f.Pool.Sizes, f.Volumes, f.Profiles, err = splitProviders(raw.Providers); err != nil {
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

// splitProviders separates the [[providers.NAME.size]],
// [providers.NAME.volume] and [providers.NAME.media.MEDIUM] tables from
// the provider options.
func splitProviders(in map[string]map[string]any) (map[string]map[string]any, []Size, map[string]VolumeConfig, map[string]map[string]ProfileConfig, error) {
	var sizes []Size
	out := map[string]map[string]any{}
	vols := map[string]VolumeConfig{}
	profs := map[string]map[string]ProfileConfig{}
	for _, name := range slices.Sorted(maps.Keys(in)) {
		opts := maps.Clone(in[name])
		var t struct {
			Size    []Size                   `toml:"size"`
			Volume  *VolumeConfig            `toml:"volume"`
			Profile map[string]ProfileConfig `toml:"media"`
		}
		sub := map[string]any{}
		for _, k := range []string{"size", "volume", "media"} {
			if raw, ok := opts[k]; ok {
				delete(opts, k)
				sub[k] = raw
			}
		}
		if len(sub) > 0 {
			data, err := toml.Marshal(sub)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("providers.%s: %w", name, err)
			}
			if err := decodeStrict(data, &t); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("providers.%s: %w", name, err)
			}
		}
		for _, sz := range t.Size {
			sz.Provider = name
			sizes = append(sizes, sz)
		}
		if t.Volume != nil {
			vols[name] = *t.Volume
		}
		if len(t.Profile) > 0 {
			profs[name] = t.Profile
		}
		out[name] = opts
	}
	if len(out) == 0 {
		out = nil
	}
	return out, sizes, vols, profs, nil
}

func (u *PoolConfig) applyDefaults() {
	f := &u.Pool
	if f.ID == "" {
		f.ID = sanitizeID(defaultUser())
	}
	if u.Storage.Configured() && u.Storage.Prefix == "" {
		u.Storage.Prefix = "cache/" + f.ID
	}
	if f.MachineCPU == 0 {
		f.MachineCPU = 8
	}
	if f.MachineMemory == "" {
		f.MachineMemory = "16G"
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
	if f.Estimator == "" {
		f.Estimator = "history"
	}
	if f.CostOptimization == nil {
		v := 0.5
		f.CostOptimization = &v
	}
	if len(u.Providers) == 0 {
		// Without [providers]: one local-docker provider on this host.
		cmd := []any{"docker"}
		if c := os.Getenv("OFFSHORE_DOCKER"); c != "" {
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
	errs = u.Storage.validate(errs)
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
		if v == "" && name == "idle_stop" {
			continue // unset: derived by the scheduler's lifecycle policy
		}
		if _, err := ParseDuration(v); err != nil {
			errs = append(errs, "pool."+name+": "+err.Error())
		}
	}
	if f.CostOptimization != nil && (*f.CostOptimization < 0 || *f.CostOptimization > 1) {
		errs = append(errs, "scheduler.cost_optimization: must be between 0 (fastest result) and 1 (cheapest)")
	}
	if f.QueueConfidence != nil && (*f.QueueConfidence <= 0 || *f.QueueConfidence > 1) {
		errs = append(errs, "scheduler.queue_confidence: must be above 0 and at most 1")
	}
	for i, e := range f.Egress {
		if e.PricePerGB < 0 {
			errs = append(errs, fmt.Sprintf("scheduler.egress[%d].price_per_gb: must be >= 0", i))
		}
		if e.Bandwidth != "" {
			if _, err := ParseBytes(e.Bandwidth); err != nil {
				errs = append(errs, fmt.Sprintf("scheduler.egress[%d].bandwidth: %v", i, err))
			}
		}
	}
	if f.DiskBudget != "" {
		if _, err := ParseDiskBudget(f.DiskBudget); err != nil {
			errs = append(errs, "scheduler.disk_budget: "+err.Error())
		}
	}
	if f.SourceCache != "" {
		if _, err := ParseSourceCache(f.SourceCache); err != nil {
			errs = append(errs, "scheduler.source_cache: "+err.Error())
		}
	}
	if r := u.Writeback.DirtyRatio; r < 0 || r > 1 {
		errs = append(errs, "writeback.dirty_ratio: must be between 0 (default) and 1")
	}
	if e := u.Writeback.DirtyExpire; e != "" {
		if d, err := ParseDuration(e); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("writeback.dirty_expire: invalid duration %q", e))
		}
	}
	for i, w := range f.WarmPool {
		if _, _, _, _, err := w.Parse(); err != nil {
			errs = append(errs, fmt.Sprintf("pool.warm_pool[%d]: %v", i, err))
		}
		if w.Machines < 0 {
			errs = append(errs, fmt.Sprintf("pool.warm_pool[%d].machines: need machines >= 0", i))
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
		if sz.Disk != "" {
			if _, err := ParseBytes(sz.Disk); err != nil {
				errs = append(errs, fmt.Sprintf("pool.size[%d].disk: %v", i, err))
			}
		}
		if sz.DiskBudget != "" {
			if _, err := ParseDiskBudget(sz.DiskBudget); err != nil {
				errs = append(errs, fmt.Sprintf("pool.size[%d].disk_budget: %v", i, err))
			}
		}
		switch sz.Medium {
		case "", MediumLocal:
		case MediumVolume:
		default:
			errs = append(errs, fmt.Sprintf("pool.size[%d].medium: unsupported %q (local, volume)", i, sz.Medium))
		}
		if err := sz.validateStack(); err != nil {
			errs = append(errs, fmt.Sprintf("pool.size[%d].stack: %v", i, err))
		}
		if sz.HasVolume() {
			if b := u.ProviderType(sz.Provider); b != "" && !volumeBackend(b) {
				errs = append(errs, fmt.Sprintf("pool.size[%d].medium: %s (%s) has no volumes", i, sz.Provider, b))
			}
		}
		if _, ok := u.Providers[sz.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("pool.size[%d].provider: %q is not a configured provider (%s)", i, sz.Provider, strings.Join(u.ProviderNames(), ", ")))
		}
		if sz.Arch != "" && sz.Arch != ArchAMD64 && sz.Arch != ArchARM64 {
			errs = append(errs, fmt.Sprintf("pool.size[%d].arch: unsupported %q (amd64, arm64)", i, sz.Arch))
		}
		if b := u.ProviderType(sz.Provider); b != "" && !cloudBackend(b) {
			if len(sz.InstanceTypes) > 0 {
				errs = append(errs, fmt.Sprintf("pool.size[%d]: instance_types needs an aws-ec2 or oci provider (%s is %s)", i, sz.Provider, b))
			}
			if a := MachineArch(sz.Arch); b == "local-docker" && a != MachineArch(runtime.GOARCH) {
				errs = append(errs, fmt.Sprintf("pool.size[%d].arch: local-docker machines run on this host (%s), not %s", i, runtime.GOARCH, a))
			}
		}
	}
	for _, name := range u.ProviderNames() {
		if _, ok := u.Providers[name]["max_machines"]; ok {
			errs = append(errs, fmt.Sprintf("providers.%s.max_machines: no longer supported: machine counts are not configured (the scheduler bounds scale-out, the account's quotas are discovered); disable a provider with enabled = false", name))
		}
		if v, ok := u.Providers[name]["enabled"]; ok {
			if _, isBool := v.(bool); !isBool {
				errs = append(errs, fmt.Sprintf("providers.%s.enabled: need true or false, got %v", name, v))
			}
		}
	}
	if f.Policy != "" && f.Policy != "default" {
		errs = append(errs, fmt.Sprintf("scheduler.policy: %q is no longer supported (only \"default\"; see docs/design-scheduler-v2.md)", f.Policy))
	}
	if f.MinMachines < 0 {
		errs = append(errs, "pool.min_machines: need >= 0")
	}
	if f.MaxSpendPerHour < 0 {
		errs = append(errs, "pool.max_spend_per_hour: need >= 0 (0 = off)")
	}
	for _, name := range slices.Sorted(maps.Keys(u.Volumes)) {
		prefix := "providers." + name + ".volume"
		if _, ok := u.Providers[name]; !ok {
			continue
		}
		if b := u.ProviderType(name); b != "" && !volumeBackend(b) {
			errs = append(errs, fmt.Sprintf("%s: %s providers have no volumes", prefix, b))
		}
		errs = append(errs, u.Volumes[name].validate(prefix)...)
	}
	for _, name := range slices.Sorted(maps.Keys(u.Profiles)) {
		for _, med := range slices.Sorted(maps.Keys(u.Profiles[name])) {
			prefix := "providers." + name + ".media." + med
			if med != MediumLocal && med != MediumVolume && med != medium.Store {
				errs = append(errs, fmt.Sprintf("%s: unknown medium (local, volume, store)", prefix))
			}
			errs = append(errs, u.Profiles[name][med].validate(prefix)...)
		}
	}
	errs = append(errs, u.Telemetry.validate()...)
	if len(errs) > 0 {
		return fmt.Errorf("invalid %s:\n  %s", u.displayPath(), strings.Join(errs, "\n  "))
	}
	return nil
}

// Durations returns the parsed pool durations.
func (f Pool) Durations() (idle, lifetime, queue time.Duration) {
	if f.IdleStop != "" {
		idle, _ = ParseDuration(f.IdleStop)
	}
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
	if u.Writeback != (Writeback{}) {
		wb, err := section(u.Writeback)
		if err != nil {
			return nil, err
		}
		doc["writeback"] = wb
	}
	if u.Storage.Configured() {
		st, err := section(u.Storage)
		if err != nil {
			return nil, err
		}
		doc["storage"] = st
	}
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

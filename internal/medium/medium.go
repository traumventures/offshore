// Package medium describes where a machine's caches live and what each
// place can do: a capability profile per medium (local disk, attachable
// volume, object store) and provider, and the stack of media of one
// machine, the input of the scheduler's cost model.
//
// The three tiers:
//
//   - local: the machine's own disk (root disk or instance-store NVMe).
//     Fastest; dies with the machine. Moving it means a snapshot to the
//     store and a lazy restore.
//   - volume: an attachable network block volume (EBS, OCI block volume).
//     Survives the machine; moves by detach/attach within its zone.
//   - store: the S3-compatible object store. Not a live medium but the
//     durable, unlimited backing store of snapshots and global result
//     caches, reachable from everywhere.
//
// Profiles have approximate code defaults (see defaults.go, with the
// provider documentation they come from), can be overridden in the pool
// configuration ([providers.NAME.profile.MEDIUM]) and by measured values
// (SetMeasured).
package medium

import (
	"sync"
	"time"
)

// Media.
const (
	Local  = "local"
	Volume = "volume"
	Store  = "store"
)

// Reach is how far data on a medium can be used without copying it.
type Reach int

const (
	ReachMachine Reach = iota // only the machine it is on
	ReachZone                 // machines in the same zone (attach)
	ReachRegion               // machines in the region (free transfer)
	ReachGlobal               // everywhere (transfer, maybe egress)
)

func (r Reach) String() string { return [...]string{"machine", "zone", "region", "global"}[r] }

// Profile is the capability profile of one medium of one provider.
// Zero numeric fields mean unknown/none.
type Profile struct {
	Medium string `json:"medium"`
	// Throughput is the sustained sequential throughput in bytes/s.
	Throughput int64         `json:"throughput,omitempty"`
	IOPS       int           `json:"iops,omitempty"`
	Latency    time.Duration `json:"latency,omitempty"` // per small request
	// Reach: where the data is usable without a copy. Durability: what
	// the data survives (ReachMachine: nothing beyond the machine).
	Reach      Reach `json:"reach"`
	Durability Reach `json:"durability"`
	// Prices (currency of the pool's price_per_hour).
	PricePerGBMonth float64 `json:"price_per_gb_month,omitempty"`
	RequestPer1k    float64 `json:"request_per_1k,omitempty"` // per 1000 requests (store)
	EgressPerGB     float64 `json:"egress_per_gb,omitempty"`  // leaving the provider
	// Attach is the time to make existing data available to another
	// machine within reach (volume attach); Provision the time to create
	// empty capacity.
	Attach    time.Duration `json:"attach,omitempty"`
	Provision time.Duration `json:"provision,omitempty"`
	// Limits.
	MinBytes       int64         `json:"min_bytes,omitempty"`
	MaxBytes       int64         `json:"max_bytes,omitempty"`
	ResizeCooldown time.Duration `json:"resize_cooldown,omitempty"`
	Resizable      bool          `json:"resizable,omitempty"`
	// Source says where the values come from: "default" (approximate code
	// defaults), "config" or "measured" (the last layer applied).
	Source string `json:"source,omitempty"`
}

// Merge overlays the non-zero fields of o on p (Medium, Reach and
// Durability are intrinsic and kept unless p has none).
func (p Profile) Merge(o Profile, source string) Profile {
	if p.Medium == "" {
		p.Medium, p.Reach, p.Durability = o.Medium, o.Reach, o.Durability
	}
	if o.Throughput > 0 {
		p.Throughput = o.Throughput
	}
	if o.IOPS > 0 {
		p.IOPS = o.IOPS
	}
	if o.Latency > 0 {
		p.Latency = o.Latency
	}
	if o.PricePerGBMonth > 0 {
		p.PricePerGBMonth = o.PricePerGBMonth
	}
	if o.RequestPer1k > 0 {
		p.RequestPer1k = o.RequestPer1k
	}
	if o.EgressPerGB > 0 {
		p.EgressPerGB = o.EgressPerGB
	}
	if o.Attach > 0 {
		p.Attach = o.Attach
	}
	if o.Provision > 0 {
		p.Provision = o.Provision
	}
	if o.MinBytes > 0 {
		p.MinBytes = o.MinBytes
	}
	if o.MaxBytes > 0 {
		p.MaxBytes = o.MaxBytes
	}
	if o.ResizeCooldown > 0 {
		p.ResizeCooldown = o.ResizeCooldown
	}
	if o.Resizable {
		p.Resizable = true
	}
	if source != "" {
		p.Source = source
	}
	return p
}

// PricePerGBHour is the storage price per GB and hour.
func (p Profile) PricePerGBHour() float64 { return p.PricePerGBMonth / HoursPerMonth }

// HoursPerMonth is the month of the GB-month prices.
const HoursPerMonth = 730

// TransferTime is the time to move bytes at the profile's throughput
// (0 when unknown).
func (p Profile) TransferTime(bytes int64) time.Duration {
	if p.Throughput <= 0 || bytes <= 0 {
		return 0
	}
	return time.Duration(float64(bytes) / float64(p.Throughput) * float64(time.Second))
}

// Stack is the composite profile of a machine: the hot medium builds read
// and write (local disk or a volume) over the durable store.
type Stack struct {
	Hot   Profile `json:"hot"`
	Store Profile `json:"store"`
}

// Medium is the hot medium's name (default local).
func (s Stack) Medium() string {
	if s.Hot.Medium == "" {
		return Local
	}
	return s.Hot.Medium
}

// RestoreTime is the time to fill bytes from the store into the hot
// medium: bounded by the slower of both.
func (s Stack) RestoreTime(bytes int64) time.Duration {
	return max(s.Store.TransferTime(bytes), s.Hot.TransferTime(bytes))
}

// Measured returns a measured profile for a provider instance and medium
// (ok false: none). Measurements override defaults and configuration.
type Measured func(provider, medium string) (Profile, bool)

var (
	measuredMu sync.RWMutex
	measured   Measured
)

// SetMeasured installs the source of measured profiles (nil: none).
func SetMeasured(m Measured) {
	measuredMu.Lock()
	measured = m
	measuredMu.Unlock()
}

// Resolve returns the profile of a medium for a provider instance (name)
// of a provider type: the code defaults, the configured override, then a
// measurement.
func Resolve(providerType, name, med string, override Profile) Profile {
	p := Default(providerType, med)
	if override != (Profile{}) {
		p = p.Merge(override, "config")
	}
	measuredMu.RLock()
	m := measured
	measuredMu.RUnlock()
	if m != nil {
		if mp, ok := m(name, med); ok {
			p = p.Merge(mp, "measured")
		}
	}
	return p
}

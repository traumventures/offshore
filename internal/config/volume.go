package config

import (
	"fmt"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// Cache media of sizes (Size.Medium).
const (
	MediumLocal  = medium.Local
	MediumVolume = medium.Volume
)

// VolumeConfig is [providers.NAME.volume]: the attachable cache volumes of
// sizes with medium = "volume".
type VolumeConfig struct {
	Type       string `toml:"type"`       // aws-ec2 volume type (default gp3)
	IOPS       int    `toml:"iops"`       // aws-ec2 provisioned IOPS (gp3: 3000..16000)
	Throughput int    `toml:"throughput"` // aws-ec2 MiB/s (gp3: 125..1000)
	VPUs       int    `toml:"vpus"`       // oci performance units per GB (0, 10, 20, ...)
	// Min is the size of a new volume (default: the size's disk, else
	// 100G); Max the largest it grows to; Step the least it grows by.
	Min  string `toml:"min"`
	Max  string `toml:"max"`
	Step string `toml:"step"`
	// KeepDetached keeps a machine's volume when the machine is destroyed
	// so that a later machine in the same zone reuses its warm cache
	// (default false: deleted with the machine).
	KeepDetached bool `toml:"keep_detached"`
	// TTL: a detached volume is deleted after this long, and content older
	// than it is wiped when a volume is attached (default 24h).
	TTL string `toml:"ttl"`
}

// Volume defaults.
const (
	DefaultVolumeMin  = "100G"
	DefaultVolumeStep = "50G"
	DefaultVolumeTTL  = "24h"
)

// VolumeSizes returns min, max (0: provider limit), step and ttl with
// defaults applied; disk is the size's disk (0: unknown).
func (v VolumeConfig) VolumeSizes(disk int64) (minB, maxB, step int64, ttl time.Duration) {
	minB, _ = ParseBytes(DefaultVolumeMin)
	if disk > 0 {
		minB = disk
	}
	if v.Min != "" {
		minB, _ = ParseBytes(v.Min)
	}
	if v.Max != "" {
		maxB, _ = ParseBytes(v.Max)
	}
	step, _ = ParseBytes(DefaultVolumeStep)
	if v.Step != "" {
		step, _ = ParseBytes(v.Step)
	}
	ttl, _ = ParseDuration(DefaultVolumeTTL)
	if v.TTL != "" {
		ttl, _ = ParseDuration(v.TTL)
	}
	return
}

func (v VolumeConfig) validate(prefix string) []string {
	var errs []string
	for k, s := range map[string]string{"min": v.Min, "max": v.Max, "step": v.Step} {
		if s == "" {
			continue
		}
		if n, err := ParseBytes(s); err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("%s.%s: invalid size %q", prefix, k, s))
		}
	}
	if v.TTL != "" {
		if d, err := ParseDuration(v.TTL); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("%s.ttl: invalid duration %q", prefix, v.TTL))
		}
	}
	if v.IOPS < 0 || v.Throughput < 0 || v.VPUs < 0 {
		errs = append(errs, prefix+": iops, throughput and vpus must be >= 0")
	}
	minB, maxB, _, _ := v.VolumeSizes(0)
	if maxB > 0 && maxB < minB {
		errs = append(errs, prefix+": max is smaller than min")
	}
	return errs
}

// ProfileConfig is [providers.NAME.media.MEDIUM] (MEDIUM = local,
// volume or store): overrides of the medium's capability profile defaults.
type ProfileConfig struct {
	Throughput      string  `toml:"throughput"` // per second, e.g. "500M"
	IOPS            int     `toml:"iops"`
	Latency         string  `toml:"latency"` // e.g. "1ms"
	PricePerGBMonth float64 `toml:"price_per_gb_month"`
	RequestPer1k    float64 `toml:"request_per_1k"`
	EgressPerGB     float64 `toml:"egress_per_gb"`
	Attach          string  `toml:"attach"`
	Provision       string  `toml:"provision"`
	MaxSize         string  `toml:"max_size"`
	ResizeCooldown  string  `toml:"resize_cooldown"`
}

// Profile converts the override (invalid values are rejected by Validate).
func (p ProfileConfig) Profile() medium.Profile {
	var out medium.Profile
	if p.Throughput != "" {
		out.Throughput, _ = ParseBytes(p.Throughput)
	}
	out.IOPS = p.IOPS
	dur := func(s string) time.Duration {
		d, _ := ParseDuration(s)
		if s == "" {
			return 0
		}
		return d
	}
	out.Latency, out.Attach, out.Provision, out.ResizeCooldown = dur(p.Latency), dur(p.Attach), dur(p.Provision), dur(p.ResizeCooldown)
	out.PricePerGBMonth, out.RequestPer1k, out.EgressPerGB = p.PricePerGBMonth, p.RequestPer1k, p.EgressPerGB
	if p.MaxSize != "" {
		out.MaxBytes, _ = ParseBytes(p.MaxSize)
	}
	return out
}

func (p ProfileConfig) validate(prefix string) []string {
	var errs []string
	for k, s := range map[string]string{"throughput": p.Throughput, "max_size": p.MaxSize} {
		if s != "" {
			if _, err := ParseBytes(s); err != nil {
				errs = append(errs, fmt.Sprintf("%s.%s: %v", prefix, k, err))
			}
		}
	}
	for k, s := range map[string]string{"latency": p.Latency, "attach": p.Attach, "provision": p.Provision, "resize_cooldown": p.ResizeCooldown} {
		if s != "" {
			if _, err := ParseDuration(s); err != nil {
				errs = append(errs, fmt.Sprintf("%s.%s: %v", prefix, k, err))
			}
		}
	}
	if p.IOPS < 0 || p.PricePerGBMonth < 0 || p.RequestPer1k < 0 || p.EgressPerGB < 0 {
		errs = append(errs, prefix+": values must be >= 0")
	}
	return errs
}

// volumeBackend reports whether a provider type has attachable volumes.
func volumeBackend(t string) bool { return t == "aws-ec2" || t == "oci" || t == "local-docker" }

// Profile returns the capability profile of a medium (Local, Volume,
// Store, or medium.LocalNVMe) of a provider instance: defaults, the
// configured override, measurements.
func (u *PoolConfig) Profile(provider, med string) medium.Profile {
	key := med
	if med == medium.LocalNVMe {
		key = medium.Local
	}
	return medium.ResolveWith(u.ProviderType(provider), provider, med, u.rootDisk(provider, med), u.Profiles[provider][key].Profile())
}

// rootDisk adapts the default profile of an aws-ec2 gp3 root volume
// (medium local) to the provider's disk_gb, disk_iops and disk_throughput
// options: its numbers and its price per GB-month including the
// provisioned performance above the gp3 baseline, pro rata over the size,
// which a stopped machine keeps paying. Other media and providers: nil.
func (u *PoolConfig) rootDisk(provider, med string) func(medium.Profile) medium.Profile {
	if med != medium.Local || u.ProviderType(provider) != "aws-ec2" {
		return nil
	}
	opts := u.Providers[provider]
	gb := intOption(opts, "disk_gb", medium.AWSRootDiskGB)
	iops := intOption(opts, "disk_iops", medium.AWSRootDiskIOPS)
	tp := intOption(opts, "disk_throughput", medium.AWSRootDiskThroughput)
	return func(p medium.Profile) medium.Profile { return medium.GP3Profile(p, gb, iops, tp) }
}

// intOption returns an integer provider option (TOML integers, JSON
// numbers) or def.
func intOption(opts map[string]any, key string, def int64) int64 {
	switch v := opts[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return def
}

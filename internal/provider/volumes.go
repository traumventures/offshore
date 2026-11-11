package provider

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Volume kinds (LabelKind of a volume).
const (
	// VolumeCache is an attachable block volume holding a machine's cache
	// (build slots, machine caches).
	VolumeCache = "cache"
	// VolumeDeps is local-docker's shared dependency cache volume.
	VolumeDeps = "deps"
)

// Volume labels besides the common ones (pool, kind, created/expires at).
const (
	// LabelVolumeName is the volume's offshore name (providers that name
	// resources by id keep it as a tag).
	LabelVolumeName = "offshore.volume"
	// LabelResizedAt is when the volume was last resized (unix seconds).
	LabelResizedAt = "offshore.resized-at"
)

// VolumeState is the state of a volume.
type VolumeState string

const (
	VolumeCreating  VolumeState = "creating"
	VolumeAvailable VolumeState = "available" // detached, can be attached
	VolumeAttached  VolumeState = "attached"
	VolumeBusy      VolumeState = "busy" // attaching, detaching, being modified or deleted
	VolumeFailed    VolumeState = "failed"
)

// VolumeSpec describes a volume to create.
type VolumeSpec struct {
	Owner string // pool id (LabelPool)
	Name  string // offshore name, unique in the pool
	Kind  string // VolumeCache (default)
	// Zone is where the volume lives (aws-ec2 availability zone, oci
	// availability domain; empty: the provider's default zone). A volume
	// attaches only to machines in its zone.
	Zone  string
	Bytes int64
	// Performance, provider specific (zero: provider default):
	Type       string // aws-ec2 volume type (default gp3)
	IOPS       int    // aws-ec2 provisioned IOPS
	Throughput int    // aws-ec2 throughput in MiB/s
	VPUs       int    // oci volume performance units per GB (0, 10, 20, ...)
	ExpiresAt  time.Time
	Labels     map[string]string // extra labels
}

// Volume is a volume known to a provider. ID is the provider's handle
// for all volume operations.
type Volume struct {
	ID        string
	Name      string
	Kind      string
	Zone      string
	Bytes     int64
	State     VolumeState
	Machine   string // offshore id of the machine it is attached to (empty: none)
	Device    string // device path on the attached machine, if known
	CreatedAt time.Time
	ExpiresAt time.Time // zero: no expiry
	ResizedAt time.Time // last resize (zero: never)
	Labels    map[string]string
}

// Expired reports whether v has an expiry at or before now.
func (v Volume) Expired(now time.Time) bool {
	return !v.ExpiresAt.IsZero() && !now.Before(v.ExpiresAt)
}

// VolumeCaps describes what a provider's volumes support.
type VolumeCaps struct {
	MinBytes int64 // smallest volume (0: no minimum)
	MaxBytes int64 // largest volume (0: unlimited)
	// ResizeCooldown is the minimum time between two resizes of one
	// volume (aws-ec2: 6h).
	ResizeCooldown time.Duration
	// HotAttach: volumes attach to and detach from existing machines.
	// Without it a volume attaches only at machine creation and detaches
	// when the machine is destroyed.
	HotAttach bool
	// Zonal: a volume attaches only to machines in its zone.
	Zonal bool
	// Bounded: the volume has a fixed size that resizes change; unbounded
	// volumes (local directories) grow with their content.
	Bounded bool
}

// Volumes is the block volume part of a provider (see Parts).
type Volumes interface {
	VolumeCaps() VolumeCaps
	// CreateVolume creates a detached volume.
	CreateVolume(ctx context.Context, spec VolumeSpec) (Volume, error)
	// AttachVolume attaches an available volume to a machine in its zone
	// and clears its expiry.
	AttachVolume(ctx context.Context, id, machineID string) (Volume, error)
	// DetachVolume detaches a volume (no-op if it is detached) and sets its
	// expiry (zero: none). The machine must have stopped using it.
	DetachVolume(ctx context.Context, id string, expiresAt time.Time) error
	// ResizeVolume grows a volume to bytes (never shrinks); the machine
	// grows its file system.
	ResizeVolume(ctx context.Context, id string, bytes int64) error
	// DeleteVolume deletes a detached volume.
	DeleteVolume(ctx context.Context, id string) error
	// ListVolumes returns the volumes of owner (all kinds).
	ListVolumes(ctx context.Context, owner string) ([]Volume, error)
}

// Volume errors.
var (
	ErrVolumeInUse    = errors.New("volume in use")
	ErrVolumeNotFound = errors.New("volume not found")
	ErrNoHotAttach    = errors.New("volumes attach only at machine creation")
	ErrResizeCooldown = errors.New("volume was resized too recently")
	ErrVolumeShrink   = errors.New("volumes never shrink")
	ErrVolumeZone     = errors.New("volume and machine are in different zones")
	ErrVolumeTooLarge = errors.New("volume larger than the provider allows")
)

// CheckResize validates a resize of v to bytes against caps at now.
func CheckResize(caps VolumeCaps, v Volume, bytes int64, now time.Time) error {
	if !caps.Bounded {
		return nil
	}
	if bytes <= v.Bytes {
		return ErrVolumeShrink
	}
	if caps.MaxBytes > 0 && bytes > caps.MaxBytes {
		return ErrVolumeTooLarge
	}
	if caps.ResizeCooldown > 0 && !v.ResizedAt.IsZero() && now.Before(v.ResizedAt.Add(caps.ResizeCooldown)) {
		return ErrResizeCooldown
	}
	return nil
}

// VolumeLabels returns the labels of a new volume.
func VolumeLabels(spec VolumeSpec, now time.Time) map[string]string {
	l := map[string]string{}
	for k, v := range spec.Labels {
		l[k] = v
	}
	kind := spec.Kind
	if kind == "" {
		kind = VolumeCache
	}
	l[LabelPool] = spec.Owner
	l[LabelKind] = kind
	l[LabelVolumeName] = spec.Name
	l[LabelCreatedAt] = strconv.FormatInt(now.Unix(), 10)
	if !spec.ExpiresAt.IsZero() {
		l[LabelExpiresAt] = strconv.FormatInt(spec.ExpiresAt.Unix(), 10)
	}
	return l
}

// VolumeFromLabels fills the label derived fields of v (name, kind,
// times) from v.Labels.
func VolumeFromLabels(v *Volume) {
	l := v.Labels
	if v.Name == "" {
		v.Name = l[LabelVolumeName]
	}
	v.Kind = l[LabelKind]
	if t := unixLabel(l, LabelCreatedAt); !t.IsZero() {
		v.CreatedAt = t
	}
	v.ExpiresAt = unixLabel(l, LabelExpiresAt)
	v.ResizedAt = unixLabel(l, LabelResizedAt)
}

func unixLabel(l map[string]string, k string) time.Time {
	if n, err := strconv.ParseInt(l[k], 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// UnixLabel formats t as a label value (empty for the zero time).
func UnixLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.Unix(), 10)
}

// GiB rounds bytes up to whole GiB.
func GiB(bytes int64) int64 { return (bytes + 1<<30 - 1) >> 30 }

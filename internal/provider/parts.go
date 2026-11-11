package provider

import (
	"context"

	"github.com/traumventures/offshore/internal/store"
)

// A provider has four parts. Compute (machines) is the Provider interface
// itself; the others are optional interfaces a provider implements:
//
//   - Registry (ImageHost): where environment images are built and pulled
//     from;
//   - Store (StoreSource): its native S3-compatible object store for slot
//     snapshots and global caches, replaced for all providers by the pool's
//     [storage] section;
//   - Secrets (SecretChannel): how a new machine receives its secret
//     environment;
//   - Volumes: attachable block volumes (aws-ec2 EBS, oci block volumes,
//     local-docker volumes) that outlive machines.
type (
	Compute  = Provider
	Registry = ImageHost
)

// StoreSource is implemented by providers with a native object store.
type StoreSource interface {
	// NativeStore describes the store for the pool (machines' owner).
	// ok is false if the provider has no store workers can use.
	NativeStore(ctx context.Context, pool string) (c store.Config, ok bool, err error)
}

// StoreGranter is implemented by providers whose native store hands
// workers something other than S3 credentials (OCI: pre-authenticated
// request URLs per scope). It takes precedence over StoreSource; name is
// the provider instance (the store's name in grants and snapshots).
type StoreGranter interface {
	NativeGranter(ctx context.Context, name, pool string) (g store.Granter, ok bool, err error)
}

// SecretChannel delivers the secret environment of a machine.
type SecretChannel interface {
	PutSecrets(ctx context.Context, machineID string, env map[string]string) error
	DeleteSecrets(ctx context.Context, machineID string) error
}

// DiskResizer is implemented by providers that can grow a machine's cache
// disk in place.
type DiskResizer interface {
	ResizeDisk(ctx context.Context, id string, bytes int64) error
}

// LocalDisks is implemented by providers whose instance types may have
// instance-store disks (medium local caches live on them).
type LocalDisks interface {
	// InstanceStorage returns the instance-store capacity of an instance
	// type (0: none).
	InstanceStorage(ctx context.Context, instanceType string) (int64, error)
}

// Parts is a provider split into its parts (nil: not supported).
type Parts struct {
	Compute  Compute
	Registry Registry
	Store    StoreSource
	Secrets  SecretChannel
	Volumes  Volumes
}

// Split returns the parts of a provider.
func Split(p Provider) Parts {
	parts := Parts{Compute: p}
	parts.Registry, _ = p.(ImageHost)
	parts.Store, _ = p.(StoreSource)
	parts.Secrets, _ = p.(SecretChannel)
	parts.Volumes, _ = p.(Volumes)
	return parts
}

// StoreConfig returns the store machines of a provider use: the pool's
// [storage] override if set, otherwise the provider's native store.
func StoreConfig(ctx context.Context, p Provider, pool string, override *store.Config) (store.Config, bool, error) {
	if override != nil {
		return *override, true, nil
	}
	if s := Split(p).Store; s != nil {
		return s.NativeStore(ctx, pool)
	}
	return store.Config{}, false, nil
}

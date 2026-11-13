// Package store is the object store part of a provider: any S3-compatible
// store (the provider's native one, R2, MinIO, your own) or, for local
// pools, a directory shared by the machines.
//
// The control plane never moves cache data. It only hands workers grants:
// short-lived credentials scoped to a few key prefixes below the pool's
// prefix (STS AssumeRole with a session policy where a role is configured).
// Workers open the store from a grant and read and write directly.
//
// Objects are build caches: stores built here never serve objects older
// than their TTL, the pool's [storage] retention carried by the grant
// (default 24h, at most 7d, see blobcache.MaxTTL); the providers' bucket
// lifecycles delete them after the same number of days.
package store

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
)

// Store is an object store with expiring objects.
type Store = blobcache.Store

// Config describes a store.
type Config struct {
	// Name identifies the store in snapshots and egress prices: "storage"
	// for the pool's [storage] section, otherwise the provider's name.
	Name     string
	Endpoint string // S3 endpoint URL ("" = AWS for Region)
	Bucket   string
	Region   string
	Prefix   string // key prefix of the pool, e.g. "cache/<pool>"
	// AccessKeyID and SecretAccessKey are resolved credentials of the
	// control plane ("" = default AWS credential chain).
	AccessKeyID     string
	SecretAccessKey string
	// Profile is the shared AWS config profile of the control plane's
	// credentials when no keys are given ("" = default credential chain),
	// e.g. the aws-ec2 provider's profile.
	Profile string
	// RoleARN is assumed (STS) for scoped worker grants. Without it the
	// configured credentials themselves are handed to workers.
	RoleARN   string
	PathStyle bool // path-style addressing (MinIO)
	// Dir makes a directory store: the path where workers see the
	// directory shared by the pool's machines.
	Dir string
}

// Grant gives a worker access to some prefixes of a store.
type Grant struct {
	Store     string   `json:"store"`
	Endpoint  string   `json:"endpoint,omitempty"`
	Bucket    string   `json:"bucket,omitempty"`
	Region    string   `json:"region,omitempty"`
	Prefix    string   `json:"prefix,omitempty"` // pool prefix
	Scopes    []string `json:"scopes"`           // allowed prefixes relative to Prefix
	PathStyle bool     `json:"path_style,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	// Credentials (S3 stores).
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
	Expires         int64  `json:"expires,omitempty"` // unix seconds (0: never)
	// URLs are pre-authenticated URL prefixes per scope (OCI Object Storage
	// pre-authenticated requests): no credentials, each reaches only the
	// objects below its scope until Expires.
	URLs map[string]string `json:"urls,omitempty"`
	// RetentionSeconds is the pool's [storage] retention: the maximum age
	// of objects (0: blobcache.DefaultTTL). Stores opened from the grant
	// never serve older objects.
	RetentionSeconds int64 `json:"retention_seconds,omitempty"`
}

// Retention returns the grant's object retention (the store TTL).
func (g Grant) Retention() time.Duration {
	if g.RetentionSeconds <= 0 {
		return blobcache.DefaultTTL
	}
	return blobcache.ClampTTL(time.Duration(g.RetentionSeconds) * time.Second)
}

// RefreshAge is the age after which data held by the grant's store is
// written again instead of reused (half the retention): a chain
// referencing it stays restorable for at least the other half.
func (g Grant) RefreshAge() time.Duration { return g.Retention() / 2 }

// Expired reports whether the grant's credentials ended before now plus
// margin.
func (g Grant) Expired(now time.Time, margin time.Duration) bool {
	return g.Expires != 0 && !now.Add(margin).Before(time.Unix(g.Expires, 0))
}

// Allows reports whether scope is (below) one of the grant's scopes.
func (g Grant) Allows(scope string) bool {
	scope = cleanScope(scope)
	for _, s := range g.Scopes {
		if scope == s || strings.HasPrefix(scope, s+"/") {
			return true
		}
	}
	return false
}

// ErrScope reports a scope outside a grant.
var ErrScope = errors.New("store: scope not granted")

func cleanScope(s string) string { return strings.Trim(path.Clean("/"+s), "/") }

// Scope names used below a pool prefix.

// RepoScope holds a repository's snapshots and chunks.
func RepoScope(repoID string) string { return "repos/" + repoID }

// EnvScope holds the global result caches of an environment.
func EnvScope(envHash string) string { return "envs/" + envHash }

// Opener opens stores from grants (variable for tests).
var Opener = func(g Grant, scope string, ttl time.Duration) (Store, error) {
	root := strings.Trim(path.Join(g.Prefix, cleanScope(scope)), "/")
	if g.Dir != "" {
		return blobcache.NewDirStore(path.Join(g.Dir, root), ttl)
	}
	if len(g.URLs) > 0 {
		base, ok := urlBase(g, scope)
		if !ok {
			return nil, fmt.Errorf("%w: %q (no URL)", ErrScope, scope)
		}
		return newURLStore(base, ttl), nil
	}
	return newS3(context.Background(), g, root, ttl)
}

// Open opens the part of a store below scope (relative to the grant's
// prefix). ttl bounds the age of served objects; the grant's retention
// bounds it further (0 or blobcache.MaxTTL: the retention).
func Open(g Grant, scope string, ttl time.Duration) (Store, error) {
	if !g.Allows(scope) {
		return nil, fmt.Errorf("%w: %q (granted %v)", ErrScope, scope, g.Scopes)
	}
	if r := g.Retention(); ttl <= 0 || ttl > r {
		ttl = r
	}
	return Opener(g, scope, ttl)
}

// Granter mints grants.
type Granter interface {
	Name() string
	Grant(ctx context.Context, scopes []string, ttl time.Duration) (Grant, error)
}

// NewGranter returns the granter of a store configuration.
func NewGranter(ctx context.Context, c Config) (Granter, error) {
	if c.Name == "" {
		return nil, errors.New("store: name required")
	}
	if c.Dir != "" {
		return dirGranter{c}, nil
	}
	if c.Bucket == "" {
		return nil, fmt.Errorf("store %s: bucket required", c.Name)
	}
	return newS3Granter(ctx, c)
}

type dirGranter struct{ c Config }

func (d dirGranter) Name() string { return d.c.Name }

func (d dirGranter) Grant(_ context.Context, scopes []string, _ time.Duration) (Grant, error) {
	return Grant{Store: d.c.Name, Dir: d.c.Dir, Prefix: d.c.Prefix, Scopes: cleanScopes(scopes)}, nil
}

func cleanScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s = cleanScope(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

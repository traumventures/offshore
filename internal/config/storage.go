package config

import (
	"fmt"
	"strings"
	"time"
)

// Storage is the pool's [storage] section: an S3-compatible object store
// (R2, MinIO, AWS S3, your own) used instead of the providers' native
// stores for slot snapshots and global caches. Workers get short-lived
// credentials scoped to repository prefixes; with role_arn they are minted
// by STS AssumeRole, otherwise the configured keys are handed out.
type Storage struct {
	Endpoint string `toml:"endpoint"` // "" = AWS S3 in region
	Bucket   string `toml:"bucket"`
	Region   string `toml:"region"`
	Prefix   string `toml:"prefix"` // default "cache/<pool id>"
	// AccessKey and SecretKey are references ("env:VAR", "file:PATH",
	// "cmd:COMMAND") to the control plane's credentials.
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	RoleARN   string `toml:"role_arn"`
	PathStyle bool   `toml:"path_style"`
	// Dir is a directory store shared by the machines (a path the workers
	// see, e.g. a network file system mounted on static hosts).
	Dir string `toml:"dir"`
	// Retention is the maximum age of cache data in the object store
	// (the [storage] store or the providers' native ones): the store TTL,
	// the bucket lifecycle, the orchestrator's layer TTL and, at half of
	// it, the age after which active chains are rewritten. Default "24h",
	// allowed "24h".."7d". It may be set without a bucket.
	Retention string `toml:"retention"`
}

// Retention bounds of [storage] retention.
const (
	DefaultRetention = 24 * time.Hour
	MaxRetention     = 7 * 24 * time.Hour
)

// RetentionDuration returns the configured retention (DefaultRetention
// when unset or invalid; Validate reports invalid values).
func (s Storage) RetentionDuration() time.Duration {
	if s.Retention == "" {
		return DefaultRetention
	}
	d, err := ParseDuration(s.Retention)
	if err != nil || d < DefaultRetention || d > MaxRetention {
		return DefaultRetention
	}
	return d
}

// StorageName is the store name of the [storage] section.
const StorageName = "storage"

// Configured reports whether the section is set.
func (s Storage) Configured() bool { return s.Bucket != "" || s.Dir != "" }

func (s Storage) validate(errs []string) []string {
	if s.Retention != "" {
		d, err := ParseDuration(s.Retention)
		if err != nil || d < DefaultRetention || d > MaxRetention {
			errs = append(errs, fmt.Sprintf("storage.retention: %q must be between 24h and 7d", s.Retention))
		}
	}
	if !s.Configured() {
		if s != (Storage{Retention: s.Retention}) {
			errs = append(errs, "storage: bucket (or dir) required")
		}
		return errs
	}
	if s.Bucket != "" && s.Dir != "" {
		errs = append(errs, "storage: bucket and dir are exclusive")
	}
	for k, ref := range map[string]string{"access_key": s.AccessKey, "secret_key": s.SecretKey} {
		if ref == "" {
			continue
		}
		kind, _, ok := strings.Cut(ref, ":")
		if !ok || (kind != "env" && kind != "file" && kind != "cmd") {
			errs = append(errs, fmt.Sprintf("storage.%s: must be \"env:VAR\", \"file:PATH\" or \"cmd:COMMAND\"", k))
		}
	}
	if (s.AccessKey == "") != (s.SecretKey == "") {
		errs = append(errs, "storage: access_key and secret_key go together")
	}
	if s.Bucket != "" && s.RoleARN == "" && s.AccessKey == "" {
		errs = append(errs, "storage: role_arn or access_key/secret_key required (workers get scoped credentials)")
	}
	return errs
}

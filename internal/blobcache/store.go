// Package blobcache is a minimal WebDAV server for compiler caches.
//
// It implements exactly what sccache's WebDAV backend (the OpenDAL webdav
// service) uses: GET (with ranges), HEAD, PUT, DELETE, MKCOL and PROPFIND
// (depth 0 and 1) plus OPTIONS. Objects live in a Store; every store enforces
// a time to live of at most MaxTTL, measured from the time an object was
// written, and never serves older objects.
//
// URL layout: [/<namespace>]/[_ttl/<seconds>/]<key>. A Handler with a fixed
// Namespace (the worker's built-in cache: the namespace is the
// environment hash) takes no namespace segment from the URL, so sessions
// cannot reach other environments. A Handler without one (a supervisor
// serving several machines) takes the first path segment as the namespace.
// The optional _ttl segment lowers the maximum age for reads below the
// store's TTL (repository setting cache.compiler.ttl).
package blobcache

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// MaxTTL is the upper bound for the age of served objects.
const MaxTTL = 24 * time.Hour

// ErrNotFound reports a missing or expired object.
var ErrNotFound = errors.New("blobcache: not found")

// Info describes a stored object.
type Info struct {
	Size    int64
	ModTime time.Time // time the object was written
}

// Store holds cache objects by key. Keys are validated slash separated
// paths (see ValidKey). Implementations must not return objects older than
// their TTL.
type Store interface {
	// Get opens an object. If the returned reader implements io.Seeker,
	// range requests are served from it.
	Get(ctx context.Context, key string) (io.ReadCloser, Info, error)
	Stat(ctx context.Context, key string) (Info, error)
	// Put stores an object atomically. size is -1 when unknown.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Delete(ctx context.Context, key string) error
}

// ClampTTL returns ttl bounded to (0, MaxTTL]; zero or negative means MaxTTL.
func ClampTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 || ttl > MaxTTL {
		return MaxTTL
	}
	return ttl
}

// ValidKey reports whether key is a safe relative object path: non-empty
// segments of [A-Za-z0-9._-], none of them "." or "..".
func ValidKey(key string) bool {
	if key == "" || len(key) > 1024 {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if !validSegment(seg) {
			return false
		}
	}
	return true
}

func validSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." || len(seg) > 255 {
		return false
	}
	for _, r := range seg {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func expired(mod, now time.Time, ttl time.Duration) bool {
	return now.Sub(mod) > ttl
}

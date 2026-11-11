package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Cache is a repository cache ([cache.<name>]). Its scope decides where
// it lives and how long:
//
//   - "slot": worker-local directories inside the repository (e.g.
//     target/) in the session's exclusive build slot, reused by the next
//     session of the repository on the machine.
//   - "machine": directories shared by all sessions of the machine (e.g.
//     $CARGO_HOME/registry). With Digest, the content is immutable per
//     content hash of the digest files: Install generates it remotely,
//     the paths are hidden from the source projection and every session
//     gets a read-only copy linked from the machine store.
//   - "global": a result cache shared by all machines of the pool
//     through the store (Tool: sccache, ccache, go).
type Cache struct {
	Name    string   `toml:"-"`
	Scope   string   `toml:"scope"`
	Paths   []string `toml:"paths"`
	Digest  []string `toml:"digest"`
	Install string   `toml:"install"`
	Tool    string   `toml:"tool"`
	MaxSize string   `toml:"max_size"`
	Prune   string   `toml:"prune"`
	TTL     string   `toml:"ttl"`
}

// Cache scopes.
const (
	ScopeSlot    = "slot"
	ScopeMachine = "machine"
	ScopeGlobal  = "global"
)

// Global cache tools.
const (
	CacheToolSccache = "sccache"
	CacheToolCcache  = "ccache"
	CacheToolGo      = "go"
)

// Prune policies of slot and machine caches.
const (
	PruneRust = "rust" // incremental sessions, unread units, cold slots
	PruneLRU  = "lru"  // files unread for N runs, cold slots
	PruneNone = "none" // TTL and machine budget only
)

// Default cache TTLs per scope.
const (
	DefaultSlotTTL    = 24 * time.Hour
	DefaultMachineTTL = 7 * 24 * time.Hour
	MaxGlobalTTL      = 24 * time.Hour
)

// buildCaches turns the [cache.<name>] tables into Repo.Caches (sorted by
// name) with defaults applied.
func (r *Repo) buildCaches() {
	r.Caches = r.Caches[:0]
	for name, c := range r.CacheTable {
		c.Name = name
		if c.Scope == "" {
			switch {
			case c.Tool != "":
				c.Scope = ScopeGlobal
			default:
				c.Scope = ScopeSlot
			}
		}
		if c.Prune == "" && c.Scope != ScopeGlobal {
			c.Prune = PruneLRU
			if c.Digest != nil {
				c.Prune = PruneNone
			}
		}
		if c.TTL == "" {
			switch c.Scope {
			case ScopeSlot:
				c.TTL = "24h"
			case ScopeMachine:
				c.TTL = "7d"
			case ScopeGlobal:
				c.TTL = "24h"
			}
		}
		r.Caches = append(r.Caches, c)
	}
	sort.Slice(r.Caches, func(i, j int) bool { return r.Caches[i].Name < r.Caches[j].Name })
}

func insideRepo(p string) bool {
	return p != "" && !filepath.IsAbs(p) && !strings.HasPrefix(filepath.Clean(p), "..") &&
		filepath.Clean(p) != "." && !strings.ContainsAny(p, "*?[$~")
}

// validateCaches appends the errors of the cache tables.
func (r *Repo) validateCaches(errs []string) []string {
	slotPaths := map[string]string{}
	tools := map[string]string{}
	for _, c := range r.Caches {
		at := "cache." + c.Name
		if !validCacheName(c.Name) {
			errs = append(errs, fmt.Sprintf("%s: name must match [a-z0-9_-]+", at))
		}
		ttl, err := ParseDuration(c.TTL)
		if err != nil || ttl <= 0 {
			errs = append(errs, fmt.Sprintf("%s.ttl: invalid duration %q", at, c.TTL))
		}
		if c.MaxSize != "" {
			if _, err := ParseBytes(c.MaxSize); err != nil {
				errs = append(errs, at+".max_size: "+err.Error())
			}
		}
		switch c.Prune {
		case "", PruneRust, PruneLRU, PruneNone:
		default:
			errs = append(errs, fmt.Sprintf("%s.prune: unsupported %q (rust, lru, none)", at, c.Prune))
		}
		switch c.Scope {
		case ScopeSlot:
			if len(c.Paths) == 0 {
				errs = append(errs, at+".paths: required for scope \"slot\"")
			}
			for _, p := range c.Paths {
				if !insideRepo(p) {
					errs = append(errs, fmt.Sprintf("%s.paths: %q must be a plain directory inside the repository", at, p))
				} else if other, dup := slotPaths[filepath.Clean(p)]; dup {
					errs = append(errs, fmt.Sprintf("%s.paths: %q is also in cache.%s", at, p, other))
				} else {
					slotPaths[filepath.Clean(p)] = c.Name
				}
			}
			if c.Digest != nil || c.Install != "" || c.Tool != "" {
				errs = append(errs, at+": digest, install and tool are not allowed for scope \"slot\"")
			}
		case ScopeMachine:
			if len(c.Paths) == 0 {
				errs = append(errs, at+".paths: required for scope \"machine\"")
			}
			if c.Tool != "" {
				errs = append(errs, at+".tool: only for scope \"global\"")
			}
			if c.Digest != nil {
				if len(c.Digest) == 0 {
					errs = append(errs, at+".digest: needs at least one file")
				}
				for _, d := range c.Digest {
					if filepath.IsAbs(d) || strings.HasPrefix(filepath.Clean(d), "..") {
						errs = append(errs, fmt.Sprintf("%s.digest: %q must be inside the repository", at, d))
					}
				}
				if strings.TrimSpace(c.Install) == "" {
					errs = append(errs, at+".install: required with digest")
				}
				for _, p := range c.Paths {
					if !insideRepo(p) {
						errs = append(errs, fmt.Sprintf("%s.paths: %q must be a plain directory inside the repository (digest caches)", at, p))
					}
				}
			} else if c.Install != "" {
				errs = append(errs, at+".install: requires digest")
			}
			for _, p := range c.Paths {
				if strings.ContainsAny(p, "*?[") || strings.HasPrefix(filepath.Clean(p), "..") {
					errs = append(errs, fmt.Sprintf("%s.paths: %q must be a plain directory ($VAR, absolute or inside the repository)", at, p))
				}
			}
		case ScopeGlobal:
			switch c.Tool {
			case CacheToolSccache, CacheToolCcache, CacheToolGo:
				if other, dup := tools[c.Tool]; dup {
					errs = append(errs, fmt.Sprintf("%s.tool: %q is also used by cache.%s", at, c.Tool, other))
				}
				tools[c.Tool] = c.Name
			case "":
				errs = append(errs, at+".tool: required for scope \"global\" (sccache, ccache, go)")
			default:
				errs = append(errs, fmt.Sprintf("%s.tool: unsupported %q (sccache, ccache, go)", at, c.Tool))
			}
			if len(c.Paths) > 0 || c.Digest != nil || c.Install != "" || c.Prune != "" {
				errs = append(errs, at+": paths, digest, install and prune are not allowed for scope \"global\"")
			}
			if err == nil && (ttl < time.Second || ttl > MaxGlobalTTL) {
				errs = append(errs, fmt.Sprintf("%s.ttl: %q must be between 1s and 24h", at, c.TTL))
			}
		default:
			errs = append(errs, fmt.Sprintf("%s.scope: unsupported %q (slot, machine, global)", at, c.Scope))
		}
	}
	return errs
}

func validCacheName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// CachesByScope returns the caches of a scope, sorted by name.
func (r *Repo) CachesByScope(scope string) []Cache {
	var out []Cache
	for _, c := range r.Caches {
		if c.Scope == scope {
			out = append(out, c)
		}
	}
	return out
}

// SlotPaths returns the paths of all slot-scope caches (worker-local build
// directories relative to the repository root).
func (r *Repo) SlotPaths() []string {
	var out []string
	for _, c := range r.CachesByScope(ScopeSlot) {
		out = append(out, c.Paths...)
	}
	return out
}

// DigestPaths returns the paths of machine-scope caches with a digest
// (generated remotely, hidden from the source projection).
func (r *Repo) DigestPaths() []string {
	var out []string
	for _, c := range r.CachesByScope(ScopeMachine) {
		if c.Digest != nil {
			out = append(out, c.Paths...)
		}
	}
	return out
}

// GlobalCache returns the global cache using tool, if any.
func (r *Repo) GlobalCache(tool string) (Cache, bool) {
	for _, c := range r.CachesByScope(ScopeGlobal) {
		if c.Tool == tool {
			return c, true
		}
	}
	return Cache{}, false
}

// CacheTTLs returns the TTL of the build slot (the shortest slot-scope
// TTL, default 24h) and of the machine caches (the longest machine-scope
// TTL, default 7d). They travel with every request; the pool caps them by
// its max_lifetime.
func (r *Repo) CacheTTLs() (slot, machine time.Duration) {
	for _, c := range r.Caches {
		d, _ := ParseDuration(c.TTL)
		switch c.Scope {
		case ScopeSlot:
			if slot == 0 || d < slot {
				slot = d
			}
		case ScopeMachine:
			if d > machine {
				machine = d
			}
		}
	}
	if slot == 0 {
		slot = DefaultSlotTTL
	}
	if machine == 0 {
		machine = DefaultMachineTTL
	}
	return slot, machine
}

// TTLDuration returns the cache's TTL.
func (c Cache) TTLDuration() time.Duration {
	d, _ := ParseDuration(c.TTL)
	return d
}

// MaxBytes returns max_size in bytes (0: unbounded).
func (c Cache) MaxBytes() int64 {
	if c.MaxSize == "" {
		return 0
	}
	b, _ := ParseBytes(c.MaxSize)
	return b
}

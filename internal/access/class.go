// Package access learns how the files of a build slot are used and turns
// that into cache policy, like madvise/fadvise hints that the system
// learns on its own.
//
// Every file path maps to a path pattern (Pattern: hashed and numbered
// segments generalized, e.g. target/debug/deps/*.rlib). A Model
// aggregates, per pattern and across sessions, how often files are
// created, overwritten, deleted and read, how long they live and how big
// they are (Observe, fed with the trees before and after a session and
// the paths read), persists that as a small JSON file per repository on
// the worker, and classifies each pattern into an access Class:
//
//   - Immutable (read-mostly): present across sessions, (almost) never
//     written; e.g. compiled dependencies.
//   - WriteOnce (write-once-read-many): new files appear regularly but
//     are not overwritten; e.g. build script outputs.
//   - Churn (write-hot): overwritten or replaced most sessions; e.g.
//     incremental compilation state, fingerprints.
//   - Ephemeral (write-only, short-lived): created and deleted again
//     within about a session, never read; e.g. temporary directories.
//
// Tool adapters seed the model with priors (internal/adapter
// AccessPriors); learned statistics override a prior once a pattern was
// seen in MinSessions sessions. Policy maps a class to the cache policy
// (placement, writeback delay, page-out, prefetch, reclaim order,
// dedupe) the cache file system applies per path.
package access

import (
	"fmt"
	"time"
)

// Class is how the files of a pattern are accessed.
type Class int

// Classes.
const (
	Unknown   Class = iota // not enough observations and no prior
	Immutable              // read-mostly: written once long ago, read often
	WriteOnce              // write-once-read-many: new files, never overwritten
	Churn                  // write-hot: overwritten or replaced most sessions
	Ephemeral              // write-only or short-lived: gone within a session
)

var classNames = [...]string{"unknown", "immutable", "write-once", "churn", "ephemeral"}

func (c Class) String() string {
	if c < 0 || int(c) >= len(classNames) {
		return fmt.Sprintf("class(%d)", int(c))
	}
	return classNames[c]
}

// ParseClass parses a class name.
func ParseClass(s string) (Class, error) {
	for i, n := range classNames {
		if n == s {
			return Class(i), nil
		}
	}
	return Unknown, fmt.Errorf("access: unknown class %q", s)
}

// MarshalText implements encoding.TextMarshaler.
func (c Class) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *Class) UnmarshalText(b []byte) error {
	v, err := ParseClass(string(b))
	*c = v
	return err
}

// Policy is what the cache file system does with the pages of a class.
type Policy struct {
	Class Class `json:"class"`
	// LocalOnly keeps the pages on the local tier: never demoted or paged
	// out to a volume (churn is rewritten before it would be read back).
	LocalOnly bool `json:"local_only,omitempty"`
	// NoWriteback: dirty pages are never written back or paged out while
	// the slot is live (ephemeral files are gone before it matters).
	NoWriteback bool `json:"no_writeback,omitempty"`
	// WritebackDelay is how long dirty pages stay dirty before writeback
	// (the link's dirty_expire for the class): 0 uploads early.
	WritebackDelay time.Duration `json:"writeback_delay,omitempty"`
	// SkipDeleted drops dirty pages of files deleted before their flush
	// instead of writing them back.
	SkipDeleted bool `json:"skip_deleted,omitempty"`
	// Prefetch: read ahead after a restore (the working set of the next
	// session).
	Prefetch bool `json:"prefetch,omitempty"`
	// ReclaimOrder: under pressure pages of lower orders are reclaimed
	// first (ephemeral 0, churn 1, unknown 2, write-once and immutable 3,
	// coldest first within an order).
	ReclaimOrder int `json:"reclaim_order"`
	// Dedupe: content addressed sharing across slots and machines (CAS).
	Dedupe bool `json:"dedupe,omitempty"`
}

// DefaultWritebackDelay is the writeback delay of classes without an
// opinion (the anyfs link default).
const DefaultWritebackDelay = 30 * time.Second

// ChurnWritebackDelay is the long dirty expiry of churn: most of it is
// rewritten or deleted before it would be flushed.
const ChurnWritebackDelay = 10 * time.Minute

// PolicyFor returns the policy of a class.
func PolicyFor(c Class) Policy {
	switch c {
	case Immutable:
		return Policy{Class: c, Prefetch: true, ReclaimOrder: 3, Dedupe: true}
	case WriteOnce:
		return Policy{Class: c, WritebackDelay: 5 * time.Second, Prefetch: true, ReclaimOrder: 3, Dedupe: true}
	case Churn:
		return Policy{Class: c, LocalOnly: true, WritebackDelay: ChurnWritebackDelay, SkipDeleted: true, ReclaimOrder: 1}
	case Ephemeral:
		return Policy{Class: c, LocalOnly: true, NoWriteback: true, SkipDeleted: true, ReclaimOrder: 0}
	}
	return Policy{Class: Unknown, WritebackDelay: DefaultWritebackDelay, ReclaimOrder: 2}
}

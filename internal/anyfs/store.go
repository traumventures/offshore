package anyfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/medium"
)

// RefreshAge is the age after which a page held by a store with the
// default 24h TTL is written again instead of reused (half the TTL). A
// store's own refresh age is RefreshAgeOf: half its TTL, the pool's
// [storage] retention, so a layer stays restorable for the other half.
const RefreshAge = blobcache.DefaultTTL / 2

// RefreshAgeOf returns the refresh age of a store: half its TTL.
func RefreshAgeOf(st blobcache.Store) time.Duration { return blobcache.TTLOf(st) / 2 }

// Store is the backing store tier: pages as objects below pages/ of a
// (scoped) object store. It holds nothing resident.
type Store struct {
	st      blobcache.Store
	med     medium.Profile
	now     func() time.Time
	refresh time.Duration // RefreshAgeOf(st)
}

// StoreMedium is the default profile of the store medium (no numbers).
func StoreMedium() medium.Profile {
	return medium.Profile{Medium: medium.Store, Reach: medium.ReachGlobal, Durability: medium.ReachGlobal}
}

// NewStore returns the store tier over st with the store's profile.
func NewStore(st blobcache.Store, med medium.Profile) *Store {
	if med.Medium == "" {
		med = StoreMedium()
	}
	return &Store{st: st, med: med, now: time.Now, refresh: RefreshAgeOf(st)}
}

// PageObject is the object key of a page.
func PageObject(k Key) string { return "pages/" + string(k[:2]) + "/" + string(k) }

// Get implements Tier; contents are verified.
func (s *Store) Get(ctx context.Context, k Key) ([]byte, error) {
	rc, _, err := s.st.Get(ctx, PageObject(k))
	if err != nil {
		if errors.Is(err, blobcache.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if KeyOf(b) != k {
		return nil, fmt.Errorf("page %s: content hash mismatch", k.Short())
	}
	return b, nil
}

// Has implements Tier: the object exists and is young enough to outlive
// a layer referencing it (RefreshAgeOf the store).
func (s *Store) Has(ctx context.Context, k Key) bool {
	info, err := s.st.Stat(ctx, PageObject(k))
	return err == nil && s.now().Sub(info.ModTime) < s.refresh
}

// Put implements Tier (skipped when the store has a young copy).
func (s *Store) Put(ctx context.Context, k Key, b []byte, _ State) error {
	if s.Has(ctx, k) {
		return nil
	}
	return s.st.Put(ctx, PageObject(k), bytes.NewReader(b), int64(len(b)))
}

// Reclaim implements Tier (nothing resident).
func (s *Store) Reclaim(context.Context, int64) (int64, error) { return 0, nil }

// Profile implements Tier.
func (s *Store) Profile() Profile {
	return Profile{Name: s.med.Medium, Levels: []medium.Profile{s.med}}
}

// Objects returns the underlying object store (layers live next to the
// pages).
func (s *Store) Objects() blobcache.Store { return s.st }

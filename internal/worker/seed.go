package worker

import (
	"context"
	"errors"
	"io"

	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/store"
)

// Seeding: a slot restored from a head of another store prefix.
//
// A restore normally mounts a head of the repository's own prefix. A
// seed mounts the head of a sibling (another worktree of the same
// repository: its build outputs, never its sources) or a head in
// another provider's store: the repository's stack then reads layer
// objects and pages its own prefix lacks from the source (a read-only
// fallback below its own store) and the first flush rewrites every page
// of the head the own prefix lacks (refresh), so the repository's heads
// never reference another prefix and the source is needed only until
// then.

// fallbackStore reads objects the primary lacks from a read-only source;
// writes, deletes and Stat (what the store "has": put skipping and head
// flushes rely on it) go to the primary only.
type fallbackStore struct {
	primary, source blobcache.Store
}

func (f fallbackStore) Get(ctx context.Context, key string) (io.ReadCloser, blobcache.Info, error) {
	rc, info, err := f.primary.Get(ctx, key)
	if errors.Is(err, blobcache.ErrNotFound) {
		return f.source.Get(ctx, key)
	}
	return rc, info, err
}

func (f fallbackStore) Stat(ctx context.Context, key string) (blobcache.Info, error) {
	return f.primary.Stat(ctx, key)
}

func (f fallbackStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return f.primary.Put(ctx, key, r, size)
}

func (f fallbackStore) Delete(ctx context.Context, key string) error {
	return f.primary.Delete(ctx, key)
}

// setSeed adds a read-only source below a repository's store prefix:
// the prefix of repository src in the store of grant g. The next flush
// refreshes every page of the head into the repository's own prefix.
func (c *cacheFS) setSeed(repo, src string, g store.Grant) error {
	r, err := c.repo(repo)
	if err != nil {
		return err
	}
	st, err := store.Open(g, store.RepoScope(src), blobcache.MaxTTL)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.seed, r.seedPending = st, true
	r.mu.Unlock()
	return c.relink(r)
}

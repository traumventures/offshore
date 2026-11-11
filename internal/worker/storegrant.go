package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/store"
)

// grantCache keeps the latest store grant per repository (sessions and
// slot operations bring them; build slot writebacks use them).
type grantCache struct {
	mu sync.Mutex
	m  map[string]store.Grant
}

func (g *grantCache) put(repo string, gr store.Grant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]store.Grant{}
	}
	if old, ok := g.m[repo]; ok && old.Expires > gr.Expires && gr.Expires != 0 {
		return
	}
	g.m[repo] = gr
}

// get returns a grant of repo valid for at least margin.
func (g *grantCache) get(repo string, margin time.Duration) (store.Grant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr, ok := g.m[repo]
	if !ok || gr.Expired(time.Now(), margin) {
		return store.Grant{}, false
	}
	return gr, true
}

// noteGrant records a grant brought by a session or slot operation and
// connects the result cache to the environment's store prefix.
func (s *Worker) noteGrant(repo string, gr *store.Grant) {
	if gr == nil {
		return
	}
	s.grants.put(repo, *gr)
	if s.cfs != nil && repo != "" {
		if g, ok := s.grants.get(repo, 0); ok {
			if err := s.cfs.setGrant(repo, g); err != nil {
				s.log.Warn("build slot store", "repo", repo, "err", err)
			}
		}
	}
	if s.rcStore != nil && s.cfg.EnvHash != "" && gr.Allows(store.EnvScope(s.cfg.EnvHash)) {
		s.rcStore.setRemote(*gr, store.EnvScope(s.cfg.EnvHash))
	}
}

// tieredStore is the result caches' store: the machine's directory store,
// backed by the pool's store when a grant is known. Reads fill the local
// tier; writes go to both (the remote write is part of the request, so
// sccache's wait for pending writes at session end covers it).
type tieredStore struct {
	local *blobcache.DirStore
	log   *slog.Logger

	mu      sync.Mutex
	remote  blobcache.Store
	expires int64 // grant expiry of remote (0: never)
}

func (t *tieredStore) setRemote(g store.Grant, scope string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.remote != nil && (t.expires == 0 || t.expires >= g.Expires) && g.Expires != 0 {
		return
	}
	st, err := store.Open(g, scope, blobcache.MaxTTL)
	if err != nil {
		if t.log != nil {
			t.log.Warn("result cache store", "err", err)
		}
		return
	}
	t.remote, t.expires = st, g.Expires
}

func (t *tieredStore) backend() blobcache.Store {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.remote == nil || (t.expires != 0 && time.Now().Unix() >= t.expires) {
		return nil
	}
	return t.remote
}

// Get implements blobcache.Store.
func (t *tieredStore) Get(ctx context.Context, key string) (io.ReadCloser, blobcache.Info, error) {
	rc, info, err := t.local.Get(ctx, key)
	if err == nil || !errors.Is(err, blobcache.ErrNotFound) {
		return rc, info, err
	}
	r := t.backend()
	if r == nil {
		return nil, blobcache.Info{}, err
	}
	rrc, rinfo, rerr := r.Get(ctx, key)
	if rerr != nil {
		return nil, blobcache.Info{}, rerr
	}
	perr := t.local.Put(ctx, key, rrc, rinfo.Size)
	rrc.Close()
	if perr != nil {
		return nil, blobcache.Info{}, perr
	}
	return t.local.Get(ctx, key)
}

// Stat implements blobcache.Store.
func (t *tieredStore) Stat(ctx context.Context, key string) (blobcache.Info, error) {
	info, err := t.local.Stat(ctx, key)
	if err == nil || !errors.Is(err, blobcache.ErrNotFound) {
		return info, err
	}
	if r := t.backend(); r != nil {
		return r.Stat(ctx, key)
	}
	return info, err
}

// Put implements blobcache.Store.
func (t *tieredStore) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if err := t.local.Put(ctx, key, body, size); err != nil {
		return err
	}
	r := t.backend()
	if r == nil {
		return nil
	}
	rc, info, err := t.local.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := r.Put(ctx, key, rc, info.Size); err != nil && t.log != nil {
		t.log.Warn("result cache upload", "key", key, "err", err) // local copy stays
	}
	return nil
}

// Delete implements blobcache.Store.
func (t *tieredStore) Delete(ctx context.Context, key string) error {
	err := t.local.Delete(ctx, key)
	if r := t.backend(); r != nil {
		if rerr := r.Delete(ctx, key); rerr != nil && !errors.Is(rerr, blobcache.ErrNotFound) {
			return rerr
		}
	}
	return err
}

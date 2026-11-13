package orchestrator

import (
	"context"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/store"
)

// GrantTTL is the lifetime of worker store credentials.
const GrantTTL = 12 * time.Hour

// StoreSet resolves the store of each provider instance: the pool's
// [storage] override or the provider's native store. Granters are created
// on first use (a native store may need the provider's infrastructure).
type StoreSet struct {
	Pool     string
	Override *store.Config // [storage] (nil: native stores)
	Provs    map[string]provider.Provider
	// Retention is the pool's [storage] retention (0: blobcache.DefaultTTL):
	// every grant carries it, so stores opened from grants never serve
	// older objects and workers rewrite chains after half of it.
	Retention time.Duration
	// NewGranter is store.NewGranter (replaceable in tests).
	NewGranter func(context.Context, store.Config) (store.Granter, error)

	mu       sync.Mutex
	granters map[string]store.Granter
}

// Granter returns the granter of a provider instance (nil, nil: no store).
func (s *StoreSet) Granter(ctx context.Context, prov string) (store.Granter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.granters[prov]; ok {
		return g, nil
	}
	p := s.Provs[prov]
	if p == nil && s.Override == nil {
		return nil, nil
	}
	if sg, isG := p.(provider.StoreGranter); isG && s.Override == nil {
		g, ok, err := sg.NativeGranter(ctx, prov, s.Pool)
		if err != nil || !ok {
			return nil, err
		}
		return s.remember(prov, g), nil
	}
	c, ok, err := provider.StoreConfig(ctx, p, s.Pool, s.Override)
	if err != nil || !ok {
		return nil, err
	}
	if s.Override == nil {
		c.Name = prov // native stores are named after the provider instance
	}
	newG := s.NewGranter
	if newG == nil {
		newG = store.NewGranter
	}
	g, err := newG(ctx, c)
	if err != nil {
		return nil, err
	}
	return s.remember(prov, g), nil
}

// remember caches a provider's granter, stamping the retention on its
// grants, and returns it.
func (s *StoreSet) remember(prov string, g store.Granter) store.Granter {
	if s.granters == nil {
		s.granters = map[string]store.Granter{}
	}
	rg := retentionGranter{g, s.RetentionTTL()}
	s.granters[prov] = rg
	return rg
}

// RetentionTTL returns the store retention (the object TTL).
func (s *StoreSet) RetentionTTL() time.Duration {
	if s == nil || s.Retention <= 0 {
		return blobcache.DefaultTTL
	}
	return blobcache.ClampTTL(s.Retention)
}

// retentionGranter stamps the pool's retention on grants.
type retentionGranter struct {
	store.Granter
	retention time.Duration
}

func (g retentionGranter) Grant(ctx context.Context, scopes []string, ttl time.Duration) (store.Grant, error) {
	gr, err := g.Granter.Grant(ctx, scopes, ttl)
	if err == nil {
		gr.RetentionSeconds = int64(g.retention / time.Second)
	}
	return gr, err
}

// storeGrant mints a worker grant for a repository and environment on a
// provider's machine (nil if the provider has no store or minting fails,
// which only disables snapshots and the shared result cache).
func (o *Orchestrator) storeGrant(ctx context.Context, prov, repo, env string) *store.Grant {
	if o.Stores == nil || repo == "" {
		return nil
	}
	g, err := o.Stores.Granter(ctx, prov)
	if err != nil {
		o.log.Warn("store unavailable", "provider", prov, "err", err)
		return nil
	}
	if g == nil {
		return nil
	}
	scopes := []string{store.RepoScope(repo)}
	if env != "" {
		scopes = append(scopes, store.EnvScope(env))
	}
	gr, err := g.Grant(ctx, scopes, GrantTTL)
	if err != nil {
		o.log.Warn("store grant failed", "provider", prov, "err", err)
		return nil
	}
	return &gr
}

// SlotGrant mints a store grant for slot operations on a machine.
func (o *Orchestrator) SlotGrant(ctx context.Context, machineID, repo string) *store.Grant {
	o.mu.Lock()
	m := o.machines[machineID]
	var prov, env string
	if m != nil {
		prov, env = m.Provider, m.EnvHash
	}
	o.mu.Unlock()
	if m == nil {
		return nil
	}
	return o.storeGrant(ctx, prov, repo, env)
}

// SourceGrant mints a grant of repo's prefix in the store named storeName
// for a restore on a machine whose own store or repository differs (a
// sibling worktree's head, a head in another provider's store). Native
// stores are named after their provider instance; a store of no known
// provider is the machine's own (the pool's [storage]). The grant
// carries no environment scope.
func (o *Orchestrator) SourceGrant(ctx context.Context, machineID, storeName, repo string) *store.Grant {
	o.mu.Lock()
	prov := ""
	if m := o.machines[machineID]; m != nil {
		prov = m.Provider
	}
	if _, ok := o.provs[storeName]; ok {
		prov = storeName
	}
	o.mu.Unlock()
	if prov == "" {
		return nil
	}
	return o.storeGrant(ctx, prov, repo, "")
}

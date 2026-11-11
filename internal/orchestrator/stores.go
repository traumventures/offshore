package orchestrator

import (
	"context"
	"sync"
	"time"

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
	if s.granters == nil {
		s.granters = map[string]store.Granter{}
	}
	s.granters[prov] = g
	return g, nil
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

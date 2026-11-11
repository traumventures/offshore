package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/store"
)

type nameGranter string

func (n nameGranter) Name() string { return string(n) }
func (n nameGranter) Grant(context.Context, []string, time.Duration) (store.Grant, error) {
	return store.Grant{Store: string(n)}, nil
}

// granterProvider has a native granter (like OCI's pre-authenticated
// requests) instead of an S3 store config.
type granterProvider struct {
	provider.Provider
	calls int
}

func (g *granterProvider) NativeGranter(_ context.Context, name, pool string) (store.Granter, bool, error) {
	g.calls++
	return nameGranter(name + "@" + pool), true, nil
}

func TestStoreSetNativeGranter(t *testing.T) {
	gp := &granterProvider{}
	s := &StoreSet{Pool: "p1", Provs: map[string]provider.Provider{"oci-fra": gp}}
	g, err := s.Granter(context.Background(), "oci-fra")
	if err != nil || g == nil || g.Name() != "oci-fra@p1" {
		t.Fatalf("granter %v %v", g, err)
	}
	if g, _ = s.Granter(context.Background(), "oci-fra"); gp.calls != 1 || g.Name() != "oci-fra@p1" {
		t.Fatalf("granter not cached: %d calls", gp.calls)
	}
	// [storage] overrides the native store.
	s = &StoreSet{Pool: "p1", Provs: map[string]provider.Provider{"oci-fra": gp}, Override: &store.Config{Name: "storage", Dir: t.TempDir()}}
	if g, err = s.Granter(context.Background(), "oci-fra"); err != nil || g.Name() != "storage" {
		t.Fatalf("override %v %v", g, err)
	}
}

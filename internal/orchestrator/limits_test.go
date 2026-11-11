package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// A quota error caps the provider at the vCPUs the pool runs there (no
// cooldown, not disabled); the cap lapses after limitCap.
func TestQuotaErrorCapsProvider(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	if _, err := o.Acquire(context.Background(), req("r1"), io.Discard); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	took := o.providerFailedLocked("fake", errors.New("api error VcpuLimitExceeded: You have requested more vCPU capacity"))
	pool := o.poolLocked(o.now())
	o.mu.Unlock()
	if !took {
		t.Fatal("quota error not handled by the provider health")
	}
	if l := pool.ProviderLimit["fake"]; l.CPU != 4 || l.Source != "limit error" {
		t.Fatalf("limit %+v, want 4 vCPUs from the limit error", l)
	}
	if ps := pool.Providers["fake"]; pool.Now.Before(ps.Down) {
		t.Fatalf("capped provider must not cool down: down until %v", ps.Down)
	}
	o.mu.Lock()
	h := o.health["fake"]
	o.mu.Unlock()
	if l := h.limit(o.now().Add(limitCap + time.Second)); l != (scheduler.Limit{}) {
		t.Fatalf("cap must lapse: %+v", l)
	}
}

// Without a machine of the provider a quota error is a cooldown (a cap
// at 0 vCPUs would mean no limit).
func TestQuotaErrorWithoutMachinesCoolsDown(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	o.mu.Lock()
	o.providerFailedLocked("fake", provider.ClassError(provider.ClassQuota, errors.New("limit")))
	pool := o.poolLocked(o.now())
	o.mu.Unlock()
	if !pool.Now.Before(pool.Providers["fake"].Down) || pool.ProviderLimit["fake"] != (scheduler.Limit{}) {
		t.Fatalf("want a cooldown and no cap: %+v %+v", pool.Providers["fake"], pool.ProviderLimit)
	}
}

type quotaFake struct {
	*fakeProvider
	vcpus float64
	own   float64
}

func (q *quotaFake) Quota(_ context.Context, _ string, own float64) (float64, bool, error) {
	q.own = own
	return q.vcpus, true, nil
}

// Providers that report their quota get it as the scheduler's limit.
func TestQuotaDiscovery(t *testing.T) {
	q := &quotaFake{fakeProvider: newFake(), vcpus: 12}
	cfg := testConfig("fake")
	o, err := New(cfg, map[string]provider.Provider{"fake": q}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o.refreshQuotas(context.Background())
	o.mu.Lock()
	pool := o.poolLocked(o.now())
	o.mu.Unlock()
	if l := pool.ProviderLimit["fake"]; l.CPU != 12 || l.Source != "quota" {
		t.Fatalf("limit %+v", l)
	}
	q.vcpus = 16
	o.refreshQuotas(context.Background()) // within quotaRefresh: not read again
	o.mu.Lock()
	pool = o.poolLocked(o.now())
	o.mu.Unlock()
	if pool.ProviderLimit["fake"].CPU != 12 {
		t.Fatalf("quota re-read before quotaRefresh: %+v", pool.ProviderLimit["fake"])
	}
}

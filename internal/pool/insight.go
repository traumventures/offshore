package pool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/orchestrator"
)

// Control plane paths for provider health and session time series (POST,
// API key).
const (
	pathProviders      = "/v1/providers"
	pathProviderReset  = "/v1/providers/reset"
	pathSessionSamples = "/v1/sessions/samples"
)

// InsightSource is implemented by pools that expose provider health and
// per-session time series.
type InsightSource interface {
	// Providers returns the health of every provider (ok, cooldown,
	// disabled with the error).
	Providers(ctx context.Context) ([]orchestrator.ProviderHealth, error)
	// ResetProvider re-enables a provider disabled by an account limit or
	// auth error and ends its cooldown.
	ResetProvider(ctx context.Context, name string) error
	// SessionSamples returns the cgroup samples (every 5 s) of a session.
	SessionSamples(ctx context.Context, lease string, since time.Time) ([]events.Sample, error)
}

var (
	_ InsightSource = (*Local)(nil)
	_ InsightSource = (*Remote)(nil)
)

type providerReq struct {
	Name string `json:"name"`
}

type samplesReq struct {
	Lease string    `json:"lease"`
	Since time.Time `json:"since,omitzero"`
}

// Providers implements InsightSource.
func (l *Local) Providers(context.Context) ([]orchestrator.ProviderHealth, error) {
	return l.orch.Providers(), nil
}

// ResetProvider implements InsightSource.
func (l *Local) ResetProvider(_ context.Context, name string) error {
	return l.orch.ResetProvider(name)
}

// SessionSamples implements InsightSource.
func (l *Local) SessionSamples(_ context.Context, lease string, since time.Time) ([]events.Sample, error) {
	if lease == "" {
		return nil, errors.New("lease required")
	}
	return l.orch.SessionSamples(lease, since), nil
}

// Providers implements InsightSource by asking the control plane.
func (r *Remote) Providers(ctx context.Context) ([]orchestrator.ProviderHealth, error) {
	var out []orchestrator.ProviderHealth
	err := r.callJSON(ctx, pathProviders, nil, nil, &out)
	return out, err
}

// ResetProvider implements InsightSource by asking the control plane.
func (r *Remote) ResetProvider(ctx context.Context, name string) error {
	return r.callJSON(ctx, pathProviderReset, providerReq{Name: name}, nil, nil)
}

// SessionSamples implements InsightSource by asking the control plane.
func (r *Remote) SessionSamples(ctx context.Context, lease string, since time.Time) ([]events.Sample, error) {
	var out []events.Sample
	err := r.callJSON(ctx, pathSessionSamples, samplesReq{Lease: lease, Since: since}, nil, &out)
	return out, err
}

// insightHandler returns the handler of an insight path (nil: none).
func (s *Server) insightHandler(path string) func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
	src := s.pool
	switch path {
	case pathProviders:
		return func(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) { return src.Providers(ctx) }
	case pathProviderReset:
		return func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req providerReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return nil, src.ResetProvider(ctx, req.Name)
		}
	case pathSessionSamples:
		return func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req samplesReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return src.SessionSamples(ctx, req.Lease, req.Since)
		}
	}
	return nil
}

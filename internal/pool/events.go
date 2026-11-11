package pool

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/tracing"
)

// pathEvents returns the analysis event log (POST, API key).
const pathEvents = "/v1/events"

// maxEvents bounds one events response.
const maxEvents = 100000

// EventSource is implemented by pools that keep the analysis event log.
type EventSource interface {
	// Events returns the events with since <= time < until (zero: open).
	Events(ctx context.Context, since, until time.Time) ([]events.Event, error)
}

var (
	_ EventSource = (*Local)(nil)
	_ EventSource = (*Remote)(nil)
)

type eventsReq struct {
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`
}

// Events implements EventSource from the orchestrator's state directory.
func (l *Local) Events(_ context.Context, since, until time.Time) ([]events.Event, error) {
	evs, err := events.Read(l.orch.Events().Dir(), since, until)
	if len(evs) > maxEvents {
		evs = evs[len(evs)-maxEvents:]
	}
	return evs, err
}

// Events implements EventSource by asking the control plane.
func (r *Remote) Events(ctx context.Context, since, until time.Time) ([]events.Event, error) {
	var evs []events.Event
	err := r.callJSON(ctx, pathEvents, eventsReq{Since: since, Until: until}, nil, &evs)
	return evs, err
}

// serveEvents handles pathEvents.
func (s *Server) serveEvents(ctx context.Context, r *http.Request) (any, error) {
	var req eventsReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return s.pool.Events(ctx, req.Since, req.Until)
}

// clientSpan starts the span of a control plane call and propagates it.
func clientSpan(ctx context.Context, req *http.Request, path string) trace.Span {
	ctx, span := tracing.Start(ctx, "controlplane "+path, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", req.Method), attribute.String("url.path", path)))
	tracing.InjectHTTP(ctx, req.Header)
	return span
}

// serverSpan continues the caller's trace for a control plane request.
func serverSpan(r *http.Request, path string) (*http.Request, trace.Span) {
	ctx := tracing.ExtractHTTP(r.Context(), r.Header)
	ctx, span := tracing.Start(ctx, "controlplane "+path, trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("http.request.method", r.Method), attribute.String("url.path", path)))
	return r.WithContext(ctx), span
}

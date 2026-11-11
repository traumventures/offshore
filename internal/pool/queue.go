package pool

import (
	"context"
	"io"
	"net/http"

	"github.com/traumventures/cloudsourced/internal/orchestrator"
)

// pathQueue returns the requests waiting for capacity (POST, API key).
const pathQueue = "/v1/queue"

// QueueSource is implemented by pools that can list waiting requests.
type QueueSource interface {
	Queue(ctx context.Context) ([]orchestrator.QueuedTask, error)
}

var (
	_ QueueSource = (*Local)(nil)
	_ QueueSource = (*Remote)(nil)
)

// Queue implements QueueSource.
func (l *Local) Queue(context.Context) ([]orchestrator.QueuedTask, error) { return l.orch.Queue(), nil }

// Queue implements QueueSource by asking the control plane.
func (r *Remote) Queue(ctx context.Context) ([]orchestrator.QueuedTask, error) {
	var q []orchestrator.QueuedTask
	err := r.callJSON(ctx, pathQueue, nil, nil, &q)
	return q, err
}

// serveQueue handles pathQueue.
func (s *Server) serveQueue(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) {
	return s.pool.Queue(ctx)
}

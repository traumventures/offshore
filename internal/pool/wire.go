package pool

import (
	"encoding/json"
	"sync"

	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// API paths of the control plane. Every response is a stream of
// newline-delimited Frames ending with a Done frame, so that long running
// calls (image builds, queueing for capacity) report progress.
const (
	pathEnv         = "/v1/env"
	pathAcquire     = "/v1/acquire"
	pathRelease     = "/v1/release"
	pathUnreachable = "/v1/unreachable"
	pathEstimate    = "/v1/estimate"
	pathStatus      = "/v1/status"
	pathReconcile   = "/v1/reconcile"
	pathDown        = "/v1/down"
	pathHealth      = "/healthz"
)

// Frame is one line of a control plane response.
type Frame struct {
	Progress string          `json:"progress,omitempty"`
	Error    string          `json:"error,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Done     bool            `json:"done,omitempty"`
}

type releaseReq struct {
	Lease    string           `json:"lease"`
	UsageKey string           `json:"usage_key,omitempty"`
	Usage    *scheduler.Usage `json:"usage,omitempty"`
}

type unreachableReq struct {
	Lease   string `json:"lease"`
	Machine string `json:"machine"`
}

type estimateReq struct {
	Key      string              `json:"key"`
	Fallback scheduler.Resources `json:"fallback"`
}

// envResult tells the client whether the control plane needs the context.
type envResult struct {
	NeedContext bool `json:"need_context,omitempty"`
}

// frameWriter turns writes into progress frames.
type frameWriter struct {
	mu    *sync.Mutex
	enc   *json.Encoder
	flush func()
}

func (w frameWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.enc.Encode(Frame{Progress: string(b)})
	w.flush()
	return len(b), err
}

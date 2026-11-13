package pool

import (
	"context"
	"testing"

	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/scheduler"
)

// Clients that predate Estimate.LearnedPeak send none: the control plane
// fills it from its own history.
func TestServerPeakFillsOldClients(t *testing.T) {
	h := scheduler.NewHistory("")
	h.Observe("r/cargo/clippy?p=a,b", scheduler.Usage{PeakMemory: 14 << 30})
	l := &Local{est: h}
	need := scheduler.Resources{CPU: 3, Memory: 3 << 30}
	req := l.serverPeak(context.Background(), orchestrator.Request{Key: "r2/cargo/clippy?p=a,b,c", Need: need,
		Estimate: &scheduler.Estimate{Key: "r2/cargo/clippy?p=a,b,c", Need: need, Statistic: "client"}})
	if req.Estimate == nil || req.Estimate.LearnedPeak != 14<<30 || req.Estimate.Statistic != "client" {
		t.Fatalf("%+v", req.Estimate)
	}
	req = l.serverPeak(context.Background(), orchestrator.Request{Key: "r2/cargo/clippy?p=a,b,c", Need: need})
	if req.Estimate == nil || req.Estimate.LearnedPeak != 14<<30 {
		t.Fatalf("no client estimate: %+v", req.Estimate)
	}
}

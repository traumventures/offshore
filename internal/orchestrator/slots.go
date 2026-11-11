package orchestrator

import (
	"context"

	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/scheduler"
)

// SlotTarget addresses a worker for build slot operations.
type SlotTarget struct {
	Machine     string
	Addr        string
	Fingerprint string
	Token       string
}

// SlotOps are the build slot mechanisms of the workers (implemented by the
// storage layer). The scheduler decides when to use them: a full slot is
// pruned, its disk resized, moved (saved and restored elsewhere) or
// rebuilt cold.
type SlotOps interface {
	// PruneSlot shrinks repo's build slot on the machine to at most
	// targetBytes (0: the cache's prune policy) and returns the bytes freed.
	PruneSlot(ctx context.Context, t SlotTarget, repo string, targetBytes int64) (int64, error)
	// SaveSlot commits repo's build slot and flushes it to the object
	// store; it returns the head.
	SaveSlot(ctx context.Context, t SlotTarget, repo string) (proto.LayerHead, error)
	// RestoreSlot mounts a head as repo's build slot on the machine
	// (contents are faulted in lazily).
	RestoreSlot(ctx context.Context, t SlotTarget, repo string, s scheduler.Snapshot) error
}

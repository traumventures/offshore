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
	// ReclaimSlot makes freeBytes free on the machine's cache disk beyond
	// the worker's reserve for a session of repo (whose slots stay) and
	// returns the bytes freed; an error when it could not make the room.
	ReclaimSlot(ctx context.Context, t SlotTarget, repo string, freeBytes int64) (int64, error)
	// SaveSlot commits repo's build slot and flushes it to the object
	// store; it returns the head.
	SaveSlot(ctx context.Context, t SlotTarget, repo string) (proto.LayerHead, error)
	// RestoreSlot mounts a head as repo's build slot on the machine
	// (contents are faulted in lazily).
	RestoreSlot(ctx context.Context, t SlotTarget, repo string, s scheduler.Snapshot) error
}

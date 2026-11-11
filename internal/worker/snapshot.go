package worker

import (
	"context"
	"errors"

	"github.com/traumventures/cloudsourced/internal/proto"
)

var errNoStore = errors.New("snapshots need a store (not configured on this machine)")

func (s *Worker) saveSlot(ctx context.Context, repo string) (proto.SlotResult, error) {
	return proto.SlotResult{}, errNoStore
}

func (s *Worker) restoreSlot(ctx context.Context, repo string, snap proto.SnapshotInfo) (proto.SlotResult, error) {
	return proto.SlotResult{}, errNoStore
}

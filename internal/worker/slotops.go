package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/traumventures/cloudsourced/internal/proto"
)

// serveSlot runs one build slot operation of the orchestrator.
func (s *Worker) serveSlot(ctx context.Context, pc *proto.Conn) error {
	var op proto.SlotOp
	if err := pc.Expect(proto.TSlotOp, &op); err != nil {
		return err
	}
	res := s.slotOp(ctx, op)
	return pc.Send(proto.TSlotResult, res)
}

func (s *Worker) slotOp(ctx context.Context, op proto.SlotOp) proto.SlotResult {
	var res proto.SlotResult
	begin := time.Now()
	s.noteGrant(op.Repo, op.Store)
	var err error
	switch op.Op {
	case proto.SlotPrune:
		res.Freed, err = s.pruneRepo(op.Repo, op.TargetBytes)
	case proto.SlotSave:
		res, err = s.saveSlot(ctx, op.Repo)
	case proto.SlotRestore:
		if op.Snapshot == nil {
			err = fmt.Errorf("restore: no snapshot")
		} else {
			res, err = s.restoreSlot(ctx, op.Repo, *op.Snapshot)
		}
	default:
		err = fmt.Errorf("unknown slot operation %q", op.Op)
	}
	if res.Seconds == 0 {
		res.Seconds = time.Since(begin).Seconds()
	}
	if err != nil {
		res.Err = err.Error()
		s.log.Warn("slot operation failed", "op", op.Op, "repo", op.Repo, "err", err)
	} else {
		s.log.Info("slot operation", "op", op.Op, "repo", op.Repo, "freed", res.Freed, "seconds", res.Seconds)
	}
	return res
}

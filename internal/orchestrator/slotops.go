package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/store"
	"github.com/traumventures/offshore/internal/transport"
)

// WorkerSlotOps implements SlotOps over the workers' slot streams
// (proto.KindSlot). SlotTarget.Token must be an admin token of the
// machine (Orchestrator.AdminToken). New wires it as Orchestrator.Slots.
type WorkerSlotOps struct {
	// Grant mints the store grant sent with saves and restores
	// (Orchestrator.SlotGrant; nil: the worker uses the last grant it got
	// with a session of the repository).
	Grant func(ctx context.Context, machineID, repo string) *store.Grant
	// Timeout bounds one operation (0: 30 minutes; saves and restores
	// move gigabytes).
	Timeout time.Duration
}

var _ SlotOps = WorkerSlotOps{}

func (w WorkerSlotOps) call(ctx context.Context, t SlotTarget, op proto.SlotOp) (proto.SlotResult, error) {
	d := w.Timeout
	if d <= 0 {
		d = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	conn, err := transport.Dial(ctx, t.Addr, t.Fingerprint)
	if err != nil {
		return proto.SlotResult{}, err
	}
	defer conn.CloseWithError(0, "")
	// The stream reads do not follow ctx: closing the connection ends a
	// worker that does not answer in time (a prune blocked behind the
	// worker's own maintenance held placements for up to 20 minutes).
	stop := context.AfterFunc(ctx, func() { conn.CloseWithError(0, "deadline") })
	defer stop()
	_, pc, err := transport.OpenStream(ctx, conn, t.Token, proto.KindSlot)
	if err != nil {
		return proto.SlotResult{}, err
	}
	if err := pc.Send(proto.TSlotOp, op); err != nil {
		return proto.SlotResult{}, err
	}
	var res proto.SlotResult
	if err := pc.Expect(proto.TSlotResult, &res); err != nil {
		if ctx.Err() != nil {
			return res, fmt.Errorf("slot %s on %s: %w", op.Op, t.Machine, ctx.Err())
		}
		return res, err
	}
	if res.Err != "" {
		return res, errors.New(res.Err)
	}
	return res, nil
}

// PruneSlot implements SlotOps.
func (w WorkerSlotOps) PruneSlot(ctx context.Context, t SlotTarget, repo string, targetBytes int64) (int64, error) {
	res, err := w.call(ctx, t, proto.SlotOp{Op: proto.SlotPrune, Repo: repo, TargetBytes: targetBytes})
	return res.Freed, err
}

// SaveSlot implements SlotOps.
func (w WorkerSlotOps) SaveSlot(ctx context.Context, t SlotTarget, repo string) (proto.LayerHead, error) {
	res, err := w.call(ctx, t, proto.SlotOp{Op: proto.SlotSave, Repo: repo, Store: w.grant(ctx, t, repo)})
	if err != nil {
		return proto.LayerHead{}, err
	}
	if res.Head == nil {
		return proto.LayerHead{}, errors.New("worker returned no head")
	}
	return *res.Head, nil
}

// RestoreSlot implements SlotOps.
func (w WorkerSlotOps) RestoreSlot(ctx context.Context, t SlotTarget, repo string, s scheduler.Snapshot) error {
	h := proto.LayerHead{Repo: s.Repo, Store: s.Store, Provider: s.Provider, Layer: s.Layer, Bytes: s.Bytes, At: s.At.Unix()}
	_, err := w.call(ctx, t, proto.SlotOp{Op: proto.SlotRestore, Repo: repo, Head: &h, Store: w.grant(ctx, t, repo)})
	return err
}

func (w WorkerSlotOps) grant(ctx context.Context, t SlotTarget, repo string) *store.Grant {
	if w.Grant == nil {
		return nil
	}
	return w.Grant(ctx, t.Machine, repo)
}

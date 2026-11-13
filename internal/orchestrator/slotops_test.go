package orchestrator

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/transport"
)

// A worker that accepts a slot operation and never answers must not hold
// the placement past the caller's deadline (production: prunes held
// placements for up to 20 minutes although slot preparation had a
// 5-minute timeout, because stream reads ignored the context).
func TestSlotOpHonorsDeadline(t *testing.T) {
	id, err := transport.NewIdentity("w", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", id)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	go func() {
		for {
			c, err := ln.Accept(sctx)
			if err != nil {
				return
			}
			go func() {
				s, err := c.AcceptStream(sctx)
				if err != nil {
					return
				}
				io.Copy(io.Discard, s) // read the request, never answer
			}()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	_, err = WorkerSlotOps{}.ReclaimSlot(ctx, SlotTarget{Machine: "m", Addr: ln.Addr().String(), Fingerprint: id.Fingerprint}, "repo", 0)
	if err == nil {
		t.Fatal("no error from a worker that never answers")
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("prune returned after %s, past its 300ms deadline", d)
	}
}

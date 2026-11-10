package hostagent

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/traumventures/cloudsourced/internal/machine"
)

// spotPlatform is a platform whose machine gets interruption notices.
type spotPlatform struct {
	fakePlatform
	notice *machine.DrainNotice
}

func (p *spotPlatform) Interruption(context.Context) (*machine.DrainNotice, error) { return p.notice, nil }

func TestInterruptionNoticeDrainsServer(t *testing.T) {
	dir := t.TempDir()
	p := &spotPlatform{}
	a := &Agent{Spec: Spec{Layout: machine.Layout{ID: "m1", Image: "img", StatusDir: dir}}, Platform: p,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	if n := a.CheckInterruption(ctx, nil); n != nil {
		t.Fatalf("no notice yet, got %+v", n)
	}
	if _, ok, _ := machine.ReadDrainNotice(dir); ok {
		t.Fatal("drain notice written without an interruption")
	}
	p.notice = &machine.DrainNotice{Reason: machine.DrainSpotRebalance, Noticed: 100}
	last := a.CheckInterruption(ctx, nil)
	if n, ok, _ := machine.ReadDrainNotice(dir); !ok || n.Reason != machine.DrainSpotRebalance || last == nil {
		t.Fatalf("rebalance notice: %+v %v", n, ok)
	}
	p.notice = &machine.DrainNotice{Reason: machine.DrainSpotInterruption, Action: "stop", Deadline: 220, Noticed: 100}
	last = a.CheckInterruption(ctx, last)
	if n, _, _ := machine.ReadDrainNotice(dir); n.Reason != machine.DrainSpotInterruption || n.Deadline != 220 {
		t.Fatalf("interruption notice: %+v", n)
	}
	// An interruption is never downgraded by a later rebalance notice.
	p.notice = &machine.DrainNotice{Reason: machine.DrainSpotRebalance, Noticed: 150}
	if a.CheckInterruption(ctx, last).Reason != machine.DrainSpotInterruption {
		t.Fatal("interruption downgraded")
	}
	if n, _, _ := machine.ReadDrainNotice(dir); n.Reason != machine.DrainSpotInterruption {
		t.Fatalf("drain notice downgraded: %+v", n)
	}
}

func TestNoInterruptionsWithoutSupport(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{Spec: Spec{Layout: machine.Layout{ID: "m1", Image: "img", StatusDir: dir}}, Platform: &fakePlatform{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if a.CheckInterruption(context.Background(), nil) != nil {
		t.Fatal("platform without Interrupter reported a notice")
	}
}

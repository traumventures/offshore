package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/store"
)

// recordingSlots records slot operations.
type recordingSlots struct {
	mu       sync.Mutex
	saved    []string // machine/repo
	restored []string
}

func (r *recordingSlots) PruneSlot(context.Context, SlotTarget, string, int64) (int64, error) {
	return 0, nil
}

func (r *recordingSlots) SaveSlot(_ context.Context, t SlotTarget, repo string) (scheduler.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = append(r.saved, t.Machine+"/"+repo)
	return scheduler.Snapshot{Repo: repo, Store: "storage", Bytes: 1 << 30, At: time.Now()}, nil
}

func (r *recordingSlots) RestoreSlot(_ context.Context, t SlotTarget, repo string, s scheduler.Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restored = append(r.restored, t.Machine+"/"+repo+"@"+s.Provider)
	return nil
}

func TestSaveBeforeStopAndMove(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	slots := &recordingSlots{}
	o.Slots = slots
	o.Stores = &StoreSet{Pool: "p", Override: &store.Config{Name: "storage", Dir: t.TempDir()}}
	ctx := context.Background()
	l, err := o.Acquire(ctx, req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o.Release(l.ID)
	id := l.MachineID
	idle := time.Now().Add(-time.Minute).Unix()
	f.mu.Lock()
	st := f.status[id]
	st.IdleSince = idle
	st.Disk = &proto.DiskStatus{Total: 100 << 30, SlotBytes: map[string]int64{"r1": 1 << 30, "r2": 2 << 30}}
	// r2 was saved by the worker after the machine went idle: unchanged.
	st.Snapshots = []proto.SnapshotInfo{{Repo: "r2", Store: "storage", Bytes: 2 << 30, At: idle + 10}}
	f.mu.Unlock()
	o.refreshed = time.Time{}
	if err := o.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// Move: save on the source, restore that snapshot on the target.
	o.mu.Lock()
	tgt, _, _ := o.slotTargetLocked(id)
	o.mu.Unlock()
	if err := o.moveSlot(ctx, slots, tgt, scheduler.SlotAction{Kind: scheduler.SlotMove, Repo: "r1", From: id}); err != nil {
		t.Fatal(err)
	}
	if len(slots.restored) != 1 || slots.restored[0] != id+"/r1@fake" {
		t.Fatalf("restored %v", slots.restored)
	}
	slots.saved = nil
	o.stopMachine(ctx, id, "idle")
	if len(slots.saved) != 1 || slots.saved[0] != id+"/r1" {
		t.Fatalf("saved before stop %v, want only the changed slot r1", slots.saved)
	}
	o.mu.Lock()
	snaps := o.snapshots["r1"]
	o.mu.Unlock()
	if len(snaps) == 0 || snaps[0].Provider != "fake" || snaps[0].Store != "storage" {
		t.Fatalf("recorded snapshots %+v", snaps)
	}
}

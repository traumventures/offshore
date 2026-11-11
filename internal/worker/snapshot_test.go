package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/snapshot"
	"github.com/traumventures/cloudsourced/internal/store"
)

func snapWorker(t *testing.T) (*Worker, *store.Grant) {
	t.Helper()
	cache := t.TempDir()
	sl, _ := newSlots(filepath.Join(cache, "build"))
	w := &Worker{cfg: Config{CacheDir: cache, DepsDir: filepath.Join(cache, "deps"), BuildTTL: 24 * time.Hour}, slots: sl,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g, _ := store.NewGranter(context.Background(), store.Config{Name: "local", Dir: filepath.Join(t.TempDir(), "store"), Prefix: "cache"})
	gr, _ := g.Grant(context.Background(), []string{store.RepoScope("repo")}, time.Hour)
	return w, &gr
}

func TestSlotSaveAndRestore(t *testing.T) {
	a, gr := snapWorker(t)
	slot, _ := rustSlot(t, a.slots.dir)
	rlib := filepath.Join("target", "debug", "deps", "libnew-fedcba9876543210.rlib")
	want, _ := os.ReadFile(filepath.Join(slot, "data", rlib))
	wantFi, _ := os.Stat(filepath.Join(slot, "data", rlib))

	// Without a grant (no session brought one) the save fails.
	if res := a.slotOp(context.Background(), proto.SlotOp{Op: proto.SlotSave, Repo: "repo"}); res.Err == "" {
		t.Fatal("save without grant")
	}
	res := a.slotOp(context.Background(), proto.SlotOp{Op: proto.SlotSave, Repo: "repo", Store: gr})
	if res.Err != "" || res.Snapshot == nil || res.Snapshot.Store != "local" || res.Snapshot.Bytes == 0 || res.NewChunks == 0 {
		t.Fatalf("save: %+v", res)
	}
	if st := a.statusSnapshots(); len(st) != 1 || st[0].At != res.Snapshot.At {
		t.Fatalf("status snapshots %+v", st)
	}
	// A second save reads only changes: nothing new.
	if r2 := a.slotOp(context.Background(), proto.SlotOp{Op: proto.SlotSave, Repo: "repo", Store: gr}); r2.Err != "" || r2.NewChunks != 0 {
		t.Fatalf("second save %+v", r2)
	}

	for _, lazy := range []bool{false, true} {
		b, _ := snapWorker(t)
		defer func(f func(string, *snapshot.Filler) (func() error, error)) { mountFill = f }(mountFill)
		if lazy {
			// A stand-in for the FUSE layer: data points at the backing dir.
			mountFill = func(mp string, f *snapshot.Filler) (func() error, error) {
				backing := filepath.Join(filepath.Dir(mp), "fill")
				if err := os.Symlink(backing, mp); err != nil {
					return nil, err
				}
				return func() error { return os.Remove(mp) }, nil
			}
		} else {
			mountFill = func(string, *snapshot.Filler) (func() error, error) { return nil, errors.New("no fuse") }
		}
		r := b.slotOp(context.Background(), proto.SlotOp{Op: proto.SlotRestore, Repo: "repo", Snapshot: res.Snapshot, Store: gr})
		if r.Err != "" {
			t.Fatalf("restore (lazy %v): %s", lazy, r.Err)
		}
		if lazy {
			ref, _ := b.latestSlot("repo")
			b.fills.mu.Lock()
			fl := b.fills.m[ref.Path]
			b.fills.mu.Unlock()
			if fl == nil {
				t.Fatal("no fill in progress")
			}
			<-fl.done
			for i := 0; i < 100 && b.fillsLen() > 0; i++ {
				time.Sleep(10 * time.Millisecond)
			}
			if b.fillsLen() != 0 {
				t.Fatal("fill not finished")
			}
		}
		// A session leases the restored slot warm and sees the content
		// with the original mtimes (cargo compares them).
		slot2, warm, err := b.slots.lease("repo", 24*time.Hour)
		if err != nil || !warm {
			t.Fatalf("lease restored slot: %v warm %v", err, warm)
		}
		got, err := os.ReadFile(filepath.Join(slot2, "data", rlib))
		fi, _ := os.Stat(filepath.Join(slot2, "data", rlib))
		if err != nil || string(got) != string(want) || !fi.ModTime().Equal(wantFi.ModTime()) {
			t.Fatalf("restored content (lazy %v): %v, mtime %v want %v", lazy, err, fi.ModTime(), wantFi.ModTime())
		}
		m, _ := readMeta(slot2)
		if r, _ := restoreUsage(m); !r || m.Caches[0].Prune != "rust" || m.Runs != 0 {
			t.Fatalf("restored meta %+v", m)
		}
		if fi, err := os.Lstat(filepath.Join(slot2, "data")); err != nil || !fi.IsDir() {
			t.Fatal("data is not the filled directory")
		}
	}
}

func (s *Worker) statusSnapshots() []proto.SnapshotInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshots
}

func (s *Worker) fillsLen() int {
	s.fills.mu.Lock()
	defer s.fills.mu.Unlock()
	return len(s.fills.m)
}

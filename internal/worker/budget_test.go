package worker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/proto"
)

// The cache capacity bounds the resident data of all repositories
// together: once committed and written back, pages above it are reclaimed.
func TestEnforceCapacityMachineWide(t *testing.T) {
	a, gr := fsWorker(t, fsPlain, t.TempDir(), cacheFSOptions{})
	mt := time.Now().Add(-time.Hour).Truncate(time.Second)
	big := []byte(strings.Repeat("rlib", 1<<20)) // 4 MiB
	runSession(t, a, func(data string) {
		for _, n := range []string{"a", "b", "c"} {
			put(t, filepath.Join(data, "target/debug/deps/lib"+n+".rlib"), append([]byte(n), big...), mt)
		}
	})
	if _, err := a.saveSlot(context.Background(), "repo", gr); err != nil {
		t.Fatal(err)
	}
	before := a.cfs.resident()
	if before < 8<<20 {
		t.Fatalf("resident %d after save", before)
	}
	a.cfs.opt.Capacity = 0
	if n := a.cfs.enforceCapacity(context.Background()); n != 0 {
		t.Fatalf("unbounded capacity reclaimed %d", n)
	}
	a.cfs.opt.Capacity = before / 2
	freed := a.cfs.enforceCapacity(context.Background())
	if after := a.cfs.resident(); freed <= 0 || after > a.cfs.opt.Capacity {
		t.Fatalf("freed %d, resident %d > capacity %d", freed, after, a.cfs.opt.Capacity)
	}
}

// While sessions run and free drops below the reserve, the guard
// reclaims synchronously; when nothing can be freed the machine admits no
// sessions (draining, "disk full") until free is back at 1.5 x reserve.
func TestDiskGuardStopsAdmission(t *testing.T) {
	cache := t.TempDir()
	sl, err := newSlots(filepath.Join(cache, "build"))
	if err != nil {
		t.Fatal(err)
	}
	const g = int64(1) << 30
	free := 3 * g
	w := &Worker{cfg: Config{CacheDir: cache, BuildTTL: time.Hour}, slots: sl, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ready: true,
		sessions: map[string]*session{}, statfs: func() (int64, int64, bool) { return 100 * g, free, true }}
	// Reserve: floor max(5G, 3%, 100G - 90G) = 10G, plus 4G of growth.
	w.sessions["s1"] = &session{id: "s1", req: proto.Exec{SlotGrowth: 4 * g}}
	if r := w.reserve(100 * g); r != 14*g {
		t.Fatalf("reserve %d GiB, want 14", r/g)
	}
	w.guardTick(t.Context())
	if !w.diskFull {
		t.Fatal("not full below the reserve with nothing to reclaim")
	}
	free = 15 * g // above the reserve, below 1.5 x: hysteresis
	w.guardTick(t.Context())
	if !w.diskFull {
		t.Fatal("cleared below 1.5 x reserve")
	}
	free = 22 * g
	w.guardTick(t.Context())
	if w.diskFull {
		t.Fatal("not cleared at 1.5 x reserve")
	}
	delete(w.sessions, "s1")
	if st := w.statusOnly(); st.Draining || st.DrainReason != "" {
		t.Fatalf("status %+v", st)
	}
}

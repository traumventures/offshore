package worker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Reclaim starts at and frees down to 85% of the budget.
func TestDiskTargetLowWatermark(t *testing.T) {
	const g = int64(1) << 30
	if got := diskTarget(200 * g); got != 170*g {
		t.Fatalf("target %d GiB", got/g)
	}
}

// Above 97% of the disk the machine admits no sessions and reports
// draining ("disk full") until usage is below 95%.
func TestDiskFullStopsAdmission(t *testing.T) {
	cache := t.TempDir()
	sl, err := newSlots(filepath.Join(cache, "build"))
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{cfg: Config{CacheDir: cache, BuildTTL: time.Hour}, slots: sl, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ready: true}
	w.setDiskFull(0.96)
	if st := w.statusOnly(); st.Draining {
		t.Fatal("draining below the emergency threshold")
	}
	w.setDiskFull(0.975)
	st := w.statusOnly()
	if !st.Draining || st.DrainReason != "disk full" {
		t.Fatalf("status %+v", st)
	}
	w.setDiskFull(0.96) // hysteresis
	if !w.statusOnly().Draining {
		t.Fatal("cleared above 95%")
	}
	w.setDiskFull(0.94)
	if st := w.statusOnly(); st.Draining || st.DrainReason != "" {
		t.Fatalf("not cleared: %+v", st)
	}
}

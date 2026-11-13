//go:build storagesmoke

// Storage v2 smoke test on a real, disposable machine (root, /dev/fuse,
// cargo with network access to crates.io); never part of the regular
// test run:
//
//	CGO_ENABLED=0 go test -c -tags storagesmoke -o worker-smoke.test ./internal/worker
//	SMOKE_DIR=/smoke ./worker-smoke.test -test.run TestStorageSmoke -test.v
//
// It drives the worker's storage on the machine's real disk (statfs, no
// fakes) with kernel-overlay slots and real cargo builds: ledger
// accounting against statfs, the post-session trim of an overlay slot
// (deletions committed, flushed, rebased, pages dropped: statfs frees),
// reclaim in ledger order and the ENOSPC guard during a build on a disk
// filled to its reserve.

package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/store"
)

// lockedBuf is a log sink shared by the worker's goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

const smokeManifest = `[package]
name = "smoke"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = { version = "1"%s }
%s`

const smokeMain = `use serde::{Deserialize, Serialize};
#[derive(Serialize, Deserialize, Debug)]
struct P { x: u32, y: String }
fn main() { let p = P { x: 1, y: "a".into() }; println!("{}", serde_json::to_string(&p).unwrap()); }
`

func TestStorageSmoke(t *testing.T) {
	dir := os.Getenv("SMOKE_DIR")
	if dir == "" || os.Geteuid() != 0 {
		t.Skip("SMOKE_DIR and root required")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Fatal("no cargo")
	}
	ctx := context.Background()
	cache, storeDir := filepath.Join(dir, "cache"), filepath.Join(dir, "store")
	logs := &lockedBuf{}
	log := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
	sl, err := newSlots(filepath.Join(cache, "build"))
	if err != nil {
		t.Fatal(err)
	}
	mode := fsKernel
	cfs, err := newCacheFS(filepath.Join(cache, "anyfs"), cacheFSOptions{Mode: &mode, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if cfs.mode != fsKernel {
		t.Fatalf("slot mode %s, want kernel", cfs.mode)
	}
	sl.remove = cfs.removeSlot
	t.Cleanup(cfs.unmountAll)
	w := &Worker{cfg: Config{CacheDir: cache, DepsDir: filepath.Join(cache, "deps"), BuildTTL: 24 * time.Hour}, slots: sl, cfs: cfs, log: log}
	t.Cleanup(w.stopHeadFlushes)
	g, err := store.NewGranter(ctx, store.Config{Name: "local", Dir: storeDir, Prefix: "cache"})
	if err != nil {
		t.Fatal(err)
	}
	grant := func(repo string) *store.Grant {
		gr, err := g.Grant(ctx, []string{store.RepoScope(repo)}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		w.noteGrant(repo, &gr)
		return &gr
	}
	gRepo, gOther := grant("repo"), grant("other")
	cargoHome := filepath.Join(w.cfg.DepsDir, "cargo")

	// build runs cargo build in a leased slot of repo like a session
	// and records the run; extra is appended to the manifest.
	build := func(repo, features, extra string) (slot, out string, cerr error) {
		t.Helper()
		start := time.Now()
		slot, _, err := w.slots.lease(repo, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.cfs.prepare(ctx, w.slots, slot, repo, 24*time.Hour); err != nil {
			t.Fatal(err)
		}
		data := filepath.Join(slot, "data")
		src := filepath.Join(dir, "src-"+repo)
		os.MkdirAll(filepath.Join(src, "src"), 0o755)
		os.WriteFile(filepath.Join(src, "Cargo.toml"), []byte(strings.Replace(strings.Replace(smokeManifest, "%s", features, 1), "%s", extra, 1)), 0o644)
		if _, err := os.Stat(filepath.Join(src, "src", "main.rs")); err != nil {
			os.WriteFile(filepath.Join(src, "src", "main.rs"), []byte(smokeMain), 0o644)
		}
		w.cfs.sessionStarted(slot, nil)
		cmd := exec.Command("cargo", "build", "-j", "8")
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "CARGO_HOME="+cargoHome, "CARGO_TARGET_DIR="+filepath.Join(data, "target"), "CARGO_INCREMENTAL=1")
		b, cerr := cmd.CombinedOutput()
		time.Sleep(1100 * time.Millisecond) // last_used has second resolution
		w.slots.update(slot, func(m *slotMeta) {
			m.Caches = []slotCache{{Path: "target", Prune: config.PruneRust}}
		})
		reads, _, _, tracked := w.cfs.sessionEndedSince(slot, repo, start)
		if !tracked {
			reads = nil // as exec: atime
		}
		w.recordSlotRun(slot, start, reads)
		t.Logf("%s build in %s: %v, reads tracked %v (%d)", repo, slot, cerr, tracked, len(reads))
		w.slots.release(slot)
		return slot, string(b), cerr
	}
	stat := func() (int64, int64) {
		total, free, ok := w.diskStat()
		if !ok {
			t.Fatal("statfs")
		}
		return total, free
	}
	checkLedger := func(what string) *proto.DiskLedger {
		t.Helper()
		w.measureStorage(ctx)
		l, _ := w.ledger()
		if l == nil {
			t.Fatalf("%s: no ledger", what)
		}
		used := l.Total - l.Free
		classes := map[string]int64{}
		for i, e := range l.Reclaim {
			classes[e.Class] += e.Bytes
			if i > 0 && l.Reclaim[i-1].Cost/float64(l.Reclaim[i-1].Bytes) > e.Cost/float64(e.Bytes) {
				t.Errorf("%s: ledger not in ascending cost per byte at %d", what, i)
			}
		}
		cacheDisk := treeSize(cache)
		t.Logf("%s: total %d MiB, free %d MiB, used %d MiB, pinned %d MiB, reclaimable %d MiB (cache dir %d MiB), reserve %d MiB, classes %v",
			what, l.Total>>20, l.Free>>20, used>>20, l.Pinned>>20, l.Reclaimable()>>20, cacheDisk>>20, l.Reserve>>20, classes)
		if l.Pinned < 0 || l.Pinned+l.Reclaimable() != used {
			t.Errorf("%s: pinned %d + reclaimable %d != used %d", what, l.Pinned, l.Reclaimable(), used)
		}
		if l.Reclaimable() > cacheDisk+64<<20 {
			t.Errorf("%s: reclaimable %d exceeds the cache's disk %d (double counted)", what, l.Reclaimable(), cacheDisk)
		}
		return l
	}

	// 1. Cold builds of two repositories; both written back to the store.
	if _, out, err := build("other", "", ""); err != nil {
		t.Fatalf("other build: %v\n%s", err, out)
	}
	if _, err := w.saveSlot(ctx, "other", gOther); err != nil {
		t.Fatal(err)
	}
	slot, out, err := build("repo", "", "")
	if err != nil {
		t.Fatalf("repo build: %v\n%s", err, out)
	}
	if _, err := w.saveSlot(ctx, "repo", gRepo); err != nil {
		t.Fatal(err)
	}
	checkLedger("after cold builds")

	// 2. Kernel-overlay slot trim: a feature change rebuilds serde_json
	// under a new unit hash; after three unread runs the old unit is
	// garbage, and the trim after the session frees disk (statfs).
	if _, out, err := build("repo", `, features = ["preserve_order"]`, ""); err != nil {
		t.Fatalf("feature build: %v\n%s", err, out)
	}
	if _, err := w.saveSlot(ctx, "repo", gRepo); err != nil {
		t.Fatal(err)
	}
	m, _ := readMeta(slot)
	w.rebaseSlot(ctx, slotRef{Repo: "repo", Path: slot, Meta: m}) // the old unit lives in the lower now
	for i := 0; i < rustUnreadRuns; i++ {
		if _, out, err := build("repo", `, features = ["preserve_order"]`, ""); err != nil {
			t.Fatalf("no-op build: %v\n%s", err, out)
		}
	}
	m, _ = readMeta(slot)
	if m.Garbage <= 0 {
		t.Fatalf("no garbage after the feature change: %+v", m)
	}
	minSlotBudget = 1 << 20 // a small workload: its max_size is the budget
	w.slots.update(slot, func(x *slotMeta) { x.Caches[0].MaxBytes = x.Bytes + x.Bytes/10 })
	_, before := stat()
	head := w.cfs.flushedHeads()
	w.afterSession(slot, "repo")
	_, after := stat()
	t.Logf("trim: garbage %d KiB, statfs freed %d KiB", m.Garbage>>10, (after-before)>>10)
	if after-before < m.Garbage/2 {
		t.Errorf("kernel-overlay trim freed %d bytes of disk, garbage was %d", after-before, m.Garbage)
	}
	if fmtHeads(w.cfs.flushedHeads()) == fmtHeads(head) {
		t.Error("trim not flushed to the store head")
	}
	if _, out, err := build("repo", `, features = ["preserve_order"]`, ""); err != nil || strings.Contains(out, "Compiling serde_json") {
		t.Errorf("build after the trim: err %v, recompiled the live unit:\n%s", err, out)
	}
	checkLedger("after trim")

	// 3. ENOSPC guard and reclaim order: a dormant repository's tier of
	// 8 GiB (cheapest per byte) and a filler outside the cache leave the
	// reserve plus 100 MiB free; a larger build runs with the guard on.
	writeFallocPage(t, cfs.dir, "dormant", "dd01", 8<<30)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(cfs.dir, "repos", "dormant", "local"), old, old)
	os.Chtimes(filepath.Join(cfs.dir, "repos", "dormant"), old, old)
	w.mu.Lock()
	if w.sessions == nil {
		w.sessions = map[string]*session{}
	}
	w.sessions["smoke"] = &session{id: "smoke"}
	w.mu.Unlock()
	l := checkLedger("before fill")
	pos := map[string]int{}
	for i, e := range l.Reclaim {
		if _, ok := pos[e.Class+"/"+e.Repo]; !ok {
			pos[e.Class+"/"+e.Repo] = i
		}
	}
	if d, ok := pos[proto.ReclaimDormant+"/dormant"]; !ok || d > pos[proto.ReclaimSlot+"/other"] {
		t.Errorf("dormant tier not before the idle slot in the ledger: %+v", l.Reclaim)
	}
	total, free := stat()
	r := w.reserve(total)
	filler := filepath.Join(dir, "filler")
	f, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, free-r-100<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(filler)
	_, free = stat()
	t.Logf("filled: free %d MiB, reserve %d MiB", free>>20, r>>20)
	gctx, stop := context.WithCancel(ctx)
	var minFree int64 = free
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.diskGuard(gctx) }()
	go func() {
		defer wg.Done()
		for gctx.Err() == nil {
			if _, f := stat(); f < minFree {
				minFree = f
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	_, out, err = build("repo", `, features = ["preserve_order"]`, "regex = \"1\"\nsyn = { version = \"2\", features = [\"full\", \"extra-traits\", \"visit\", \"fold\"] }\n")
	time.Sleep(2 * guardInterval)
	stop()
	wg.Wait()
	w.mu.Lock()
	delete(w.sessions, "smoke")
	full := w.diskFull
	w.mu.Unlock()
	_, free = stat()
	t.Logf("guarded build: min free %d MiB, free after %d MiB, reserve %d MiB, disk full %v", minFree>>20, free>>20, r>>20, full)
	if err != nil || strings.Contains(out, "No space left") {
		t.Fatalf("build under the guard: %v\n%s", err, out)
	}
	if !strings.Contains(logs.String(), "disk below its reserve: urgent reclaim") {
		t.Error("the guard did not reclaim")
	}
	if _, err := os.Stat(filepath.Join(cfs.dir, "repos", "dormant")); err == nil {
		t.Error("dormant tier not reclaimed first")
	}
	if full || free < r {
		t.Errorf("guard left free %d below reserve %d (disk full %v)", free, r, full)
	}
	checkLedger("after guarded build")
}

func fmtHeads(hs []proto.LayerHead) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(h.Layer + ",")
	}
	return b.String()
}

// writeFallocPage is writePage of an allocated (unwritten) page.
func writeFallocPage(t *testing.T, anyfsDir, repo, key string, size int64) {
	t.Helper()
	p := filepath.Join(anyfsDir, "repos", repo, "local", "pages", key[:2], key)
	pp := filepath.Join(anyfsDir, "pool", key[:2], key)
	for _, d := range []string{filepath.Dir(p), filepath.Dir(pp)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o444)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Link(p, pp); err != nil {
		t.Fatal(err)
	}
}

// Command anyfsbench runs the storage tier benchmark against the real
// anyfs implementation (internal/anyfs): pages and layers in a directory
// store, a local tier and materialized files, the read-only FUSE lower
// under a kernel overlay (see bench.sh layouts E and F).
//
//	anyfsbench pack   -src DIR -store DIR            # commit DIR as a base layer
//	anyfsbench mount  -store DIR -cache DIR -mnt DIR [-latency 5ms -bw 200e6] [-prefetch 65536] [-warm] -stats FILE
//	anyfsbench commit -store DIR -cache DIR -dir MERGED   # commit the merged view as a layer
//
// mount writes its counters to -stats on SIGUSR1 and on exit; commit
// prints one JSON line.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/blobcache"
)

const head = "bench"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: anyfsbench pack|mount|commit ...")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	src := fl.String("src", "", "pack: source dir")
	storeDir := fl.String("store", "", "directory store")
	cache := fl.String("cache", "", "local tier + materialized files")
	mnt := fl.String("mnt", "", "mount point")
	dir := fl.String("dir", "", "commit: merged view to commit")
	latency := fl.Duration("latency", 0, "simulated per-object store latency")
	bw := fl.Float64("bw", 0, "simulated store bandwidth in bytes/s (0: unlimited)")
	prefetch := fl.Int64("prefetch", 0, "fault in files up to this size at mount")
	warm := fl.Bool("warm", false, "materialize every file before mounting (a warm slot)")
	stats := fl.String("stats", "", "stats file")
	to := fl.String("to", head, "commit: head to point at the new layer")
	passthrough := fl.Bool("passthrough", true, "FUSE passthrough")
	fl.Parse(args)
	ds, err := blobcache.NewDirStore(*storeDir, 24*time.Hour)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	var obj blobcache.Store = ds
	if *latency > 0 || *bw > 0 {
		obj = &slowStore{Store: ds, latency: *latency, bw: *bw}
	}
	st := anyfs.NewStore(obj, anyfs.StoreMedium())
	switch cmd {
	case "pack":
		begin := time.Now()
		tr, ss, err := anyfs.Scan(*src, anyfs.ScanOptions{Sink: func(k anyfs.Key, b []byte) error { return st.Put(ctx, k, b, anyfs.Dirty) }})
		if err != nil {
			log.Fatal(err)
		}
		l := anyfs.Commit(nil, anyfs.NewTree(nil), tr, time.Now(), nil)
		must(anyfs.SaveLayer(ctx, ds, l))
		must(anyfs.SetHead(ctx, ds, head, l.ID))
		out(map[string]any{"op": "pack", "seconds": time.Since(begin).Seconds(), "files": ss.Files, "bytes": ss.Bytes, "pages": ss.Pages, "page_bytes": ss.PageBytes})
	case "commit":
		chain := loadChain(ctx, ds)
		stack, files := openStack(*cache, st)
		begin := time.Now()
		base := chain.Tree()
		var keys []anyfs.Key
		var mu sync.Mutex
		tr, ss, err := anyfs.Scan(*dir, anyfs.ScanOptions{Prev: base, Sink: func(k anyfs.Key, b []byte) error {
			mu.Lock()
			keys = append(keys, k)
			mu.Unlock()
			return stack.Put(ctx, k, b, anyfs.Dirty)
		}})
		must(err)
		scan := time.Since(begin).Seconds()
		must(stack.(*anyfs.Link).FlushKeys(ctx, keys))
		l := anyfs.Commit(chain, base, tr, time.Now(), nil)
		must(anyfs.SaveLayer(ctx, ds, l))
		must(anyfs.SetHead(ctx, ds, *to, l.ID))
		_ = files
		out(map[string]any{"op": "commit", "seconds": time.Since(begin).Seconds(), "scan_seconds": scan, "files": ss.Files,
			"read_bytes": ss.Read, "reused": ss.Reused, "pages": ss.Pages, "page_bytes": ss.PageBytes, "nodes": len(l.Nodes), "delta": l.Delta})
	case "mount":
		chain := loadChain(ctx, ds)
		stack, files := openStack(*cache, st)
		ts := anyfs.NewTreeSource(chain.Tree(), files, stack)
		if *warm {
			begin := time.Now()
			n := ts.PrefetchSmall(ctx, 1<<62, 16)
			log.Printf("warm: materialized %d files in %s", n, time.Since(begin))
		}
		fs, err := anyfs.MountLower(*mnt, ts, anyfs.LowerOptions{AllowOther: true, PrefetchMax: *prefetch, NoPassthrough: !*passthrough})
		must(err)
		log.Printf("mounted %d nodes (%d bytes) at %s, passthrough=%v", ts.Tree().Len(), ts.Tree().Bytes(), *mnt, fs.Passthrough())
		dump := func() {
			if *stats == "" {
				return
			}
			s := anyfs.StatsOf(stack)
			f := files.Stats()
			b, _ := json.Marshal(map[string]any{"faults": s.Faults, "page_ins": s.PageIns, "page_in_bytes": s.PageInBytes,
				"resident_bytes": s.ResidentBytes + f.ResidentBytes, "file_faults": f.Faults, "file_fault_bytes": f.PageInBytes,
				"wait_seconds": waited.Seconds(), "ops": opCounts(fs.Ops()), "op_seconds": opSeconds(fs.Ops())})
			os.WriteFile(*stats, b, 0o644)
		}
		sig := make(chan os.Signal, 4)
		signal.Notify(sig, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)
		for s := range sig {
			dump()
			if s != syscall.SIGUSR1 {
				fs.Unmount()
				return
			}
		}
	default:
		log.Fatalf("unknown command %q", cmd)
	}
}

func opCounts(m map[string]anyfs.OpStat) map[string]int64 {
	out := map[string]int64{}
	for k, v := range m {
		out[k] = v.Count
	}
	return out
}

func opSeconds(m map[string]anyfs.OpStat) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		out[k] = v.Seconds
	}
	return out
}

func loadChain(ctx context.Context, ds blobcache.Store) anyfs.Chain {
	id, err := anyfs.Head(ctx, ds, head)
	must(err)
	c, err := anyfs.LoadChain(ctx, ds, id)
	must(err)
	return c
}

func openStack(cache string, st *anyfs.Store) (anyfs.Tier, *anyfs.Files) {
	local, err := anyfs.OpenLocal(anyfs.LocalOptions{Dir: filepath.Join(cache, "local")})
	must(err)
	stack, err := anyfs.Fold(nil, nil, local, st)
	must(err)
	files, err := anyfs.OpenFiles(filepath.Join(cache, "files"), nil)
	must(err)
	for _, d := range files.Digests() {
		files.SetBacked(d)
	}
	return stack, files
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func out(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

var (
	waitMu sync.Mutex
	waited time.Duration
)

// slowStore simulates an object store: per request latency and a shared
// pipe of limited bandwidth.
type slowStore struct {
	blobcache.Store
	latency time.Duration
	bw      float64
	mu      sync.Mutex
	free    time.Time
}

func (s *slowStore) Get(ctx context.Context, key string) (io.ReadCloser, blobcache.Info, error) {
	begin := time.Now()
	rc, info, err := s.Store.Get(ctx, key)
	if err != nil {
		return rc, info, err
	}
	d := s.latency
	if s.bw > 0 {
		s.mu.Lock()
		now := time.Now()
		start := s.free
		if start.Before(now) {
			start = now
		}
		s.free = start.Add(time.Duration(float64(info.Size) / s.bw * float64(time.Second)))
		d += s.free.Sub(now)
		s.mu.Unlock()
	}
	time.Sleep(d)
	waitMu.Lock()
	waited += time.Since(begin)
	waitMu.Unlock()
	return rc, info, nil
}

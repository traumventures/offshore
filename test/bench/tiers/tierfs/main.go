// Command tierfs is a measurement prototype (not production code) for mixing
// storage tiers under a Rust build: a read-only manifest+blob Layer served
// over FUSE (lower of a kernel overlayfs), and a read-write loopback FUSE.
// Both use FUSE passthrough when the kernel and privileges allow it.
//
//	tierfs pack  -src DIR -store DIR [-link]
//	tierfs ro    -store DIR [-cache DIR -latency 5ms -bw 200e6] -mnt DIR -stats FILE
//	tierfs rw    -backing DIR -mnt DIR -stats FILE [-writeback]
//
// SIGUSR1 writes the op counters to -stats; SIGTERM/SIGINT unmounts.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tierfs pack|ro|rw ...")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	src := fl.String("src", "", "pack: source dir")
	store := fl.String("store", "", "blob store dir (manifest.json inside)")
	link := fl.Bool("link", false, "pack: hardlink files into the store instead of copying")
	cache := fl.String("cache", "", "ro: local CAS dir; empty = open store blobs in place")
	latency := fl.Duration("latency", 0, "ro: simulated per-object fetch latency")
	bw := fl.Float64("bw", 0, "ro: simulated store bandwidth in bytes/s (0 = unlimited)")
	parallel := fl.Int("parallel", 16, "ro: concurrent fetches")
	backing := fl.String("backing", "", "rw: backing dir")
	mnt := fl.String("mnt", "", "mount point")
	stats := fl.String("stats", "", "stats file written on SIGUSR1 and exit")
	passthrough := fl.Bool("passthrough", true, "use FUSE passthrough when available")
	writeback := fl.Bool("writeback", false, "rw: request writeback cache")
	timeout := fl.Duration("timeout", time.Hour, "entry/attr timeout")
	noFlush := fl.Bool("noflush", false, "answer FLUSH with ENOSYS (kernel stops sending it)")
	noXattr := fl.Bool("noxattr", false, "answer GETXATTR/LISTXATTR with ENOSYS (kernel stops sending them)")
	prefetch := fl.Int64("prefetch-max-size", 0, "ro: after mount, fetch all blobs up to this size in the background")
	fl.Parse(args)

	switch cmd {
	case "pack":
		if err := pack(*src, *store, filepath.Join(*store, "manifest.json"), *link, 8); err != nil {
			log.Fatal(err)
		}
		return
	case "ro", "rw":
	default:
		log.Fatalf("unknown command %q", cmd)
	}

	opts := &fs.Options{EntryTimeout: timeout, AttrTimeout: timeout, NegativeTimeout: timeout}
	opts.MountOptions = fuse.MountOptions{
		AllowOther: true, FsName: "tierfs", Name: "tierfs",
		DirectMount: true, DirectMountStrict: true,
		MaxWrite: 1 << 20, MaxBackground: 64, EnableSymlinkCaching: true,
	}
	if !*passthrough {
		opts.DisabledCapabilities |= fuse.CAP_PASSTHROUGH
	}
	var root fs.InodeEmbedder
	var blobs *blobTiers
	if cmd == "ro" {
		m, err := loadManifest(filepath.Join(*store, "manifest.json"))
		if err != nil {
			log.Fatal(err)
		}
		if *cache != "" {
			os.MkdirAll(*cache, 0o755)
		}
		blobs = newBlobTiers(*store, *cache, *latency, *bw, *parallel)
		root = &roRoot{m: m, blobs: blobs, passthrough: *passthrough}
		opts.Options = append(opts.Options, "ro")
	} else {
		r, err := fs.NewLoopbackRoot(*backing)
		if err != nil {
			log.Fatal(err)
		}
		root = r
		if *writeback {
			opts.ExtraCapabilities |= fuse.CAP_WRITEBACK_CACHE
		}
	}
	raw := &countFS{RawFileSystem: fs.NewNodeFS(root, opts), blobs: blobs, noFlush: *noFlush, noXattr: *noXattr}
	srv, err := fuse.NewServer(raw, *mnt, &opts.MountOptions)
	if err != nil {
		log.Fatal(err)
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		log.Fatal(err)
	}
	if blobs != nil && *prefetch > 0 {
		go prefetchSmall(root.(*roRoot).m, blobs, *prefetch, *parallel)
	}
	ks := srv.KernelSettings()
	log.Printf("mounted %s at %s; kernel offers passthrough=%v writeback=%v", cmd, *mnt,
		ks.Flags64()&fuse.CAP_PASSTHROUGH != 0, ks.Flags64()&fuse.CAP_WRITEBACK_CACHE != 0)

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)
	for s := range sig {
		if *stats != "" {
			if err := raw.dump(*stats); err != nil {
				log.Print(err)
			}
		}
		if s != syscall.SIGUSR1 {
			srv.Unmount()
			return
		}
	}
}

// prefetchSmall pulls every blob up to max bytes, smallest first. Cargo reads
// many tiny fingerprint/dep-info files serially; paying the per-object store
// latency for each of them dominates a restore otherwise.
func prefetchSmall(m *Manifest, b *blobTiers, max int64, parallel int) {
	start := time.Now()
	var es []*Entry
	for i := range m.Entries {
		if e := &m.Entries[i]; e.Type == 'f' && e.Size <= max {
			es = append(es, e)
		}
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Size < es[j].Size })
	ch := make(chan *Entry)
	var wg sync.WaitGroup
	for w := 0; w < parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range ch {
				b.path(e.Digest, e.Size)
			}
		}()
	}
	for _, e := range es {
		ch <- e
	}
	close(ch)
	wg.Wait()
	log.Printf("prefetched %d blobs <= %d bytes in %s", len(es), max, time.Since(start))
}

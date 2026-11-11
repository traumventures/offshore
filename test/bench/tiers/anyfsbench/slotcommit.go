package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/blobcache"
)

// slotCommit measures the commit of a fresh build slot (the store head
// after a cold build): a synthetic session writes a Rust-like target
// (many small files, most bytes in large rlibs/binaries) into an upper
// directory at a build's write rate, then the session is committed into
// a local page tier over a bandwidth-limited directory store and the
// head flushed.
//
//	anyfsbench slotcommit -mode before|after -upper DIR -cache DIR -store DIR -bytes 4e9 [-bw 500e6] [-rate 40e6]
//
// before: the commit as of main 1c4f052 (files read one at a time after
// the session, pages written back by the flush). after: files hashed in
// the background while the session writes them (anyfs.Hasher, as on
// fanotify close_write, early writeback of their pages), the commit
// looks their digests up (ChangeSet.Resolve), reads the rest with
// parallel workers and streams new pages to the store while reading
// (Link.Stream). Generated files are fsynced and dropped from the page
// cache (the build's memory pressure evicts them on a worker).
func slotCommit(args []string) {
	fl := flag.NewFlagSet("slotcommit", flag.ExitOnError)
	mode := fl.String("mode", "after", "before|after")
	upper := fl.String("upper", "", "session output directory (created)")
	cache := fl.String("cache", "", "local page tier")
	storeDir := fl.String("store", "", "directory store")
	total := fl.Float64("bytes", 4e9, "bytes the session writes")
	bw := fl.Float64("bw", 500e6, "store upload bandwidth (bytes/s, 0: unlimited)")
	rate := fl.Float64("rate", 40e6, "session write rate (bytes/s, 0: unpaced)")
	workers := fl.Int("workers", anyfs.DefaultReadWorkers, "after: commit read workers")
	seed := fl.Uint64("seed", 1, "content seed")
	fl.Parse(args)
	ctx := context.Background()
	ds, err := blobcache.NewDirStore(*storeDir, 24*time.Hour)
	must(err)
	obj := &uploadLimit{Store: ds, bw: *bw}
	store := anyfs.NewStore(obj, anyfs.StoreMedium())
	local, err := anyfs.OpenLocal(anyfs.LocalOptions{Dir: filepath.Join(*cache, "local")})
	must(err)
	expire := time.Hour // before: commit pages wait for the flush
	if *mode == "after" {
		expire = time.Second // early writeback class (immutable rlibs, binaries)
	}
	link := anyfs.NewLink(local, store, anyfs.Policy{Write: anyfs.WriteBack, DirtyExpire: expire, Interval: 200 * time.Millisecond}, nil)
	link.Start()
	defer link.Close()

	var h *anyfs.Hasher
	if *mode == "after" {
		h = anyfs.NewHasher(anyfs.HasherOptions{Sink: func(k anyfs.Key, b []byte) error { return link.Put(ctx, k, b, anyfs.Dirty) }})
		defer h.Close()
	}
	files := genPlan(int64(*total), *seed)
	begin := time.Now()
	written := genSession(*upper, files, *rate, *seed, func(rel string) {
		if h != nil {
			h.Queue(rel, filepath.Join(*upper, rel))
		}
	})
	session := time.Since(begin).Seconds()
	upAtEnd := obj.uploaded()
	var hs anyfs.HasherStats
	if h != nil {
		hs = h.Stats()
	}

	// Commit: plan (stats), resolve, read, layer; then flush the head.
	begin = time.Now()
	cs, err := anyfs.PlanChanges(*upper, anyfs.ChangeOptions{})
	must(err)
	var resolved int
	if h != nil {
		resolved = len(cs.Resolve(func(n *anyfs.Node) bool { return h.Lookup(n, filepath.Join(*upper, n.Path)) }))
	}
	opt := anyfs.ScanOptions{Workers: 1, Sink: func(k anyfs.Key, b []byte) error { return link.Put(ctx, k, b, anyfs.Dirty) }}
	var stream *anyfs.Stream
	if *mode == "after" {
		opt.Workers = *workers
		stream = link.Stream(ctx)
		put := func(k anyfs.Key, b []byte) error { return stream.Put(k, b) }
		opt.FileSink = func(string) anyfs.RangeSink {
			return func(k anyfs.Key, b []byte, _ anyfs.Range) error { return put(k, b) }
		}
	}
	tree, st, err := cs.Tree(*upper, opt)
	must(err)
	if stream != nil {
		must(stream.Wait().Err)
	}
	l := anyfs.Commit(nil, anyfs.NewTree(nil), tree, time.Now(), nil)
	commit := time.Since(begin).Seconds()
	must(link.FlushKeys(ctx, tree.Keys()))
	must(anyfs.SaveLayer(ctx, ds, l))
	must(anyfs.SetHead(ctx, ds, head, l.ID))
	headSecs := time.Since(begin).Seconds()
	out(map[string]any{"op": "slotcommit", "mode": *mode, "files": st.Files, "bytes": written, "session_seconds": session,
		"uploaded_during_session": upAtEnd, "hashed_in_session": hs.HashedBytes, "hash_stale": hs.Stale, "resolved_files": resolved,
		"known_bytes": st.KnownRead, "commit_read_bytes": st.Read, "commit_seconds": commit, "head_seconds": headSecs,
		"uploaded": obj.uploaded(), "workers": opt.Workers, "store_bw": *bw})
}

type genFile struct {
	rel  string
	size int64
}

// genPlan makes a Rust-like target of about total bytes: 4% in small
// files (fingerprints, .d, .rmeta, incremental pieces <= 64 KiB), 16%
// in medium ones (64 KiB..1 MiB), the rest in large rlibs and binaries
// (1..200 MiB, a few test executables of 100+ MiB).
func genPlan(total int64, seed uint64) []genFile {
	r := rand.New(rand.NewPCG(seed, 7))
	var out []genFile
	add := func(budget int64, dir string, min, max int64) {
		for i := 0; budget > 0; i++ {
			sz := min + r.Int64N(max-min)
			if sz > budget {
				sz = budget
			}
			out = append(out, genFile{fmt.Sprintf("target/debug/%s/%03d/f%06d", dir, i%97, i), sz})
			budget -= sz
		}
	}
	add(total*4/100, ".fingerprint", 64, 64<<10)
	add(total*16/100, "incremental", 64<<10, 1<<20)
	add(total*70/100, "deps", 1<<20, 60<<20)
	add(total-total*90/100, "bins", 60<<20, 200<<20)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// genSession writes the files at rate (bytes/s) with incompressible
// content, fsyncs and drops each from the page cache, and reports each
// closed file (fanotify close_write).
func genSession(dir string, files []genFile, rate float64, seed uint64, closed func(rel string)) int64 {
	var total int64
	begin := time.Now()
	buf := make([]byte, 1<<20)
	r := rand.New(rand.NewPCG(seed, 11))
	for _, f := range files {
		p := filepath.Join(dir, f.rel)
		must(os.MkdirAll(filepath.Dir(p), 0o755))
		fh, err := os.Create(p)
		must(err)
		for left := f.size; left > 0; {
			n := min(left, int64(len(buf)))
			for i := 0; i+8 <= int(n); i += 8 {
				v := r.Uint64()
				buf[i], buf[i+1], buf[i+2], buf[i+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
				buf[i+4], buf[i+5], buf[i+6], buf[i+7] = byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56)
			}
			_, err := fh.Write(buf[:n])
			must(err)
			left -= n
			total += n
			if rate > 0 {
				if ahead := time.Duration(float64(total)/rate*float64(time.Second)) - time.Since(begin); ahead > 0 {
					time.Sleep(ahead)
				}
			}
		}
		must(fh.Sync())
		unix.Fadvise(int(fh.Fd()), 0, 0, unix.FADV_DONTNEED)
		must(fh.Close())
		closed(f.rel)
	}
	return total
}

// uploadLimit simulates the object store's upload bandwidth: one shared
// pipe of bw bytes/s.
type uploadLimit struct {
	blobcache.Store
	bw   float64
	mu   sync.Mutex
	free time.Time
	up   int64
}

func (u *uploadLimit) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if u.bw > 0 {
		u.mu.Lock()
		now := time.Now()
		start := u.free
		if start.Before(now) {
			start = now
		}
		u.free = start.Add(time.Duration(float64(len(b)) / u.bw * float64(time.Second)))
		d := u.free.Sub(now)
		u.mu.Unlock()
		time.Sleep(d)
	}
	u.mu.Lock()
	u.up += int64(len(b))
	u.mu.Unlock()
	return u.Store.Put(ctx, key, bytesReader(b), int64(len(b)))
}

func (u *uploadLimit) uploaded() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.up
}

func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

package anyfs

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestCommitLatency replays a build into a scratch directory and commits
// it the way a slot does: files are queued to a background Hasher as
// they are written (at OFFSHORE_BENCH_WRITE_MBS, default 100 MB/s), then
// the session ends and the commit looks the hashed files up and reads the
// rest. It reports the time from the session's end to the committed tree.
//
//	OFFSHORE_BENCH_SRC=<dir[:dir...]> OFFSHORE_BENCH_SCRATCH=<dir>
//	[OFFSHORE_BENCH_COPIES=n] go test -run TestCommitLatency -v
func TestCommitLatency(t *testing.T) {
	srcs := filepath.SplitList(os.Getenv("OFFSHORE_BENCH_SRC"))
	scratch := os.Getenv("OFFSHORE_BENCH_SCRATCH")
	if len(srcs) == 0 || scratch == "" {
		t.Skip("OFFSHORE_BENCH_SRC/OFFSHORE_BENCH_SCRATCH not set")
	}
	copies, _ := strconv.Atoi(os.Getenv("OFFSHORE_BENCH_COPIES"))
	copies = max(copies, 1)
	mbs, _ := strconv.ParseFloat(os.Getenv("OFFSHORE_BENCH_WRITE_MBS"), 64)
	if mbs <= 0 {
		mbs = 100
	}
	dir, err := os.MkdirTemp(scratch, "build")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	discard := func(Key, []byte) error { return nil }
	// Production settings: 32 MB/s under IO pressure;
	// OFFSHORE_BENCH_PRESSURE fixes the pressure seen.
	hopt := HasherOptions{Sink: discard, Settle: 200 * time.Millisecond, BytesPerSec: 32 << 20}
	if v := os.Getenv("OFFSHORE_BENCH_PRESSURE"); v != "" {
		p, _ := strconv.ParseFloat(v, 64)
		hopt.Pressure = func() (float64, bool) { return p, true }
	}
	h := NewHasher(hopt)
	defer h.Close()

	begin := time.Now()
	var written int64
	for c := range copies {
		for i, src := range srcs {
			root := filepath.Join(dir, strconv.Itoa(c), strconv.Itoa(i))
			err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}
				rel, _ := filepath.Rel(dir, filepath.Join(root, p[len(src):]))
				n, err := benchCopy(p, filepath.Join(dir, rel))
				if err != nil {
					return err
				}
				written += n
				h.Queue(filepath.ToSlash(rel), filepath.Join(dir, rel))
				if ahead := time.Duration(float64(written)/(mbs*1e6)*float64(time.Second)) - time.Since(begin); ahead > 0 {
					time.Sleep(ahead) // the build's write rate
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	build := time.Since(begin)
	time.Sleep(time.Second) // the session ends, the last files settle
	end := time.Now()
	cs, err := PlanChanges(dir, ChangeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cs.Resolve(func(n *Node) bool { return h.Lookup(n, filepath.Join(dir, filepath.FromSlash(n.Path))) })
	h.Forget(cs.Read)
	_, st, err := cs.Tree(dir, ScanOptions{Sink: discard})
	if err != nil {
		t.Fatal(err)
	}
	commit := time.Since(end)
	t.Logf("build wrote %.0f MB in %v; background hashed %.0f MB; commit read %.0f MB in %v (%.0f MB/s); session end to tree %v",
		float64(written)/1e6, build.Round(time.Millisecond), float64(st.KnownRead)/1e6, float64(st.Read)/1e6,
		commit.Round(time.Millisecond), float64(st.Read)/1e6/commit.Seconds(), commit.Round(time.Millisecond))
}

func benchCopy(src, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

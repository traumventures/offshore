package sim

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
)

// Spec is one simulation of a batch: a trace under options. Every spec
// needs its own stateful parts (Options.Estimator, Policy state): specs
// run concurrently.
type Spec struct {
	Trace   Trace
	Options Options
}

// DefaultParallel is the default number of concurrent simulations: one
// less than the usable CPUs (the machine stays responsive), at least 1.
func DefaultParallel() int {
	n := min(runtime.NumCPU(), runtime.GOMAXPROCS(0)) - 1
	return max(n, 1)
}

// RunAll simulates specs on up to parallel goroutines (<= 0:
// DefaultParallel) and returns their reports in spec order. A simulation
// is single threaded and deterministic, so the reports do not depend on
// parallel.
func RunAll(specs []Spec, parallel int) []Report {
	if parallel <= 0 {
		parallel = DefaultParallel()
	}
	parallel = min(parallel, len(specs))
	out := make([]Report, len(specs))
	if parallel <= 1 {
		for i, sp := range specs {
			out[i] = Run(sp.Trace, sp.Options)
		}
		return out
	}
	work := make(chan int)
	var wg sync.WaitGroup
	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				out[i] = Run(specs[i].Trace, specs[i].Options)
			}
		}()
	}
	// Longest first (by jobs), so a long run does not start last.
	order := make([]int, len(specs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return len(specs[b].Trace.Jobs) - len(specs[a].Trace.Jobs) })
	for _, i := range order {
		work <- i
	}
	close(work)
	wg.Wait()
	return out
}

// CacheDir is where Cached keeps parsed traces: $OFFSHORE_SIM_CACHE, else
// the user cache directory's offshore/sim ("" disables the cache).
func CacheDir() string {
	if d, ok := os.LookupEnv("OFFSHORE_SIM_CACHE"); ok {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "offshore", "sim")
}

// cacheVersion changes when the cached representation (Trace) changes.
const cacheVersion = "trace-v1"

// Cached returns the trace parse returns for the file at path, from a
// binary (gob) cache in CacheDir keyed by the file's path, size and
// modification time and by key (the parse options). A cache miss parses
// and stores the result; cache errors only cost the parse.
func Cached(path, key string, parse func() (Trace, error)) (Trace, error) {
	dir := CacheDir()
	st, err := os.Stat(path)
	if dir == "" || err != nil {
		return parse()
	}
	abs, _ := filepath.Abs(path)
	h := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d\x00%d\x00%s", cacheVersion, abs, st.Size(), st.ModTime().UnixNano(), key))
	file := filepath.Join(dir, hex.EncodeToString(h[:16])+".gob")
	if f, err := os.Open(file); err == nil {
		var t Trace
		err := gob.NewDecoder(f).Decode(&t)
		f.Close()
		if err == nil {
			return t, nil
		}
	}
	t, err := parse()
	if err != nil {
		return t, err
	}
	var buf bytes.Buffer
	if gob.NewEncoder(&buf).Encode(t) == nil && os.MkdirAll(dir, 0o755) == nil {
		writeAtomic(file, &buf)
	}
	return t, nil
}

// writeAtomic writes r to path through a temporary file (concurrent
// readers never see a partial cache entry).
func writeAtomic(path string, r io.Reader) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	_, err = io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}

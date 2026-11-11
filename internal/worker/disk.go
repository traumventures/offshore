package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
)

// treeSize returns the bytes allocated by the files below dir (hard links
// counted once). It walks directories in parallel: a Rust target directory
// has 10^5 files.
func treeSize(dir string) int64 {
	var total atomic.Int64
	var seen sync.Map // dev/ino of multiply linked files
	sem := make(chan struct{}, 4*runtime.NumCPU())
	var wg sync.WaitGroup
	var walk func(d string)
	walk = func(d string) {
		defer wg.Done()
		sem <- struct{}{}
		des, _ := os.ReadDir(d)
		<-sem
		for _, de := range des {
			p := filepath.Join(d, de.Name())
			if de.IsDir() {
				wg.Add(1)
				go walk(p)
				continue
			}
			if fi, err := de.Info(); err == nil {
				total.Add(allocated(fi, &seen))
			}
		}
	}
	wg.Add(1)
	walk(dir)
	wg.Wait()
	return total.Load()
}

// allocated returns the disk bytes of a file; a file with several links
// counts once per seen map.
func allocated(fi os.FileInfo, seen *sync.Map) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Size()
	}
	if st.Nlink > 1 {
		key := [2]uint64{uint64(st.Dev), st.Ino}
		if _, dup := seen.LoadOrStore(key, true); dup {
			return 0
		}
	}
	return st.Blocks * 512
}

// fsUsage returns the total and used bytes of the filesystem holding dir.
func fsUsage(dir string) (total, used int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	total = int64(st.Blocks) * st.Bsize
	used = total - int64(st.Bfree)*st.Bsize
	return total, used, true
}

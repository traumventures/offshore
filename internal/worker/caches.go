package worker

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/sandbox"
)

// cacheBinds returns the bind mounts of the session's slot and machine
// caches: slot caches from the leased slot's data directory, machine
// caches from the machine cache directory (absolute paths, $VAR and ~
// expanded with the session environment; relative paths are inside the
// repository and kept per repository).
func (s *Worker) cacheBinds(req proto.Exec, slot string, env []string, base []sandbox.Bind, out io.Writer) ([]sandbox.Bind, error) {
	data := filepath.Join(slot, "data")
	var binds []sandbox.Bind
	for _, c := range req.Caches {
		switch c.Scope {
		case config.ScopeSlot:
			for _, p := range c.Paths {
				p = filepath.Clean(p)
				binds = append(binds, sandbox.Bind{Src: filepath.Join(data, p), Dst: filepath.Join(req.Repo, p)})
			}
		case config.ScopeMachine:
			if c.Digest != nil {
				b, err := s.digestBinds(req, c, env, slot, append(append([]sandbox.Bind{}, base...), binds...), out)
				if err != nil {
					return nil, fmt.Errorf("cache.%s: %w", c.Name, err)
				}
				binds = append(binds, b...)
				continue
			}
			for _, p := range c.Paths {
				src, dst, ok := s.machineCachePath(req, p, env)
				if ok {
					if c.Prune != config.PruneNone {
						s.machineBudgets.note(src, c.MaxBytes)
					}
					binds = append(binds, sandbox.Bind{Src: src, Dst: dst})
				}
			}
		}
	}
	return binds, nil
}

// machineCachePath maps a machine cache path to its directory on the
// machine (src) and its location in the session (dst).
func (s *Worker) machineCachePath(req proto.Exec, p string, env []string) (src, dst string, ok bool) {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "$") || strings.HasPrefix(p, "~/") {
		dst = expandPath(p, env)
		if !filepath.IsAbs(dst) || dst == "/" {
			return "", "", false
		}
		return filepath.Join(s.cfg.DepsDir, escapePath(dst)), dst, true
	}
	if req.RepoID == "" || !filepath.IsLocal(p) {
		return "", "", false
	}
	p = filepath.Clean(p)
	return filepath.Join(s.cfg.DepsDir, "repos", req.RepoID, escapePath(p)), filepath.Join(req.Repo, p), true
}

func escapePath(p string) string { return strings.ReplaceAll(strings.Trim(p, "/"), "/", "__") }

// machineCacheDirs remembers the directories and budgets of plain machine
// caches seen in requests (src dir -> max bytes) for pruning.
type machineCacheBudgets struct {
	mu   sync.Mutex
	dirs map[string]int64
}

func (b *machineCacheBudgets) note(dir string, max int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dirs == nil {
		b.dirs = map[string]int64{}
	}
	b.dirs[dir] = max
}

// pruneLRU deletes the least recently accessed files below dir until it
// holds at most max bytes. It returns the bytes freed.
func pruneLRU(dir string, max int64) int64 {
	type file struct {
		path  string
		at    time.Time
		bytes int64
	}
	var mu sync.Mutex
	var files []file
	var total int64
	var seen sync.Map
	walkFiles(dir, func(p string, fi os.FileInfo) {
		n := allocated(fi, &seen)
		at, mt := fileTimes(fi)
		if mt.After(at) {
			at = mt
		}
		mu.Lock()
		files = append(files, file{p, at, n})
		total += n
		mu.Unlock()
	})
	if total <= max {
		return 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	var freed int64
	for _, f := range files {
		if total-freed <= max {
			break
		}
		if os.Remove(f.path) == nil {
			freed += f.bytes
		}
	}
	return freed
}

// pruneMachineCaches enforces the max_size of plain machine caches while
// no session runs (they are shared, so a running session may use them).
func (s *Worker) pruneMachineCaches() {
	s.mu.Lock()
	idle := len(s.sessions) == 0
	s.mu.Unlock()
	if !idle {
		return
	}
	s.machineBudgets.mu.Lock()
	dirs := maps.Clone(s.machineBudgets.dirs)
	s.machineBudgets.mu.Unlock()
	for dir, max := range dirs {
		if max > 0 {
			if n := pruneLRU(dir, max); n > 0 {
				s.log.Info("pruned machine cache", "dir", dir, "freed", n)
			}
		}
	}
}

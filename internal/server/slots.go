package server

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// slots manages warm, worker-local build directories per repository.
// A session leases one slot exclusively, so concurrent builds of the same
// repository never share a build directory. New slots are cloned from the
// most recently used one with reflinks when the filesystem supports it.
// Slot data never outlives its TTL, measured from the creation of the
// oldest data it contains.
type slots struct {
	dir    string
	mu     sync.Mutex
	leased map[string]bool
}

type slotMeta struct {
	Created  int64 `json:"created"`   // unix seconds of the oldest contained data
	LastUsed int64 `json:"last_used"` // unix seconds
}

func newSlots(dir string) (*slots, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &slots{dir: dir, leased: map[string]bool{}}, nil
}

func readMeta(slot string) (slotMeta, error) {
	var m slotMeta
	b, err := os.ReadFile(filepath.Join(slot, "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func writeMeta(slot string, m slotMeta) error {
	b, _ := json.Marshal(m)
	tmp := filepath.Join(slot, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(slot, "meta.json"))
}

// lease returns an exclusive slot directory for repoID. The data directory
// is slot/data.
func (s *slots) lease(repoID string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repoDir := filepath.Join(s.dir, repoID)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return "", err
	}
	now := time.Now()
	des, _ := os.ReadDir(repoDir)
	type cand struct {
		path string
		meta slotMeta
	}
	var free, all []cand
	used := map[int]bool{}
	for _, de := range des {
		n, err := strconv.Atoi(de.Name())
		if err != nil || !de.IsDir() {
			continue
		}
		p := filepath.Join(repoDir, de.Name())
		m, err := readMeta(p)
		if err != nil || now.Sub(time.Unix(m.Created, 0)) >= ttl {
			if !s.leased[p] {
				os.RemoveAll(p) // expired: never reuse old build data
			}
			if s.leased[p] {
				used[n] = true
			}
			continue
		}
		used[n] = true
		all = append(all, cand{p, m})
		if !s.leased[p] {
			free = append(free, cand{p, m})
		}
	}
	byRecent := func(c []cand) {
		sort.Slice(c, func(i, j int) bool { return c[i].meta.LastUsed > c[j].meta.LastUsed })
	}
	if len(free) > 0 {
		byRecent(free)
		c := free[0]
		c.meta.LastUsed = now.Unix()
		if err := writeMeta(c.path, c.meta); err != nil {
			return "", err
		}
		s.leased[c.path] = true
		return c.path, nil
	}
	n := 0
	for used[n] {
		n++
	}
	p := filepath.Join(repoDir, strconv.Itoa(n))
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	meta := slotMeta{Created: now.Unix(), LastUsed: now.Unix()}
	// Warm start: clone the most recently used slot if reflinks are cheap.
	byRecent(all)
	cloned := false
	if len(all) > 0 {
		src := filepath.Join(all[0].path, "data")
		if _, err := os.Stat(src); err == nil {
			cmd := exec.Command("cp", "-a", "--reflink=always", src, filepath.Join(p, "data"))
			if err := cmd.Run(); err == nil {
				meta.Created = all[0].meta.Created
				cloned = true
			} else {
				os.RemoveAll(filepath.Join(p, "data"))
			}
		}
	}
	if !cloned {
		if err := os.MkdirAll(filepath.Join(p, "data"), 0o755); err != nil {
			return "", err
		}
	}
	if err := writeMeta(p, meta); err != nil {
		return "", err
	}
	s.leased[p] = true
	return p, nil
}

func (s *slots) release(slot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leased, slot)
	if m, err := readMeta(slot); err == nil {
		m.LastUsed = time.Now().Unix()
		writeMeta(slot, m)
	}
}

// latest returns the most recently used unexpired slot of a repo.
func (s *slots) latest(repoID string, ttl time.Duration) (string, error) {
	repoDir := filepath.Join(s.dir, repoID)
	des, err := os.ReadDir(repoDir)
	if err != nil {
		return "", err
	}
	best, bestUsed := "", int64(-1)
	for _, de := range des {
		p := filepath.Join(repoDir, de.Name())
		m, err := readMeta(p)
		if err != nil || time.Since(time.Unix(m.Created, 0)) >= ttl {
			continue
		}
		if m.LastUsed > bestUsed {
			best, bestUsed = p, m.LastUsed
		}
	}
	if best == "" {
		return "", fmt.Errorf("no build slot for repository")
	}
	return best, nil
}

// gc deletes expired, unleased slots.
func (s *slots) gc(ttl time.Duration) (removed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repos, _ := os.ReadDir(s.dir)
	for _, r := range repos {
		repoDir := filepath.Join(s.dir, r.Name())
		des, _ := os.ReadDir(repoDir)
		for _, de := range des {
			p := filepath.Join(repoDir, de.Name())
			if s.leased[p] {
				continue
			}
			m, err := readMeta(p)
			if err != nil || time.Since(time.Unix(m.Created, 0)) >= ttl {
				os.RemoveAll(p)
				removed++
			}
		}
	}
	return removed
}

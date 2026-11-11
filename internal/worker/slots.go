package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/clock"
	"github.com/traumventures/offshore/internal/hostfs"
)

// slots manages warm, worker-local build directories per repository.
// A session leases one slot exclusively, so concurrent builds of the same
// repository never share a build directory. New slots start over the
// repository's committed layer chain (cachefs.go), not as copies.
// Slot data never outlives its TTL, measured from the creation of the
// oldest data it contains. The TTL is the repository's (sent with every
// session) and recorded per slot, so repositories with different TTLs can
// share a machine; the machine-wide TTL is an upper bound.
type slots struct {
	dir    string
	clock  clock.Clock // slot ages (nil: real time)
	mu     sync.Mutex
	leased map[string]bool
	// maint marks leased slots held by maintenance (commit, prune,
	// rebase) or by the end of a session (finishing) rather than a
	// running session: a session waits for them instead of starting
	// another slot without the latest session's state.
	maint map[string]bool
	freed *sync.Cond // broadcast when a slot is released (on mu)
	// pinned slots may be leased but never removed or pruned (a restore
	// is filling them through a FUSE mount).
	pinned map[string]bool
	// remove deletes a slot directory (unmounting it; default RemoveAll).
	remove func(string)
}

func (s *slots) removeDir(p string) {
	if s.remove != nil {
		s.remove(p)
		return
	}
	os.RemoveAll(p)
}

// maintWait bounds how long a session waits for maintenance to release
// the most recently used slot before it takes another one.
var maintWait = 2 * time.Minute

// releasedLocked forgets a slot's lease and wakes waiting sessions.
func (s *slots) releasedLocked(slot string) {
	delete(s.leased, slot)
	delete(s.maint, slot)
	if s.freed != nil {
		s.freed.Broadcast()
	}
}

// finishing marks a session's slot as held for its end (snapshot,
// release) only: the client already has the exit status and may start the
// next session, which waits for this slot rather than taking another.
func (s *slots) finishing(slot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leased[slot] {
		s.maint[slot] = true
	}
}

// busyLocked reports whether a slot must not be removed.
func (s *slots) busyLocked(p string) bool { return s.leased[p] || s.pinned[p] }

func (s *slots) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}

// uncommitted reports whether a slot has changes since its last commit.
func (m slotMeta) uncommitted() bool { return m.Leases != m.Clean }

type slotMeta struct {
	Created  int64 `json:"created"`       // unix seconds of the oldest contained data
	LastUsed int64 `json:"last_used"`     // unix seconds
	TTL      int64 `json:"ttl,omitempty"` // seconds; TTL of the last session (0 = machine TTL)
	// Bytes is the slot size measured when the last session ended (or
	// when it was cloned).
	Bytes int64 `json:"bytes,omitempty"`
	// Runs counts the sessions that used the slot; the read index
	// (reads.json) records per file the last run that read or wrote it.
	Runs int64 `json:"runs,omitempty"`
	// Caches are the slot-scope caches of the last session (paths and
	// prune policies), so the slot can be pruned without a request.
	Caches []slotCache `json:"caches,omitempty"`
	// Digests are the digests of machine caches linked into the slot
	// (cache name -> digest).
	Digests map[string]string `json:"digests,omitempty"`
	// Restored is the unix time the slot was restored from a snapshot
	// (0: built here); RestoreSeconds the time spent restoring so far.
	Restored       int64   `json:"restored,omitempty"`
	RestoreSeconds float64 `json:"restore_seconds,omitempty"`
	// Base is the committed layer the slot's lower shows, Head the layer
	// of its last commit (the merged view at Committed, unix seconds);
	// UpperBytes the size of the upper directory then.
	Base      string `json:"base,omitempty"`
	Head      string `json:"head,omitempty"`
	Committed int64  `json:"committed,omitempty"`
	// Leases counts the sessions that leased the slot (counted when the
	// lease starts, so a running session counts); Clean is the value of
	// Leases the last commit covers. The slot has uncommitted changes
	// while they differ: unlike comparing LastUsed with Committed (unix
	// seconds), this holds for sessions within the same second and for a
	// background commit that finishes while the next session runs.
	Leases     int64 `json:"leases,omitempty"`
	Clean      int64 `json:"clean,omitempty"`
	UpperBytes int64 `json:"upper_bytes,omitempty"`
	// Scanned is the watermark of Head (unix ns): the merged view had
	// Head's tree then, later changes have a newer ctime in the upper
	// directory (incremental commits).
	Scanned int64 `json:"scanned_ns,omitempty"`
	// Worktree and Branch are the labels of the last session.
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

// slotCache is a slot-scope cache path with its pruning settings.
type slotCache struct {
	Path     string `json:"path"`
	Prune    string `json:"prune,omitempty"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

// expired reports whether the slot data is too old under the slot's TTL,
// bounded by max.
func (m slotMeta) expired(now time.Time, max time.Duration) bool {
	ttl := max
	if d := time.Duration(m.TTL) * time.Second; d > 0 && d < ttl {
		ttl = d
	}
	return now.Sub(time.Unix(m.Created, 0)) >= ttl
}

func newSlots(dir string) (*slots, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &slots{dir: dir, leased: map[string]bool{}, maint: map[string]bool{}, pinned: map[string]bool{}}, nil
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
	// The slot's head is the commit point of a build slot: durable before
	// the layer it names is relied on.
	return hostfs.WriteFileSync(filepath.Join(slot, "meta.json"), b, 0o644)
}

// lease returns an exclusive slot directory for repoID and whether it holds
// previous build data. The data directory is slot/data.
func (s *slots) lease(repoID string, ttl time.Duration) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline := s.now().Add(maintWait)
	for {
		p, warm, wait, err := s.leaseLocked(repoID, ttl, !s.now().Before(deadline))
		if !wait {
			return p, warm, err
		}
		if s.freed == nil {
			s.freed = sync.NewCond(&s.mu)
		}
		t := time.AfterFunc(time.Second, func() { s.mu.Lock(); s.freed.Broadcast(); s.mu.Unlock() })
		s.freed.Wait()
		t.Stop()
	}
}

// leaseLocked is one attempt of lease; wait reports that the most
// recently used slot is held by maintenance (unless noWait), so the
// caller waits for its release.
func (s *slots) leaseLocked(repoID string, ttl time.Duration, noWait bool) (string, bool, bool, error) {
	repoDir := filepath.Join(s.dir, repoID)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return "", false, false, err
	}
	now := s.now()
	des, _ := os.ReadDir(repoDir)
	type cand struct {
		path string
		meta slotMeta
	}
	var free []cand
	var newestMaint int64
	used := map[int]bool{}
	for _, de := range des {
		n, err := strconv.Atoi(de.Name())
		if err != nil || !de.IsDir() {
			continue
		}
		p := filepath.Join(repoDir, de.Name())
		m, err := readMeta(p)
		if err != nil || now.Sub(time.Unix(m.Created, 0)) >= ttl {
			if !s.busyLocked(p) {
				s.removeDir(p) // expired: never reuse old build data
			} else {
				used[n] = true
			}
			continue
		}
		used[n] = true
		if !s.leased[p] {
			free = append(free, cand{p, m})
		} else if s.maint[p] && !noWait && m.LastUsed > newestMaint {
			newestMaint = m.LastUsed
		}
	}
	byRecent := func(c []cand) {
		sort.Slice(c, func(i, j int) bool { return c[i].meta.LastUsed > c[j].meta.LastUsed })
	}
	if len(free) > 0 {
		byRecent(free)
	}
	if newestMaint > 0 && (len(free) == 0 || free[0].meta.LastUsed <= newestMaint) {
		return "", false, true, nil // the latest slot is busy with maintenance
	}
	if len(free) > 0 {
		c := free[0]
		c.meta.LastUsed = now.Unix()
		c.meta.Leases++
		c.meta.TTL = int64(ttl / time.Second)
		if err := writeMeta(c.path, c.meta); err != nil {
			return "", false, false, err
		}
		s.leased[c.path] = true
		return c.path, true, false, nil
	}
	n := 0
	for used[n] {
		n++
	}
	p := filepath.Join(repoDir, strconv.Itoa(n))
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", false, false, err
	}
	meta := slotMeta{Created: now.Unix(), LastUsed: now.Unix(), TTL: int64(ttl / time.Second), Leases: 1}
	if err := os.MkdirAll(filepath.Join(p, "data"), 0o755); err != nil {
		return "", false, false, err
	}
	if err := writeMeta(p, meta); err != nil {
		return "", false, false, err
	}
	s.leased[p] = true
	return p, false, false, nil
}

func (s *slots) release(slot string) {
	s.releaseSized(slot, -1)
}

// releaseSized releases a slot and records its size (bytes < 0: keep).
func (s *slots) releaseSized(slot string, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releasedLocked(slot)
	if m, err := readMeta(slot); err == nil {
		m.LastUsed = s.now().Unix()
		if bytes >= 0 {
			m.Bytes = bytes
		}
		writeMeta(slot, m)
	}
}

// update changes the metadata of a slot.
func (s *slots) update(slot string, f func(*slotMeta)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := readMeta(slot)
	if err != nil {
		return err
	}
	f(&m)
	return writeMeta(slot, m)
}

// acquire marks an unleased slot as leased (for maintenance such as
// pruning or snapshots); it reports false if a session holds it.
func (s *slots) acquire(slot string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busyLocked(slot) {
		return false
	}
	s.leased[slot] = true
	s.maint[slot] = true
	return true
}

// releaseMaint ends maintenance of a slot acquired with acquire, recording
// its size; the slot's last use stays.
func (s *slots) releaseMaint(slot string, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releasedLocked(slot)
	if m, err := readMeta(slot); err == nil {
		m.Bytes = bytes
		writeMeta(slot, m)
	}
}

// size returns the recorded size of a slot, measuring it if unknown.
func (s *slots) size(slot string) int64 {
	m, err := readMeta(slot)
	if err == nil && m.Bytes > 0 {
		return m.Bytes
	}
	return treeSize(filepath.Join(slot, "data"))
}

// slotRef is a slot with its metadata.
type slotRef struct {
	Repo, Path string
	Meta       slotMeta
	Leased     bool
}

// list returns all slots with readable metadata.
func (s *slots) list() []slotRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []slotRef
	repos, _ := os.ReadDir(s.dir)
	for _, r := range repos {
		des, _ := os.ReadDir(filepath.Join(s.dir, r.Name()))
		for _, de := range des {
			p := filepath.Join(s.dir, r.Name(), de.Name())
			if m, err := readMeta(p); err == nil {
				out = append(out, slotRef{Repo: r.Name(), Path: p, Meta: m, Leased: s.busyLocked(p)})
			}
		}
	}
	return out
}

// latest returns the most recently used unexpired slot of a repo; ttl is
// the upper bound (machine TTL).
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
		if err != nil || m.expired(s.now(), ttl) {
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

// gc deletes expired, unleased slots; ttl is the upper bound (machine TTL).
func (s *slots) gc(ttl time.Duration) (removed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repos, _ := os.ReadDir(s.dir)
	for _, r := range repos {
		repoDir := filepath.Join(s.dir, r.Name())
		des, _ := os.ReadDir(repoDir)
		for _, de := range des {
			p := filepath.Join(repoDir, de.Name())
			if s.busyLocked(p) {
				continue
			}
			m, err := readMeta(p)
			if err != nil || m.expired(s.now(), ttl) {
				s.removeDir(p)
				removed++
			}
		}
	}
	return removed
}

// warmRepos lists repositories with at least one unexpired slot.
func (s *slots) warmRepos(ttl time.Duration) []string {
	var out []string
	repos, _ := os.ReadDir(s.dir)
	for _, r := range repos {
		if _, err := s.latest(r.Name(), ttl); err == nil {
			out = append(out, r.Name())
		}
	}
	return out
}

// adopt creates a new pinned slot for repo with the given metadata (a
// restore fills its data) and returns its path.
func (s *slots) adopt(repoID string, meta slotMeta) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repoDir := filepath.Join(s.dir, repoID)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return "", err
	}
	for n := 0; ; n++ {
		p := filepath.Join(repoDir, strconv.Itoa(n))
		if err := os.Mkdir(p, 0o755); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		if err := writeMeta(p, meta); err != nil {
			return "", err
		}
		s.pinned[p] = true
		return p, nil
	}
}

// unpin ends a restore's protection of a slot.
func (s *slots) unpin(slot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pinned, slot)
}

// isLeased reports whether a session holds the slot.
func (s *slots) isLeased(slot string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leased[slot]
}

// acquirePinned marks a pinned slot leased if no session holds it (the
// end of a restore).
func (s *slots) acquirePinned(slot string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leased[slot] {
		return false
	}
	s.leased[slot] = true
	s.maint[slot] = true
	return true
}

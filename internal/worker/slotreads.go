package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/sandbox"
)

// Per-session reads and the working set of build slots.
//
// Reads are tracked without atime: on the kernel overlay with fanotify
// (the slot's overlay super block, so sessions' bind mounts in their own
// mount namespaces are seen; paths are mapped back through the session's
// binds), on the userspace overlay by its open callback. The worker's own
// accesses (commit scans, output serving) are filtered by pid. Plain
// directory slots do not track reads.
//
// Every session's reads feed (1) the access model at the next commit
// (reads per pattern) and (2) the repository's working set: per path the
// last session that read it, how often and its size. The working set is
// persisted next to the access model and in the meta of every committed
// layer, so it travels with the head; a slot mounted fresh over a head
// reads ahead its working set (most recent sessions first, then the most
// frequently read) before the class prefetch. working_set_bytes is the
// size of the files read in the last wsWindow sessions.

const (
	workingSetFile = "workingset.json"
	wsWindow       = 3     // sessions counted in working_set_bytes
	wsKeep         = 16    // sessions an unread entry is kept
	wsMax          = 50000 // entries kept
	wsMeta         = 8000  // entries carried in a layer's meta
)

type wsEntry struct {
	Last  int64 `json:"l"`
	Count int64 `json:"n,omitempty"`
	Size  int64 `json:"s,omitempty"`
}

// workingSet is a repository's files read in recent sessions.
type workingSet struct {
	mu    sync.Mutex
	Seq   int64               `json:"seq"`
	Files map[string]*wsEntry `json:"files"`
}

func newWorkingSet() *workingSet { return &workingSet{Files: map[string]*wsEntry{}} }

// parseWorkingSet decodes a working set (empty on errors).
func parseWorkingSet(b []byte) *workingSet {
	w := newWorkingSet()
	if len(b) == 0 || json.Unmarshal(b, w) != nil || w.Files == nil {
		return newWorkingSet()
	}
	return w
}

func loadWorkingSet(file string) *workingSet {
	b, _ := os.ReadFile(file)
	return parseWorkingSet(b)
}

// touch records one session's reads (path -> size).
func (w *workingSet) touch(reads map[string]int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Seq++
	for p, sz := range reads {
		e := w.Files[p]
		if e == nil {
			e = &wsEntry{}
			w.Files[p] = e
		}
		e.Last, e.Size = w.Seq, sz
		e.Count++
	}
	for p, e := range w.Files {
		if e.Last <= w.Seq-wsKeep {
			delete(w.Files, p)
		}
	}
	if len(w.Files) > wsMax {
		for _, p := range w.orderLocked()[wsMax:] {
			delete(w.Files, p)
		}
	}
}

// orderLocked returns the paths by priority: most recently read, then
// most often, then by path.
func (w *workingSet) orderLocked() []string {
	ps := make([]string, 0, len(w.Files))
	for p := range w.Files {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool {
		a, b := w.Files[ps[i]], w.Files[ps[j]]
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return ps[i] < ps[j]
	})
	return ps
}

// bytes is the size of the files read in the last wsWindow sessions.
func (w *workingSet) bytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var n int64
	for _, e := range w.Files {
		if e.Last > w.Seq-wsWindow {
			n += e.Size
		}
	}
	return n
}

// plan returns the files of t in the working set, by priority.
func (w *workingSet) plan(t *anyfs.Tree) []*anyfs.Node {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*anyfs.Node
	for _, p := range w.orderLocked() {
		if n, ok := t.Get(p); ok && n.IsFile() {
			out = append(out, n)
		}
	}
	return out
}

// marshal encodes the working set, the max entries of highest priority
// (0: all).
func (w *workingSet) marshal(max int) []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := struct {
		Seq   int64               `json:"seq"`
		Files map[string]*wsEntry `json:"files"`
	}{w.Seq, w.Files}
	if max > 0 && len(w.Files) > max {
		out.Files = map[string]*wsEntry{}
		for _, p := range w.orderLocked()[:max] {
			out.Files[p] = w.Files[p]
		}
	}
	b, _ := json.Marshal(out)
	return b
}

func (w *workingSet) save(file string) error {
	b := w.marshal(0)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// readTracker collects the files read in a mounted slot.
type readTracker struct {
	data  string
	self  uint32
	watch *anyfs.AccessWatch

	mu      sync.Mutex
	binds   []sandbox.Bind  // the current session's binds below data
	session map[string]bool // read in the current session
	pending map[string]bool // read since the last commit
}

func newReadTracker(data string) *readTracker {
	return &readTracker{data: data, self: uint32(os.Getpid()), session: map[string]bool{}, pending: map[string]bool{}}
}

// watchKernel starts fanotify on the slot's overlay.
func (t *readTracker) watchKernel() error {
	w, err := anyfs.WatchAccess(t.data, t.notePath)
	if err != nil {
		return err
	}
	t.watch = w
	return nil
}

// onOpen is the userspace overlay's open callback. FUSE reports the
// opening thread: threads of this process are its own accesses.
func (t *readTracker) onOpen(rel string, write bool, pid uint32) {
	if write || pid == t.self || ownThread(pid) {
		return
	}
	t.note(rel)
}

// ownThreads caches thread ids of this process.
var ownThreads sync.Map

func ownThread(tid uint32) bool {
	if _, ok := ownThreads.Load(tid); ok {
		return true
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/self/task/%d", tid)); err == nil {
		ownThreads.Store(tid, true)
		return true
	}
	return false
}

func (t *readTracker) note(rel string) {
	if rel == "" || rel == "." {
		return
	}
	t.mu.Lock()
	t.session[rel] = true
	t.pending[rel] = true
	t.mu.Unlock()
}

// notePath maps a path seen by fanotify (the slot's mount or a session's
// bind of a part of it) to the slot path.
func (t *readTracker) notePath(p string, write bool) {
	if write {
		return
	}
	if rel, ok := under(p, t.data); ok {
		t.note(rel)
		return
	}
	t.mu.Lock()
	binds := t.binds
	t.mu.Unlock()
	for _, b := range binds {
		if rest, ok := under(p, b.Dst); ok {
			if base, ok := under(b.Src, t.data); ok {
				t.note(strings.TrimPrefix(base+"/"+rest, "/"))
				return
			}
		}
	}
}

// under returns p relative to dir if it is below it.
func under(p, dir string) (string, bool) {
	if p == dir {
		return "", true
	}
	if strings.HasPrefix(p, dir+"/") {
		return p[len(dir)+1:], true
	}
	return "", false
}

// beginSession starts a session with its binds.
func (t *readTracker) beginSession(binds []sandbox.Bind) {
	var bs []sandbox.Bind
	for _, b := range binds {
		if _, ok := under(b.Src, t.data); ok {
			bs = append(bs, b)
		}
	}
	sort.Slice(bs, func(i, j int) bool { return len(bs[i].Dst) > len(bs[j].Dst) }) // nested binds first
	t.mu.Lock()
	t.binds, t.session = bs, map[string]bool{}
	t.mu.Unlock()
}

// endSession returns the files read in the session.
func (t *readTracker) endSession() map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.session
	t.session = map[string]bool{}
	return s
}

// takePending returns the files read since the last commit.
func (t *readTracker) takePending() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.pending))
	for p := range t.pending {
		out = append(out, p)
	}
	t.pending = map[string]bool{}
	sort.Strings(out)
	return out
}

// requeue puts reads taken by a plan that was not committed back.
func (t *readTracker) requeue(paths []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range paths {
		t.pending[p] = true
	}
}

func (t *readTracker) close() {
	if t.watch != nil {
		t.watch.Close()
	}
}

// tracker returns a mounted slot's read tracker (nil: reads unknown).
func (c *cacheFS) tracker(slot string) *readTracker {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sm := c.mounts[slot]; sm != nil {
		return sm.reads
	}
	return nil
}

// sessionStarted tells a slot's tracker the session's binds.
func (c *cacheFS) sessionStarted(slot string, binds []sandbox.Bind) {
	if t := c.tracker(slot); t != nil {
		t.beginSession(binds)
	}
}

// sessionEnded folds a session's reads into the repository's working set
// and returns them with the bytes read (ok false: reads not tracked).
func (c *cacheFS) sessionEnded(slot, repo string) (map[string]bool, int64, bool) {
	t := c.tracker(slot)
	if t == nil {
		return nil, 0, false
	}
	reads := t.endSession()
	sizes := make(map[string]int64, len(reads))
	var total int64
	for p := range reads {
		fi, err := os.Lstat(filepath.Join(t.data, filepath.FromSlash(p)))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		sizes[p] = fi.Size()
		total += fi.Size()
	}
	if r, err := c.repo(repo); err == nil {
		r.ws.touch(sizes)
		if err := r.ws.save(filepath.Join(c.dir, "repos", repo, workingSetFile)); err != nil {
			c.log.Warn("save working set", "repo", repo, "err", err)
		}
	}
	return reads, total, true
}

// workingSetBytes is the tracked working set of all repositories (0:
// none tracked).
func (c *cacheFS) workingSetBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for _, r := range c.repos {
		n += r.ws.bytes()
	}
	return n
}

// readahead faults in a freshly mounted slot's working set (from the head
// layer's meta, else the repository's), then the class prefetch, within
// the prefetch budget; unmount cancels it.
func (c *cacheFS) readahead(sm *slotMount, meta []byte) {
	budget := int64(prefetchClassMax)
	if c.opt.Capacity > 0 {
		budget = min(budget, c.opt.Capacity/4)
	}
	ws := sm.repo.ws
	if len(meta) > 0 {
		if w := parseWorkingSet(meta); len(w.Files) > 0 {
			ws = w
		}
	}
	tree := sm.src.Tree()
	var nodes []*anyfs.Node
	var sum int64
	seen := map[string]bool{}
	for _, n := range ws.plan(tree) {
		if sum+n.Size > budget {
			continue
		}
		nodes = append(nodes, n)
		seen[n.Path] = true
		sum += n.Size
	}
	wsFiles := len(nodes)
	for _, n := range prefetchPlan(sm.repo.access, tree, prefetchSmall, budget-sum) {
		if !seen[n.Path] {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sm.cancel = cancel
	go func() {
		defer cancel()
		n := c.files.Prefetch(ctx, sm.repo.tier, nodes, 8)
		c.log.Debug("readahead", "repo", sm.repo.id, "working_set_files", wsFiles, "files", n)
	}()
}

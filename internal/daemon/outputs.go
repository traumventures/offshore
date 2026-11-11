package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/offshore/internal/cas"
	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/outputfs"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/transport"
)

// OutputsCASDir is the local content cache of remote outputs. The proxy
// stores eager outputs of mounted build directories here instead of
// writing them into the mount.
func OutputsCASDir() string { return filepath.Join(StateDir(), "outputs", "cas") }

// OutputsUpperDir is the upper (local, writable) layer of the output
// overlay mounted at mountpoint. It lives in the client state, never in
// the repository, survives daemon restarts and is removed by
// "offshore outputs unmount".
func OutputsUpperDir(mountpoint string) string {
	h := sha256.Sum256([]byte(filepath.Clean(mountpoint)))
	return filepath.Join(StateDir(), "outputs", "upper", hex.EncodeToString(h[:8]))
}

func outputsStatePath() string { return filepath.Join(StateDir(), "outputs", "state.json") }

// outputsState is what the daemon remembers per repository across restarts:
// where the latest remote build ran and whether its outputs are mounted.
type outputsState struct {
	RepoID      string   `json:"repo_id"`
	Machine     string   `json:"machine,omitempty"`
	Addr        string   `json:"addr,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Token       string   `json:"token,omitempty"`
	Build       []string `json:"build,omitempty"`
	Mounted     bool     `json:"mounted,omitempty"`
}

// EnsureOutputs makes the build directories (slot-scope cache paths) of the
// repository at root appear at their local paths as the output overlay
// (internal/outputfs): it starts the daemon if needed, mounts the overlay
// unless mounted (whatever [outputs] mode says) and returns its status.
//
// The overlay is up to date with the latest remote build as soon as that
// build's run returned: the proxy's release call starts the new generation
// before it exits. Local cargo then finds unchanged units fresh (see
// README "Output overlay"), e.g. to run tests locally from remote-built
// binaries without fetching them.
func EnsureOutputs(ctx context.Context, root string) (OutputsStatus, error) {
	var st OutputsStatus
	if err := EnsureDaemon(ctx); err != nil {
		return st, err
	}
	raw, err := Call(ctx, Request{Op: "outputs-ensure", Root: root}, nil)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

// outputsRepo is the output view of one repository.
type outputsRepo struct {
	root   string
	state  outputsState
	tree   *outputfs.Tree
	mounts []*outputfs.FS
}

// leaseRef remembers which repository a lease was prepared for.
type leaseRef struct {
	root  string
	state outputsState
}

// OutputsStatus describes one repository's output view.
type OutputsStatus struct {
	Root        string   `json:"root"`
	Machine     string   `json:"machine,omitempty"`
	Mountpoints []string `json:"mountpoints,omitempty"`
	Uppers      []string `json:"uppers,omitempty"` // upper layers of the mountpoints
	Generation  uint64   `json:"generation"`
	CachedDirs  int      `json:"cached_dirs"`
	KnownFiles  int      `json:"known_files"`
}

type outputsMgr struct {
	d     *Daemon
	store *cas.Store

	mu     sync.Mutex
	repos  map[string]*outputsRepo
	leases map[string]leaseRef
}

func newOutputsMgr(d *Daemon) (*outputsMgr, error) {
	store, err := cas.New(OutputsCASDir())
	if err != nil {
		return nil, err
	}
	m := &outputsMgr{d: d, store: store, repos: map[string]*outputsRepo{}, leases: map[string]leaseRef{}}
	var saved map[string]outputsState
	if b, err := os.ReadFile(outputsStatePath()); err == nil {
		json.Unmarshal(b, &saved)
	}
	for root, st := range saved {
		r := m.repoLocked(root)
		r.state = st
		if st.Machine != "" {
			r.tree.Update(m.source(st))
		}
	}
	return m, nil
}

func (m *outputsMgr) repoLocked(root string) *outputsRepo {
	r, ok := m.repos[root]
	if !ok {
		r = &outputsRepo{root: root, tree: outputfs.NewTree(m.store, m.d.log)}
		m.repos[root] = r
	}
	return r
}

func (m *outputsMgr) saveLocked() {
	all := map[string]outputsState{}
	for root, r := range m.repos {
		all[root] = r.state
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	p := outputsStatePath()
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

func (m *outputsMgr) source(st outputsState) outputfs.Source {
	return &machineSource{st: st}
}

// remountSaved restores mounts after a daemon restart.
func (m *outputsMgr) remountSaved() {
	m.mu.Lock()
	var roots []string
	for root, r := range m.repos {
		if r.state.Mounted {
			roots = append(roots, root)
		}
	}
	m.mu.Unlock()
	for _, root := range roots {
		if _, err := m.mount(root, nil); err != nil {
			m.d.log.Warn("outputs: remount failed", "root", root, "err", err)
		}
	}
}

// prepared records a lease so that its release can update the outputs.
func (m *outputsMgr) prepared(root, leaseID string, st outputsState) {
	m.mu.Lock()
	m.leases[leaseID] = leaseRef{root: root, state: st}
	// Machines may restart with a new address: refresh the connection
	// details of every repository whose latest build ran there.
	type upd struct {
		tree *outputfs.Tree
		src  outputfs.Source
	}
	var updates []upd
	for _, r := range m.repos {
		if r.state.Machine == st.Machine && (r.state.Addr != st.Addr || r.state.Token != st.Token) {
			r.state.Addr, r.state.Fingerprint, r.state.Token = st.Addr, st.Fingerprint, st.Token
			updates = append(updates, upd{r.tree, m.source(r.state)})
		}
	}
	if len(updates) > 0 {
		m.saveLocked()
	}
	m.mu.Unlock()
	for _, u := range updates {
		u.tree.Update(u.src)
	}
}

// released starts a new generation when a remote run happened.
func (m *outputsMgr) released(leaseID string, ran bool) {
	m.mu.Lock()
	ref, ok := m.leases[leaseID]
	delete(m.leases, leaseID)
	if !ok || !ran {
		m.mu.Unlock()
		return
	}
	r := m.repoLocked(ref.root)
	mounted, build := r.state.Mounted, r.state.Build
	r.state = ref.state
	r.state.Mounted, r.state.Build = mounted, build
	m.saveLocked()
	tree, st := r.tree, r.state
	m.mu.Unlock()
	tree.Update(m.source(st)) // drops kernel caches outside the lock
}

// hint records eager outputs that the proxy stored in the content cache.
func (m *outputsMgr) hint(root string, files []manifest.Entry) {
	m.mu.Lock()
	r := m.repoLocked(root)
	m.mu.Unlock()
	r.tree.Hint(files)
}

// mount mounts every build directory of the repository at root.
func (m *outputsMgr) mount(root string, repo *config.Repo) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repoLocked(root)
	build := r.state.Build
	if repo != nil {
		build = repo.SlotPaths()
	}
	if len(build) == 0 {
		return nil, errors.New("no build directories (slot-scope cache paths) configured")
	}
	if len(r.mounts) == 0 {
		for _, b := range build {
			mp := filepath.Join(root, filepath.FromSlash(b))
			fs, err := outputfs.Mount(r.tree, root, b, OutputsUpperDir(mp), m.d.log)
			if err != nil {
				for _, f := range r.mounts {
					f.Unmount()
				}
				r.mounts = nil
				return nil, err
			}
			r.mounts = append(r.mounts, fs)
			m.d.log.Info("outputs mounted", "path", fs.Mountpoint(), "upper", fs.Upper(), "machine", r.state.Machine)
		}
	}
	r.state.Build, r.state.Mounted = build, true
	m.saveLocked()
	var mps []string
	for _, f := range r.mounts {
		mps = append(mps, f.Mountpoint())
	}
	return mps, nil
}

// unmount removes the mounts of root. keep leaves them marked for remount
// with their upper layers (daemon shutdown); otherwise the upper layers
// (local writes into the mount) are deleted.
func (m *outputsMgr) unmount(root string, keep bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[root]
	if !ok {
		return nil
	}
	var errs []error
	for _, f := range r.mounts {
		if err := f.Unmount(); err != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w", f.Mountpoint(), err))
			continue
		}
		m.d.log.Info("outputs unmounted", "path", f.Mountpoint())
	}
	r.mounts = nil
	if !keep {
		// Also upper layers of mounts lost by a crashed daemon.
		for _, b := range r.state.Build {
			mp := filepath.Join(root, filepath.FromSlash(b))
			if outputfs.IsMounted(mp) {
				continue
			}
			if err := os.RemoveAll(OutputsUpperDir(mp)); err != nil {
				errs = append(errs, err)
			}
		}
		r.state.Mounted = false
		m.saveLocked()
	}
	return errors.Join(errs...)
}

func (m *outputsMgr) unmountAll() {
	m.mu.Lock()
	var roots []string
	for root := range m.repos {
		roots = append(roots, root)
	}
	m.mu.Unlock()
	for _, root := range roots {
		m.unmount(root, true)
	}
	m.mu.Lock()
	for _, r := range m.repos {
		r.tree.Close()
	}
	m.mu.Unlock()
}

func statusLocked(root string, r *outputsRepo) OutputsStatus {
	s := OutputsStatus{Root: root, Machine: r.state.Machine, Generation: r.tree.Generation()}
	s.CachedDirs, s.KnownFiles = r.tree.Stats()
	for _, f := range r.mounts {
		s.Mountpoints = append(s.Mountpoints, f.Mountpoint())
		s.Uppers = append(s.Uppers, f.Upper())
	}
	return s
}

func (m *outputsMgr) statusOf(root string) OutputsStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return statusLocked(root, m.repoLocked(root))
}

func (m *outputsMgr) status() []OutputsStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OutputsStatus
	for root, r := range m.repos {
		out = append(out, statusLocked(root, r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out
}

// machineSource reads outputs from one machine over its own QUIC
// connection (independent of the daemon's sync connection, so a stale
// address never disturbs projection).
type machineSource struct {
	st outputsState

	mu   sync.Mutex
	conn *quic.Conn
}

func (s *machineSource) Name() string { return s.st.Machine }

func (s *machineSource) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.CloseWithError(0, "")
		s.conn = nil
	}
}

func (s *machineSource) open(ctx context.Context) (*quic.Stream, *proto.Conn, error) {
	s.mu.Lock()
	conn := s.conn
	if conn == nil || conn.Context().Err() != nil {
		dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		c, err := transport.Dial(dctx, s.st.Addr, s.st.Fingerprint)
		cancel()
		if err != nil {
			s.mu.Unlock()
			return nil, nil, fmt.Errorf("connect to machine %s: %w", s.st.Machine, err)
		}
		s.conn, conn = c, c
	}
	s.mu.Unlock()
	return transport.OpenStream(ctx, conn, s.st.Token, proto.KindOutputs)
}

// answerTimeout bounds the wait for the machine's first answer. A stopped
// machine keeps an open QUIC connection alive until its idle timeout, so
// without it reads would hang for a minute.
const answerTimeout = 15 * time.Second

// request opens a stream, sends one request and arms a timer that aborts
// the stream (and drops the connection) unless disarmed in time.
func (s *machineSource) request(ctx context.Context, t byte, req any) (*quic.Stream, *proto.Conn, func() bool, error) {
	st, pc, err := s.open(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	timer := time.AfterFunc(answerTimeout, func() {
		st.CancelRead(1)
		st.CancelWrite(1)
		s.Close()
	})
	stop := context.AfterFunc(ctx, func() { st.CancelRead(0) })
	disarm := func() bool { stop(); return timer.Stop() }
	if err := pc.Send(t, req); err != nil {
		disarm()
		st.Close()
		return nil, nil, nil, err
	}
	return st, pc, disarm, nil
}

func (s *machineSource) List(ctx context.Context, dir string) ([]proto.OutEntry, error) {
	st, pc, disarm, err := s.request(ctx, proto.TOutList, proto.OutList{RepoID: s.st.RepoID, Dir: dir})
	if err != nil {
		return nil, err
	}
	defer st.Close()
	var out proto.OutDir
	err = pc.Expect(proto.TOutDir, &out)
	if !disarm() && err != nil {
		return nil, fmt.Errorf("machine %s did not answer: %w", s.st.Machine, err)
	}
	if err != nil {
		return nil, err
	}
	return out.Entries, nil
}

func (s *machineSource) Read(ctx context.Context, rel string, size, mtimeNs int64, w io.Writer) error {
	st, pc, disarm, err := s.request(ctx, proto.TOutRead, proto.OutRead{RepoID: s.st.RepoID, Path: rel, Size: size, MtimeNs: mtimeNs})
	if err != nil {
		return err
	}
	defer st.Close()
	var hdr proto.BlobHeader
	err = pc.Expect(proto.TBlobHeader, &hdr)
	if !disarm() && err != nil {
		return fmt.Errorf("machine %s did not answer: %w", s.st.Machine, err)
	}
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { st.CancelRead(0) })
	defer stop()
	return pc.ReadData(w)
}

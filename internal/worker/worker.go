// Package server implements the worker side: projections of client roots,
// session execution, build slots and machine lifecycle.
package worker

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/traumventures/offshore/internal/clock"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/auth"
	"github.com/traumventures/offshore/internal/cas"
	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/hostfs"
	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/machine"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/tracing"
	"github.com/traumventures/offshore/internal/transport"
)

// Config is read from OFFSHORE_* environment variables set by the
// provider at machine creation.
type Config struct {
	MachineID string
	Identity  transport.Identity // derived from the TLS seed
	Verifier  auth.Verifier      // checks control plane tokens
	Listen    string
	CPU       float64
	Memory    int64
	EnvHash   string
	ExpiresAt time.Time
	Provision []string
	CacheDir  string
	DepsDir   string
	BuildTTL  time.Duration // upper bound for build cache age
	// DiskBudget is how much of the cache disk may be used before slots
	// are pruned: [scheduler] disk_budget of the machine's size, passed by
	// the orchestrator (zero: config.DefaultDiskBudget).
	DiskBudget config.DiskBudget
	// HostStatus is a file the server publishes machine.HostStatus to for
	// the supervisor (OFFSHORE_HOST_STATUS; empty: none).
	HostStatus string
	// SccacheDir is the local tier of the result caches
	// (OFFSHORE_SCCACHE_DIR, default <deps dir>/sccache).
	SccacheDir string
	// Clock is the time of idle, expiry and build slot decisions
	// ($OFFSHORE_TEST_CLOCK; nil: real time).
	Clock clock.Clock
	// DirtyRatio and DirtyExpire are the build slot writeback thresholds
	// ([writeback] of the pool: OFFSHORE_WRITEBACK_DIRTY_RATIO, a fraction
	// of the disk budget a session may grow a slot by before it is
	// committed at once, default 0.1; OFFSHORE_WRITEBACK_DIRTY_EXPIRE, how
	// long a slot stays idle before its commit, default 30s).
	DirtyRatio  float64
	DirtyExpire time.Duration
	// SourceWait bounds how long a source file fault waits for the client
	// to reconnect after its connection was lost (zero: 60s).
	SourceWait time.Duration
	// SourceCache bounds the fetched source contents: a share of the disk
	// budget or a size ([scheduler] source_cache: OFFSHORE_SOURCE_CACHE;
	// zero: config.DefaultSourceCache).
	SourceCache config.DiskBudget
	// SlotMode forces the build slot mount mode (tests; nil: probe).
	SlotMode *fsMode
	// CacheFSPolicy is what the cache medium must be (machine.CacheFSEnv:
	// hostfs.RequireXFS on cloud machines; empty: anything, warned).
	CacheFSPolicy hostfs.Policy
	// FSProbes inspect the cache medium (tests; zero: the real calls).
	FSProbes hostfs.Probes
}

func (c Config) now() time.Time {
	if c.Clock == nil {
		return time.Now()
	}
	return c.Clock.Now()
}

// ConfigFromEnv reads the server configuration.
func ConfigFromEnv() (Config, error) {
	c := Config{
		MachineID:  os.Getenv("OFFSHORE_MACHINE_ID"),
		Listen:     envOr("OFFSHORE_LISTEN", ":7443"),
		EnvHash:    os.Getenv("OFFSHORE_ENV_HASH"),
		CacheDir:   envOr("OFFSHORE_CACHE_DIR", "/cache"),
		DepsDir:    envOr("OFFSHORE_DEPS_DIR", "/deps"),
		BuildTTL:   24 * time.Hour,
		HostStatus: os.Getenv(machine.HostStatusEnv),
	}
	c.SccacheDir = envOr("OFFSHORE_SCCACHE_DIR", filepath.Join(c.DepsDir, "sccache"))
	clk, err := clock.FromEnv()
	if err != nil {
		return c, err
	}
	c.Clock = clk
	if c.MachineID == "" || os.Getenv(auth.EnvTLSSeed) == "" || os.Getenv(auth.EnvSignerPub) == "" || os.Getenv("OFFSHORE_EXPIRES_AT") == "" {
		return c, errors.New("server: OFFSHORE_MACHINE_ID, _TLS_SEED, _SIGNER_PUB and _EXPIRES_AT are required")
	}
	c.CPU, _ = strconv.ParseFloat(os.Getenv("OFFSHORE_CPU"), 64)
	c.Memory, _ = strconv.ParseInt(os.Getenv("OFFSHORE_MEMORY"), 10, 64)
	n, err := strconv.ParseInt(os.Getenv("OFFSHORE_EXPIRES_AT"), 10, 64)
	if err != nil {
		return c, fmt.Errorf("server: OFFSHORE_EXPIRES_AT: %w", err)
	}
	c.ExpiresAt = time.Unix(n, 0)
	seed, err := auth.DecodeKey(os.Getenv(auth.EnvTLSSeed), ed25519.SeedSize)
	if err != nil {
		return c, fmt.Errorf("server: %s: %w", auth.EnvTLSSeed, err)
	}
	os.Unsetenv(auth.EnvTLSSeed)
	id, err := auth.MachineIdentity(seed, c.MachineID, c.ExpiresAt)
	if err != nil {
		return c, fmt.Errorf("server: %w", err)
	}
	if want := os.Getenv(auth.EnvFingerprint); want != "" && want != id.Fingerprint {
		return c, fmt.Errorf("server: derived certificate fingerprint %s does not match %s", id.Fingerprint, want)
	}
	c.Identity = *id
	pub, err := auth.DecodeKey(os.Getenv(auth.EnvSignerPub), ed25519.PublicKeySize)
	if err != nil {
		return c, fmt.Errorf("server: %s: %w", auth.EnvSignerPub, err)
	}
	c.Verifier = auth.Verifier{Pub: ed25519.PublicKey(pub), Machine: c.MachineID}
	if s := os.Getenv("OFFSHORE_BUILD_TTL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return c, fmt.Errorf("server: OFFSHORE_BUILD_TTL: %w", err)
		}
		c.BuildTTL = d
	}
	if s := os.Getenv("OFFSHORE_DISK_BUDGET"); s != "" {
		b, err := config.ParseDiskBudget(s)
		if err != nil {
			return c, fmt.Errorf("server: disk budget: %w", err)
		}
		c.DiskBudget = b
	}
	if s := os.Getenv("OFFSHORE_WRITEBACK_DIRTY_RATIO"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 || f > 1 {
			return c, fmt.Errorf("server: OFFSHORE_WRITEBACK_DIRTY_RATIO %q: want 0 < ratio <= 1", s)
		}
		c.DirtyRatio = f
	}
	if s := os.Getenv("OFFSHORE_WRITEBACK_DIRTY_EXPIRE"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("server: OFFSHORE_WRITEBACK_DIRTY_EXPIRE %q: want a positive duration", s)
		}
		c.DirtyExpire = d
	}
	if s := os.Getenv("OFFSHORE_SOURCE_CACHE"); s != "" {
		b, err := config.ParseSourceCache(s)
		if err != nil {
			return c, fmt.Errorf("server: OFFSHORE_SOURCE_CACHE: %w", err)
		}
		c.SourceCache = b
	}
	if s := os.Getenv("OFFSHORE_PROVISION"); s != "" {
		if err := json.Unmarshal([]byte(s), &c.Provision); err != nil {
			return c, fmt.Errorf("server: OFFSHORE_PROVISION: %w", err)
		}
	}
	if c.CacheFSPolicy, err = hostfs.ParsePolicy(os.Getenv(machine.CacheFSEnv)); err != nil {
		return c, fmt.Errorf("server: %s: %w", machine.CacheFSEnv, err)
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Worker is a running worker.
type Worker struct {
	cfg   Config
	log   *slog.Logger
	store *cas.Store
	slots *slots
	cg    *cgroups
	self  string
	env   []string // base environment for sessions

	ccEndpoint string       // compiler cache WebDAV endpoint for sessions ("" = off)
	rcStore    *tieredStore // its store
	grants     grantCache   // latest store grants per repository
	cfs        *cacheFS     // build slot file system (anyfs)
	// cacheMedium is the cache directory's file system checked against
	// the policy at startup.
	cacheMedium *proto.CacheFSStatus
	maintMu     sync.Mutex // one slot maintenance pass at a time
	headMu      sync.Mutex
	headTimers  map[string]*time.Timer // repo -> armed head flush deadline
	// tiers counts the cache file system's transfers per medium
	// (anyfs.RecordTransfers), reported in the status.
	tiers medium.Recorder

	mu       sync.Mutex
	mounts   map[string]*anyfs.WritableFS // source projections by root
	srcFiles *anyfs.Files                 // their faulted-in file contents
	sources  map[string]*quic.Conn        // root -> client connection serving blobs
	// sourceWake is closed (and replaced) whenever sources changes:
	// faults waiting for a client to reconnect wake up.
	sourceWake chan struct{}
	sessions   map[string]*session
	// cpuLast is the machine CPU time at cpuAt; Measured.CPU is the rate
	// since then (cores busy), refreshed at most every cpuInterval.
	cpuLast, cpuRate float64
	cpuAt            time.Time
	idleSince        time.Time // last status request (an orchestrator probe)
	lastProbe        time.Time // last status request (an orchestrator probe)
	ready            bool
	draining         bool
	// boot is the machine's boot phase record: the supervisor's (read
	// from the status directory until it is complete) and the worker's.
	boot       []machine.BootPhase
	bootHost   bool // the supervisor's record is complete
	bootWorker []machine.BootPhase
	// drainReason describes a drain the server started on its own
	// (expiry).
	drainReason string
	// diskFull: the cache disk is above the emergency threshold; no
	// sessions are admitted (reported as draining) until it recovers.
	diskFull bool

	metrics *serverMetrics

	// machineCacheBytes is the size of the machine caches, measured by
	// the periodic gc.
	machineCacheBytes atomic.Int64
	// storage is the last storage measurement (storage.go).
	storage        storageMeasure
	pruneMu        sync.Mutex // one prune at a time
	budgetMu       sync.Mutex // one budget check at a time
	machineBudgets machineCacheBudgets
	// sourceFetch replaces fetchFile for tests (nil: fetchFile).
	sourceFetch func(ctx context.Context, root string, n *anyfs.Node, w io.Writer) error
}

// New prepares a server.
func New(cfg Config, log *slog.Logger) (*Worker, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cacheMed := checkCacheMedium(cfg.CacheDir, cfg.CacheFSPolicy, cfg.FSProbes, log)
	store, err := cas.New(filepath.Join(cfg.CacheDir, "cas"))
	if err != nil {
		return nil, err
	}
	sl, err := newSlots(filepath.Join(cfg.CacheDir, "build"))
	if err != nil {
		return nil, err
	}
	sl.clock = cfg.Clock
	srcFiles, err := anyfs.OpenFiles(filepath.Join(cfg.CacheDir, "sources"), nil)
	if err != nil {
		return nil, err
	}
	// Faulted-in source contents are a cache of the laptop's files: any
	// of them can be fetched again, so they are reclaimed least recently
	// used beyond [scheduler] source_cache (default an eighth of the disk
	// budget; open files never; local modifications live in the
	// projections' upper directories).
	srcFiles.MarkAllBacked()
	if total, _, ok := fsUsage(cfg.CacheDir); ok {
		srcFiles.SetLimit(sourceCacheBytes(cfg, total))
	}
	var env []string
	for _, kv := range os.Environ() {
		// Neither pool secrets nor the server's tracing setup (whose
		// headers may hold credentials) reach sessions.
		if !strings.HasPrefix(kv, "OFFSHORE_") && !strings.HasPrefix(kv, "OTEL_") {
			env = append(env, kv)
		}
	}
	s := &Worker{
		cfg: cfg, log: log, store: store, slots: sl, self: self, env: env,
		mounts: map[string]*anyfs.WritableFS{}, srcFiles: srcFiles, sources: map[string]*quic.Conn{},
		sourceWake: make(chan struct{}),
		sessions:   map[string]*session{}, idleSince: cfg.now(), lastProbe: cfg.now(), cacheMedium: cacheMed,
		bootWorker: []machine.BootPhase{{Name: machine.PhaseWorker, At: processStart().UnixMilli()}},
	}
	if s.cfs, err = newWorkerCacheFS(cfg, log, anyfs.RecordTransfers(&s.tiers, nil)); err != nil {
		return nil, err
	}
	sl.remove = s.cfs.removeSlot
	s.metrics = newServerMetrics(s)
	return s, nil
}

// Run provisions the machine and serves until ctx ends or the machine expires.
func (s *Worker) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.cg = setupCgroups(s.log)
	s.startCompilerCache(ctx)
	for _, r := range s.slots.list() {
		s.cfs.unmount(r.Path) // mounts of a previous process are dead
	}
	s.recoverSnapshots(ctx) // sessions captured but not committed before a crash
	// Temporary directories of sessions of a previous process.
	os.RemoveAll(filepath.Join(s.cfg.CacheDir, "tmp"))
	if n := s.slots.gc(s.cfg.BuildTTL); n > 0 {
		s.log.Info("removed expired build slots", "count", n)
	}
	go s.measureMachineCaches()
	ln, err := transport.Listen(s.cfg.Listen, &s.cfg.Identity)
	if err != nil {
		return err
	}
	defer ln.Close()
	s.log.Info("listening", "addr", s.cfg.Listen, "machine", s.cfg.MachineID)
	s.bootPhase(machine.PhaseListening)
	go func() {
		if err := s.provision(ctx); err != nil {
			s.log.Error("machine provisioning failed", "err", err)
			cancel()
			return
		}
		s.bootPhase(machine.PhaseReady)
		s.mu.Lock()
		s.ready = true
		s.mu.Unlock()
		s.log.Info("machine ready")
	}()
	go s.lifecycle(ctx, cancel)
	if s.cfg.HostStatus != "" {
		go s.publishHostStatus(ctx)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			break
		}
		go s.serveConn(ctx, conn)
	}
	s.shutdown()
	if ctx.Err() != nil && s.provisionFailed() {
		return errors.New("provisioning failed")
	}
	if parent.Err() != nil {
		// SIGTERM (machine stop): commit and write back what changed
		// since the last flush. The container's stop timeout bounds this
		// (machine.StopTimeout); the orchestrator normally saved before
		// stopping, so this is incremental or nothing.
		s.writebackChanged(time.Duration(machine.StopTimeout-10)*time.Second, "stop")
	}
	s.stopHeadFlushes()
	s.cfs.unmountAll()
	return nil
}

func (s *Worker) provisionFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ready
}

func (s *Worker) provision(ctx context.Context) error {
	marker := filepath.Join(s.cfg.CacheDir, "provisioned")
	if _, err := os.Stat(marker); err == nil {
		return nil // a restarted machine keeps its provisioning
	}
	for i, step := range s.cfg.Provision {
		s.log.Info("provision", "step", i+1, "run", step)
		cmd := exec.CommandContext(ctx, "/bin/sh", "-ec", step)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Env = s.env
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("provision step %d (%q): %w", i+1, step, err)
		}
	}
	return os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// lifecycle enforces the machine lifetime and collects expired build slots.
func (s *Worker) lifecycle(ctx context.Context, cancel func()) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastGC := time.Now()
	go s.measureMachineCaches() // the storage breakdown before the first GC round
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		go s.maintainSlots(ctx)
		// Disk and cache budgets every tick (statfs and counters are
		// cheap): sessions can write tens of GB between GC rounds.
		go s.checkBudgets()
		if time.Since(lastGC) > 5*time.Minute {
			s.slots.gc(s.cfg.BuildTTL)
			s.gcDigests(time.Now())
			go s.measureMachineCaches()
			go func() {
				s.pruneMu.Lock()
				defer s.pruneMu.Unlock()
				s.pruneMachineCaches()
				s.enforceDiskBudget()
			}()
			lastGC = time.Now()
		}
		if s.cfg.ExpiresAt.IsZero() || s.cfg.now().Before(s.cfg.ExpiresAt) {
			continue
		}
		s.mu.Lock()
		if !s.draining {
			s.log.Info("machine lifetime reached, draining")
			go s.writebackChanged(8*time.Minute, "drain")
		}
		s.draining = true
		if s.drainReason == "" {
			s.drainReason = "expired"
		}
		n := len(s.sessions)
		s.mu.Unlock()
		if n == 0 || s.cfg.now().After(s.cfg.ExpiresAt.Add(10*time.Minute)) {
			s.log.Info("machine expired, shutting down")
			cancel()
			return
		}
	}
}

func (s *Worker) shutdown() {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, se := range s.sessions {
		sessions = append(sessions, se)
	}
	mounts := s.mounts
	s.mu.Unlock()
	for _, se := range sessions {
		se.killFor(killerWorkerShutdown)
	}
	for _, p := range mounts {
		p.Unmount()
	}
}

func (s *Worker) serveConn(ctx context.Context, conn *quic.Conn) {
	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.serveStream(ctx, conn, st)
	}
}

func (s *Worker) serveStream(ctx context.Context, conn *quic.Conn, st *quic.Stream) {
	defer st.Close()
	pc := proto.NewConn(st)
	var hello proto.Hello
	if err := pc.Expect(proto.THello, &hello); err != nil {
		st.CancelRead(1)
		return
	}
	claims, err := s.cfg.Verifier.Verify(hello.Token)
	if err == nil {
		err = authorize(hello.Kind, claims)
	}
	if err != nil {
		s.log.Warn("rejected stream", "kind", hello.Kind, "err", err)
		pc.SendError("auth", err)
		st.CancelRead(1)
		return
	}
	ctx = tracing.Extract(ctx, hello.TraceParent)
	switch hello.Kind {
	case proto.KindSync:
		err = s.serveSync(ctx, conn, pc)
	case proto.KindExec:
		err = s.serveExec(ctx, pc, st, claims)
	case proto.KindStatus:
		s.mu.Lock()
		s.lastProbe = s.cfg.now()
		s.mu.Unlock()
		err = pc.Send(proto.TStatus, s.Status())
	case proto.KindFetch:
		err = s.serveFetch(ctx, pc)
	case proto.KindOutputs:
		err = s.serveOutputs(ctx, pc)
	case proto.KindForward:
		err = s.serveForward(ctx, pc, st, claims)
	case proto.KindSlot:
		err = s.serveSlot(ctx, pc)
	}
	if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		s.log.Debug("stream ended", "kind", hello.Kind, "err", err)
	}
}

// HostStatus is the status published for the supervisor.
func (s *Worker) HostStatus() machine.HostStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := machine.HostStatus{
		UpdatedAt: s.cfg.now().Unix(), LastProbe: s.lastProbe.Unix(),
		Sessions: len(s.sessions), Ready: s.ready,
	}
	if len(s.sessions) == 0 {
		st.IdleSince = s.idleSince.Unix()
	}
	return st
}

// publishHostStatus refreshes the host status file every few seconds.
func (s *Worker) publishHostStatus(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	failing := false
	for {
		err := machine.WriteHostStatus(s.cfg.HostStatus, s.HostStatus())
		if err != nil && !failing {
			s.log.Warn("publishing host status", "path", s.cfg.HostStatus, "err", err)
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// authorize checks that a token's scope allows a stream kind. Lease
// binding of exec streams is checked once the request is known.
func authorize(kind string, c auth.Claims) error {
	var need auth.Scope
	switch kind {
	case proto.KindSync, proto.KindExec, proto.KindFetch, proto.KindForward, proto.KindOutputs:
		need = auth.ScopeSession
	case proto.KindStatus:
		need = auth.ScopeProbe
	case proto.KindSlot:
		need = auth.ScopeAdmin
	default:
		return fmt.Errorf("unknown stream kind %q", kind)
	}
	if !c.Allows(need) {
		return fmt.Errorf("%w: scope %q does not allow %s streams", auth.ErrInvalidToken, c.Scope, kind)
	}
	return nil
}

// Status reports machine state including the server's metrics.
func (s *Worker) Status() proto.Status {
	st := s.statusOnly()
	st.Metrics = s.metrics.reg.Gather()
	return st
}

// cpuInterval is the shortest window the busy-core rate is measured over.
const cpuInterval = 5 * time.Second

// cpuBusyLocked returns how many cores the machine kept busy since the
// previous measurement (0 before the second one).
func (s *Worker) cpuBusyLocked() float64 {
	now := time.Now()
	if !s.cpuAt.IsZero() && now.Sub(s.cpuAt) < cpuInterval {
		return s.cpuRate
	}
	v, ok := cpuSeconds()
	if !ok {
		return 0
	}
	if !s.cpuAt.IsZero() && v >= s.cpuLast {
		s.cpuRate = (v - s.cpuLast) / now.Sub(s.cpuAt).Seconds()
	}
	s.cpuLast, s.cpuAt = v, now
	return s.cpuRate
}

// statusOnly reports machine state without metrics.
func (s *Worker) statusOnly() proto.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := proto.Status{
		Machine: s.cfg.MachineID, EnvHash: s.cfg.EnvHash, Ready: s.ready,
		CPU: s.cfg.CPU, Memory: s.cfg.Memory, Draining: s.draining || s.diskFull, DrainReason: s.drainReason,
	}
	if s.diskFull && !s.draining {
		st.DrainReason = "disk full"
	}
	if !s.cfg.ExpiresAt.IsZero() {
		st.ExpiresAt = s.cfg.ExpiresAt.Unix()
	}
	for _, se := range s.sessions {
		r := se.req.Grant.Reserve
		st.UsedCPU += r.CPU
		st.UsedMemory += r.Memory
		cur := s.cg.current(se.cgroup)
		st.Measured.Memory += cur
		_, cpuSec := s.cg.usage(se.cgroup)
		st.Sessions = append(st.Sessions, proto.Session{
			ID: se.id, Repo: se.req.Repo, Argv: se.req.Argv, Started: se.started.Unix(),
			CPU: r.CPU, Memory: r.Memory, Lease: se.req.Lease, Labels: labels.Clean(se.req.Labels),
			UsedMemory: cur, CPUSeconds: cpuSec, Samples: se.recentSamples(),
		})
	}
	st.Measured.CPU = s.cpuBusyLocked()
	st.Pressure = pressure()
	if len(s.sessions) == 0 {
		st.IdleSince = s.idleSince.Unix()
	}
	for r := range s.mounts {
		st.Mounts = append(st.Mounts, r)
	}
	st.WarmRepos = s.slots.warmRepos(s.cfg.BuildTTL)
	st.Disk = s.diskStatus()
	st.CacheFS = s.cacheMedium
	if s.cfs != nil {
		st.Heads = s.cfs.flushedHeads()
		st.VM = s.cfs.vm()
	}
	st.Tiers = s.tiers.Snapshot()
	st.Boot = s.bootLocked()
	return st
}

// bootPhase records a phase of the worker's start.
func (s *Worker) bootPhase(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootWorker = append(s.bootWorker, machine.BootPhase{Name: name, At: machine.NowMs()})
}

// bootLocked returns the boot phase record: the supervisor's phases
// (published next to the host status file) followed by the worker's.
func (s *Worker) bootLocked() []machine.BootPhase {
	if !s.bootHost && s.cfg.HostStatus != "" {
		if ph, err := machine.ReadBoot(filepath.Join(filepath.Dir(s.cfg.HostStatus), filepath.Base(machine.BootFile))); err == nil {
			s.boot = ph
			for _, p := range ph {
				if p.Name == machine.PhaseContainers {
					s.bootHost = true
				}
			}
		}
	}
	out := make([]machine.BootPhase, 0, len(s.boot)+len(s.bootWorker))
	return append(append(out, s.boot...), s.bootWorker...)
}

// serveSync receives a root's manifest stream from the client daemon.
func (s *Worker) serveSync(ctx context.Context, conn *quic.Conn, pc *proto.Conn) error {
	var m proto.Mount
	if err := pc.Expect(proto.TMount, &m); err != nil {
		return err
	}
	if !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root || m.Root == "/" {
		return pc.SendError("mount", fmt.Errorf("invalid root %q", m.Root))
	}
	p, err := s.projection(m.Root, m.WorkerLocal)
	if err != nil {
		return pc.SendError("mount", err)
	}
	root := m.Root
	s.mu.Lock()
	s.sources[root] = conn
	close(s.sourceWake)
	s.sourceWake = make(chan struct{})
	s.mu.Unlock()
	if err := pc.Send(proto.TAck, proto.Ack{Version: p.Version()}); err != nil {
		return err
	}
	s.log.Info("root attached", "root", root, "version", p.Version())
	for {
		t, payload, err := pc.Read()
		if err != nil {
			return err
		}
		var d manifest.Delta
		if err := proto.Decode(t, payload, proto.TDelta, &d); err != nil {
			return err
		}
		t0 := time.Now()
		p.Apply(deltaLayer(d), d.Full, d.Version)
		p.Settle()
		if dt := time.Since(t0); dt > 5*time.Second {
			s.log.Warn("slow manifest apply", "root", root, "version", d.Version, "duration", dt.Round(time.Millisecond))
		}
		if err := pc.Send(proto.TAck, proto.Ack{Version: p.Version()}); err != nil {
			return err
		}
	}
}

// projection returns the source projection of root, mounting it at root
// on first use: a writable anyfs file system over the synced manifest
// whose file contents are faulted in from the client (the peer tier).
func (s *Worker) projection(root string, workerLocal []string) (*anyfs.WritableFS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.mounts[root]; ok {
		p.SetMountPoints(workerLocal)
		return p, nil
	}
	upper := filepath.Join(s.cfg.CacheDir, "upper", strings.ReplaceAll(strings.Trim(root, "/"), "/", "_"))
	fetch := s.fetchFile
	if s.sourceFetch != nil {
		fetch = s.sourceFetch
	}
	src := anyfs.NewPeerSource(nil, s.srcFiles, func(ctx context.Context, n *anyfs.Node, w io.Writer) error {
		return fetch(ctx, root, n, w)
	})
	p, err := anyfs.NewWritable(src, upper, anyfs.WritableOptions{FsName: "offshore", AllowOther: os.Geteuid() == 0})
	if err != nil {
		return nil, err
	}
	p.SetMountPoints(workerLocal)
	if err := p.Mount(root); err != nil {
		return nil, fmt.Errorf("mount %s: %w", root, err)
	}
	s.mounts[root] = p
	return p, nil
}

// deltaLayer turns a manifest delta into an anyfs layer (deletions
// before upserts: a path deleted and upserted again shows the upsert).
func deltaLayer(d manifest.Delta) []anyfs.Node {
	out := make([]anyfs.Node, 0, len(d.Deletes)+len(d.Upserts))
	for _, p := range d.Deletes {
		out = append(out, anyfs.Node{Path: p, Whiteout: true})
	}
	for _, e := range d.Upserts {
		out = append(out, anyfs.FromEntry(e))
	}
	return out
}

// fetchFile writes a source file's content to w: from the local store if
// it holds the content (e.g. written back from here), else from the
// client serving the root. (Concurrent faults of one digest share a
// fetch in anyfs.Files.)
//
// Network trouble does not fail the read at once: a blob stream that
// makes no progress for blobStall is abandoned, a lost client connection
// is waited for until the client reconnects and attaches the root again
// (the daemon does so on its own), and the fetch is retried with backoff
// for up to sourceWait. Only then does the build see an I/O error.
func (s *Worker) fetchFile(ctx context.Context, root string, n *anyfs.Node, w io.Writer) error {
	if s.store.Has(n.Digest) {
		f, err := os.Open(s.store.Path(n.Digest))
		if err == nil {
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		}
	}
	var waitStart time.Time
	var lastErr error
	backoff := 250 * time.Millisecond
	for {
		s.mu.Lock()
		conn, wake := s.sources[root], s.sourceWake
		s.mu.Unlock()
		if conn == nil || conn.Context().Err() != nil {
			if lastErr == nil {
				lastErr = errors.New("client not connected")
			}
			if err := s.waitSource(ctx, root, wake, &waitStart, lastErr, nil); err != nil {
				return err
			}
			continue
		}
		written, err := s.fetchOnce(ctx, conn, root, n, w)
		if err == nil {
			return nil
		}
		var be *blobError
		if errors.As(err, &be) || ctx.Err() != nil {
			return err // the client answered, or the caller gave up
		}
		lastErr = err
		var pause <-chan time.Time
		if conn.Context().Err() == nil {
			// The connection looks alive but the stream failed or
			// stalled: try again shortly (or on a new connection).
			pause = time.After(backoff)
			backoff = min(backoff*2, 5*time.Second)
		}
		if werr := s.waitSource(ctx, root, wake, &waitStart, err, pause); werr != nil {
			return werr
		}
		if written > 0 {
			// Part of the content reached w: anyfs restarts the fault.
			return fmt.Errorf("%w: %v", anyfs.ErrRetryFetch, err)
		}
	}
}

// blobStall is how long a blob transfer may make no progress before it is
// abandoned and retried.
var blobStall = 20 * time.Second

// sourceWait bounds how long a source fault keeps retrying after the
// client became unreachable.
func (s *Worker) sourceWait() time.Duration {
	if s.cfg.SourceWait > 0 {
		return s.cfg.SourceWait
	}
	return 60 * time.Second
}

// waitSource blocks until the sources change (a client attached a root
// again), pause fires (nil: none), or the trouble that began at *start
// outlasts sourceWait.
func (s *Worker) waitSource(ctx context.Context, root string, wake <-chan struct{}, start *time.Time, cause error, pause <-chan time.Time) error {
	if start.IsZero() {
		*start = time.Now()
		s.log.Warn("source client unreachable, retrying", "root", root, "wait", s.sourceWait(), "err", cause)
	}
	left := s.sourceWait() - time.Since(*start)
	fail := fmt.Errorf("source client for %s unreachable for %s: %w", root, s.sourceWait(), cause)
	if left <= 0 {
		s.log.Warn("source fetch failed", "root", root, "err", fail)
		return fail
	}
	t := time.NewTimer(left)
	defer t.Stop()
	select {
	case <-wake:
		return nil
	case <-pause:
		return nil
	case <-t.C:
		s.log.Warn("source fetch failed", "root", root, "err", fail)
		return fail
	case <-ctx.Done():
		return ctx.Err()
	}
}

// blobError is an error the client reported for a blob request (not a
// transport failure).
type blobError struct{ msg string }

func (e *blobError) Error() string { return e.msg }

// errBlobStall reports a blob stream without progress for blobStall.
var errBlobStall = errors.New("blob transfer stalled")

// fetchOnce fetches a file over conn and reports how many bytes reached w.
// The stream is abandoned when no data arrives for blobStall.
func (s *Worker) fetchOnce(ctx context.Context, conn *quic.Conn, root string, n *anyfs.Node, w io.Writer) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	octx, ocancel := context.WithTimeout(ctx, blobStall)
	st, pc, err := transport.OpenStream(octx, conn, "", proto.KindBlob)
	ocancel()
	if err != nil {
		return 0, err
	}
	defer st.Close()
	// Progress watchdog: every frame read extends the deadline.
	var progress atomic.Int64
	progress.Store(time.Now().UnixNano())
	stalled := atomic.Bool{}
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(blobStall / 4)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				st.CancelRead(1)
				st.CancelWrite(1)
				return
			case <-t.C:
				if time.Since(time.Unix(0, progress.Load())) > blobStall {
					stalled.Store(true)
					st.CancelRead(1)
					st.CancelWrite(1)
					return
				}
			}
		}
	}()
	wrap := func(err error) error {
		if err != nil && stalled.Load() {
			return fmt.Errorf("%w (%s without data): %v", errBlobStall, blobStall, err)
		}
		return err
	}
	if err := pc.Send(proto.TBlobReq, proto.BlobReq{Root: root, Path: n.Path, Hash: n.Digest}); err != nil {
		return 0, wrap(err)
	}
	var resp proto.BlobResp
	if err := pc.Expect(proto.TBlobResp, &resp); err != nil {
		return 0, wrap(err)
	}
	progress.Store(time.Now().UnixNano())
	if resp.Err != "" {
		return 0, &blobError{resp.Err}
	}
	cw := &countingWriter{w: w, progress: &progress}
	err = pc.ReadData(cw)
	s.countBytes(float64(cw.n), transferFetch)
	return cw.n, wrap(err)
}

type countingWriter struct {
	w        io.Writer
	n        int64
	progress *atomic.Int64 // last write (unix nanoseconds); nil: none
}

func (c *countingWriter) Write(p []byte) (int, error) {
	k, err := c.w.Write(p)
	c.n += int64(k)
	if c.progress != nil {
		c.progress.Store(time.Now().UnixNano())
	}
	return k, err
}

// newWorkerCacheFS opens the build slot file system below the cache
// directory with the writeback thresholds of the configuration: each
// repository's local page tier may hold half the disk budget.
func newWorkerCacheFS(cfg Config, log *slog.Logger, obs anyfs.Observer) (*cacheFS, error) {
	total, _, _ := fsUsage(cfg.CacheDir)
	budget := budgetBytes(cfg.DiskBudget, total)
	ratio := cfg.DirtyRatio
	if ratio <= 0 {
		ratio = 0.1
	}
	return newCacheFS(filepath.Join(cfg.CacheDir, "anyfs"), cacheFSOptions{Mode: cfg.SlotMode, Capacity: budget / 2,
		Idle: cfg.DirtyExpire, DirtyBytes: int64(float64(budget) * ratio), Log: log, Now: cfg.now, Observer: obs})
}

// started approximates the process start (package initialization).
var started = time.Now()

func processStart() time.Time { return started }

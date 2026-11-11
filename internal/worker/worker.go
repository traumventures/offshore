// Package server implements the worker side: projections of client roots,
// session execution, build slots and machine lifecycle.
package worker

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/traumventures/cloudsourced/internal/clock"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/auth"
	"github.com/traumventures/cloudsourced/internal/cas"
	"github.com/traumventures/cloudsourced/internal/machine"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/sourcefs"
	"github.com/traumventures/cloudsourced/internal/tracing"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// Config is read from CLOUDSOURCED_* environment variables set by the
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
	// HostStatus is a file the server publishes machine.HostStatus to for
	// the supervisor (CLOUDSOURCED_HOST_STATUS; empty: none).
	HostStatus string
	// SccacheEndpoint replaces the built-in compiler cache endpoint with an
	// external blobcache (CLOUDSOURCED_SCCACHE_ENDPOINT, set by a host
	// agent). SccacheDir is the built-in cache's directory
	// (CLOUDSOURCED_SCCACHE_DIR, default <deps dir>/sccache: the dependency
	// volume is shared by the owner's machines, the cache volume is not).
	SccacheEndpoint string
	SccacheDir      string
	// Clock is the time of idle, expiry and build slot decisions
	// ($CLOUDSOURCED_TEST_CLOCK; nil: real time).
	Clock clock.Clock
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
		MachineID:  os.Getenv("CLOUDSOURCED_MACHINE_ID"),
		Listen:     envOr("CLOUDSOURCED_LISTEN", ":7443"),
		EnvHash:    os.Getenv("CLOUDSOURCED_ENV_HASH"),
		CacheDir:   envOr("CLOUDSOURCED_CACHE_DIR", "/cache"),
		DepsDir:    envOr("CLOUDSOURCED_DEPS_DIR", "/deps"),
		BuildTTL:   24 * time.Hour,
		HostStatus: os.Getenv(machine.HostStatusEnv),
	}
	c.SccacheEndpoint = os.Getenv("CLOUDSOURCED_SCCACHE_ENDPOINT")
	c.SccacheDir = envOr("CLOUDSOURCED_SCCACHE_DIR", filepath.Join(c.DepsDir, "sccache"))
	clk, err := clock.FromEnv()
	if err != nil {
		return c, err
	}
	c.Clock = clk
	if c.MachineID == "" || os.Getenv(auth.EnvTLSSeed) == "" || os.Getenv(auth.EnvSignerPub) == "" || os.Getenv("CLOUDSOURCED_EXPIRES_AT") == "" {
		return c, errors.New("server: CLOUDSOURCED_MACHINE_ID, _TLS_SEED, _SIGNER_PUB and _EXPIRES_AT are required")
	}
	c.CPU, _ = strconv.ParseFloat(os.Getenv("CLOUDSOURCED_CPU"), 64)
	c.Memory, _ = strconv.ParseInt(os.Getenv("CLOUDSOURCED_MEMORY"), 10, 64)
	n, err := strconv.ParseInt(os.Getenv("CLOUDSOURCED_EXPIRES_AT"), 10, 64)
	if err != nil {
		return c, fmt.Errorf("server: CLOUDSOURCED_EXPIRES_AT: %w", err)
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
	if s := os.Getenv("CLOUDSOURCED_BUILD_TTL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return c, fmt.Errorf("server: CLOUDSOURCED_BUILD_TTL: %w", err)
		}
		c.BuildTTL = d
	}
	if s := os.Getenv("CLOUDSOURCED_PROVISION"); s != "" {
		if err := json.Unmarshal([]byte(s), &c.Provision); err != nil {
			return c, fmt.Errorf("server: CLOUDSOURCED_PROVISION: %w", err)
		}
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

	ccEndpoint string // compiler cache WebDAV endpoint for sessions ("" = off)

	mu       sync.Mutex
	mounts   map[string]*sourcefs.Projection
	sources  map[string]*quic.Conn // root -> client connection serving blobs
	sessions map[string]*session
	// cpuLast is the machine CPU time at cpuAt; Measured.CPU is the rate
	// since then (cores busy), refreshed at most every cpuInterval.
	cpuLast, cpuRate float64
	cpuAt            time.Time
	idleSince        time.Time
	lastProbe        time.Time // last status request (an orchestrator probe)
	ready            bool
	draining         bool
	// drainReason describes a drain the server started on its own
	// (expiry).
	drainReason string

	fetchMu  sync.Mutex
	inflight map[string]*fetchCall

	metrics *serverMetrics
}

type fetchCall struct {
	done chan struct{}
	err  error
}

// New prepares a server.
func New(cfg Config, log *slog.Logger) (*Worker, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	store, err := cas.New(filepath.Join(cfg.CacheDir, "cas"))
	if err != nil {
		return nil, err
	}
	sl, err := newSlots(filepath.Join(cfg.CacheDir, "build"))
	if err != nil {
		return nil, err
	}
	sl.clock = cfg.Clock
	var env []string
	for _, kv := range os.Environ() {
		// Neither pool secrets nor the server's tracing setup (whose
		// headers may hold credentials) reach sessions.
		if !strings.HasPrefix(kv, "CLOUDSOURCED_") && !strings.HasPrefix(kv, "OTEL_") {
			env = append(env, kv)
		}
	}
	s := &Worker{
		cfg: cfg, log: log, store: store, slots: sl, self: self, env: env,
		mounts: map[string]*sourcefs.Projection{}, sources: map[string]*quic.Conn{},
		sessions: map[string]*session{}, idleSince: cfg.now(), lastProbe: cfg.now(), inflight: map[string]*fetchCall{},
	}
	s.metrics = newServerMetrics(s)
	return s, nil
}

// Run provisions the machine and serves until ctx ends or the machine expires.
func (s *Worker) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.cg = setupCgroups(s.log)
	s.startCompilerCache(ctx)
	if n := s.slots.gc(s.cfg.BuildTTL); n > 0 {
		s.log.Info("removed expired build slots", "count", n)
	}
	ln, err := transport.Listen(s.cfg.Listen, &s.cfg.Identity)
	if err != nil {
		return err
	}
	defer ln.Close()
	s.log.Info("listening", "addr", s.cfg.Listen, "machine", s.cfg.MachineID)
	go func() {
		if err := s.provision(ctx); err != nil {
			s.log.Error("machine provisioning failed", "err", err)
			cancel()
			return
		}
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
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	lastGC := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(lastGC) > 5*time.Minute {
			s.slots.gc(s.cfg.BuildTTL)
			lastGC = time.Now()
		}
		if s.cfg.ExpiresAt.IsZero() || s.cfg.now().Before(s.cfg.ExpiresAt) {
			continue
		}
		s.mu.Lock()
		if !s.draining {
			s.log.Info("machine lifetime reached, draining")
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
		se.kill()
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
		CPU: s.cfg.CPU, Memory: s.cfg.Memory, Draining: s.draining, DrainReason: s.drainReason,
	}
	if !s.cfg.ExpiresAt.IsZero() {
		st.ExpiresAt = s.cfg.ExpiresAt.Unix()
	}
	for _, se := range s.sessions {
		r := se.req.Grant.Reserve
		st.UsedCPU += r.CPU
		st.UsedMemory += r.Memory
		st.Measured.Memory += s.cg.current(se.cgroup)
		st.Sessions = append(st.Sessions, proto.Session{
			ID: se.id, Repo: se.req.Repo, Argv: se.req.Argv, Started: se.started.Unix(),
			CPU: r.CPU, Memory: r.Memory, Lease: se.req.Lease,
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
	return st
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
	s.mu.Unlock()
	p.SetFetcher(func(ctx context.Context, rel, hash string) error {
		return s.fetchBlob(ctx, root, rel, hash)
	})
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
		p.Apply(d)
		p.Settle()
		if dt := time.Since(t0); dt > 5*time.Second {
			s.log.Warn("slow manifest apply", "root", root, "version", d.Version, "duration", dt.Round(time.Millisecond))
		}
		if err := pc.Send(proto.TAck, proto.Ack{Version: p.Version()}); err != nil {
			return err
		}
	}
}

func (s *Worker) projection(root string, workerLocal []string) (*sourcefs.Projection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.mounts[root]; ok {
		p.SetWorkerLocal(workerLocal)
		return p, nil
	}
	upper := filepath.Join(s.cfg.CacheDir, "upper", strings.ReplaceAll(strings.Trim(root, "/"), "/", "_"))
	os.RemoveAll(upper)
	p, err := sourcefs.New(root, s.store, upper, workerLocal)
	if err != nil {
		return nil, err
	}
	if err := p.Mount(); err != nil {
		return nil, fmt.Errorf("mount %s: %w", root, err)
	}
	s.mounts[root] = p
	return p, nil
}

// fetchBlob asks the client for a file's content, deduplicating concurrent
// requests for the same hash.
func (s *Worker) fetchBlob(ctx context.Context, root, rel, hash string) error {
	s.fetchMu.Lock()
	if c, ok := s.inflight[hash]; ok {
		s.fetchMu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &fetchCall{done: make(chan struct{})}
	s.inflight[hash] = c
	s.fetchMu.Unlock()
	c.err = s.doFetch(ctx, root, rel, hash)
	s.fetchMu.Lock()
	delete(s.inflight, hash)
	s.fetchMu.Unlock()
	close(c.done)
	return c.err
}

func (s *Worker) doFetch(ctx context.Context, root, rel, hash string) error {
	if s.store.Has(hash) {
		return nil
	}
	s.mu.Lock()
	conn := s.sources[root]
	s.mu.Unlock()
	if conn == nil {
		return errors.New("client not connected")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	st, pc, err := transport.OpenStream(ctx, conn, "", proto.KindBlob)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := pc.Send(proto.TBlobReq, proto.BlobReq{Root: root, Path: rel, Hash: hash}); err != nil {
		return err
	}
	var resp proto.BlobResp
	if err := pc.Expect(proto.TBlobResp, &resp); err != nil {
		return err
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	pr, pw := io.Pipe()
	cr := &countingReader{r: pr}
	go func() { pw.CloseWithError(pc.ReadData(pw)) }()
	_, err = s.store.Put(hash, cr)
	pr.CloseWithError(err)
	s.countBytes(float64(cr.n), transferFetch)
	return err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

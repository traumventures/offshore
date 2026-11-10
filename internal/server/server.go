// Package server implements the worker side: projections of client roots,
// session execution, build slots and machine lifecycle.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/traumventures/cloudsourced/internal/cas"
	"github.com/traumventures/cloudsourced/internal/fusefs"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// Config is read from CLOUDSOURCED_* environment variables set by the
// provider at machine creation.
type Config struct {
	MachineID string
	Token     string
	Identity  transport.Identity
	Listen    string
	CPU       float64
	Memory    int64
	EnvHash   string
	ExpiresAt time.Time
	Provision []string
	CacheDir  string
	DepsDir   string
	BuildTTL  time.Duration // upper bound for build cache age
}

// ConfigFromEnv reads the server configuration.
func ConfigFromEnv() (Config, error) {
	c := Config{
		MachineID: os.Getenv("CLOUDSOURCED_MACHINE_ID"),
		Token:     os.Getenv("CLOUDSOURCED_TOKEN"),
		Listen:    envOr("CLOUDSOURCED_LISTEN", ":7443"),
		EnvHash:   os.Getenv("CLOUDSOURCED_ENV_HASH"),
		CacheDir:  envOr("CLOUDSOURCED_CACHE_DIR", "/cache"),
		DepsDir:   envOr("CLOUDSOURCED_DEPS_DIR", "/deps"),
		BuildTTL:  24 * time.Hour,
	}
	c.Identity.CertPEM = os.Getenv("CLOUDSOURCED_CERT")
	c.Identity.KeyPEM = os.Getenv("CLOUDSOURCED_KEY")
	if c.MachineID == "" || c.Token == "" || c.Identity.CertPEM == "" || c.Identity.KeyPEM == "" {
		return c, errors.New("server: CLOUDSOURCED_MACHINE_ID, _TOKEN, _CERT and _KEY are required")
	}
	c.CPU, _ = strconv.ParseFloat(os.Getenv("CLOUDSOURCED_CPU"), 64)
	c.Memory, _ = strconv.ParseInt(os.Getenv("CLOUDSOURCED_MEMORY"), 10, 64)
	if s := os.Getenv("CLOUDSOURCED_EXPIRES_AT"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return c, fmt.Errorf("server: CLOUDSOURCED_EXPIRES_AT: %w", err)
		}
		c.ExpiresAt = time.Unix(n, 0)
	}
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

// Server is a running worker.
type Server struct {
	cfg   Config
	log   *slog.Logger
	store *cas.Store
	slots *slots
	cg    *cgroups
	self  string
	env   []string // base environment for sessions

	mu        sync.Mutex
	mounts    map[string]*fusefs.Projection
	sources   map[string]*quic.Conn // root -> client connection serving blobs
	sessions  map[string]*session
	idleSince time.Time
	ready     bool
	draining  bool

	fetchMu  sync.Mutex
	inflight map[string]*fetchCall
}

type fetchCall struct {
	done chan struct{}
	err  error
}

// New prepares a server.
func New(cfg Config, log *slog.Logger) (*Server, error) {
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
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLOUDSOURCED_") {
			env = append(env, kv)
		}
	}
	return &Server{
		cfg: cfg, log: log, store: store, slots: sl, self: self, env: env,
		mounts: map[string]*fusefs.Projection{}, sources: map[string]*quic.Conn{},
		sessions: map[string]*session{}, idleSince: time.Now(), inflight: map[string]*fetchCall{},
	}, nil
}

// Run provisions the machine and serves until ctx ends or the machine expires.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.cg = setupCgroups(s.log)
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

func (s *Server) provisionFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ready
}

func (s *Server) provision(ctx context.Context) error {
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
func (s *Server) lifecycle(ctx context.Context, cancel func()) {
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
		if s.cfg.ExpiresAt.IsZero() || time.Now().Before(s.cfg.ExpiresAt) {
			continue
		}
		s.mu.Lock()
		if !s.draining {
			s.log.Info("machine lifetime reached, draining")
		}
		s.draining = true
		n := len(s.sessions)
		s.mu.Unlock()
		if n == 0 || time.Now().After(s.cfg.ExpiresAt.Add(10*time.Minute)) {
			s.log.Info("machine expired, shutting down")
			cancel()
			return
		}
	}
}

func (s *Server) shutdown() {
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

func (s *Server) serveConn(ctx context.Context, conn *quic.Conn) {
	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.serveStream(ctx, conn, st)
	}
}

func (s *Server) serveStream(ctx context.Context, conn *quic.Conn, st *quic.Stream) {
	defer st.Close()
	pc := proto.NewConn(st)
	var hello proto.Hello
	if err := pc.Expect(proto.THello, &hello); err != nil {
		st.CancelRead(1)
		return
	}
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(s.cfg.Token)) != 1 {
		pc.SendError("auth", errors.New("invalid token"))
		st.CancelRead(1)
		return
	}
	var err error
	switch hello.Kind {
	case proto.KindSync:
		err = s.serveSync(ctx, conn, pc)
	case proto.KindExec:
		err = s.serveExec(ctx, pc, st)
	case proto.KindStatus:
		err = pc.Send(proto.TStatus, s.Status())
	case proto.KindFetch:
		err = s.serveFetch(ctx, pc)
	case proto.KindForward:
		err = s.serveForward(ctx, pc, st)
	default:
		err = pc.SendError("protocol", fmt.Errorf("unknown stream kind %q", hello.Kind))
	}
	if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		s.log.Debug("stream ended", "kind", hello.Kind, "err", err)
	}
}

// Status reports machine state.
func (s *Server) Status() proto.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := proto.Status{
		Machine: s.cfg.MachineID, EnvHash: s.cfg.EnvHash, Ready: s.ready,
		CPU: s.cfg.CPU, Memory: s.cfg.Memory, Draining: s.draining,
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
func (s *Server) serveSync(ctx context.Context, conn *quic.Conn, pc *proto.Conn) error {
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
		p.Apply(d)
		p.Settle()
		if err := pc.Send(proto.TAck, proto.Ack{Version: p.Version()}); err != nil {
			return err
		}
	}
}

func (s *Server) projection(root string, workerLocal []string) (*fusefs.Projection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.mounts[root]; ok {
		p.SetWorkerLocal(workerLocal)
		return p, nil
	}
	upper := filepath.Join(s.cfg.CacheDir, "upper", strings.ReplaceAll(strings.Trim(root, "/"), "/", "_"))
	os.RemoveAll(upper)
	p, err := fusefs.New(root, s.store, upper, workerLocal)
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
func (s *Server) fetchBlob(ctx context.Context, root, rel, hash string) error {
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

func (s *Server) doFetch(ctx context.Context, root, rel, hash string) error {
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
	go func() { pw.CloseWithError(pc.ReadData(pw)) }()
	_, err = s.store.Put(hash, pr)
	pr.CloseWithError(err)
	return err
}

package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/creack/pty"
	"github.com/klauspost/compress/zstd"
	"github.com/quic-go/quic-go"
	"github.com/traumventures/offshore/internal/auth"
	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/sandbox"
	"github.com/traumventures/offshore/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type session struct {
	id      string
	req     proto.Exec
	started time.Time
	cmd     *exec.Cmd
	cgroup  string
	cg      *cgroups
	pgid    int
	done    chan struct{} // closed when the process has exited
	endOnce sync.Once
	// killedBy is why offshore killed the session (killer*; "" = it did
	// not); clientSignal the last signal the client forwarded.
	killedBy     atomic.Value
	clientSignal atomic.Int32

	smu     sync.Mutex
	samples []events.Sample // the last maxSessionSamples cgroup samples
}

// addSample keeps s among the session's last samples.
func (se *session) addSample(s events.Sample) {
	se.smu.Lock()
	se.samples = append(se.samples, s)
	if n := len(se.samples); n > maxSessionSamples {
		se.samples = append(se.samples[:0:0], se.samples[n-maxSessionSamples:]...)
	}
	se.smu.Unlock()
}

// recentSamples returns a copy of the session's last samples.
func (se *session) recentSamples() []events.Sample {
	se.smu.Lock()
	defer se.smu.Unlock()
	return append([]events.Sample(nil), se.samples...)
}

// sample measures the session's cgroup every sessionSampleInterval until
// done is closed.
func (se *session) sample(done <-chan struct{}) {
	sm := &sampler{dir: se.cgroup}
	sm.sample(time.Now()) // CPU base
	t := time.NewTicker(sessionSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			se.addSample(sm.sample(now))
		}
	}
}

// killFor kills the session and records why (unless already recorded).
func (se *session) killFor(why string) {
	se.killedBy.CompareAndSwap(nil, why)
	se.kill()
}

func (se *session) killReason() string {
	s, _ := se.killedBy.Load().(string)
	return s
}

// end marks the session's process as exited (idempotent).
func (se *session) end() { se.endOnce.Do(func() { close(se.done) }) }

func (se *session) kill() {
	if se.cgroup != "" {
		se.cg.kill(se.cgroup)
	}
	if se.pgid > 0 {
		syscall.Kill(-se.pgid, syscall.SIGKILL)
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type frame struct {
	t byte
	p []byte
}

// serveExec runs one remote process with full stream, signal and exit code
// forwarding, followed by write back and output transfer.
func (s *Worker) serveExec(ctx context.Context, pc *proto.Conn, st *quic.Stream, claims auth.Claims) (err error) {
	begin := time.Now()
	var req proto.Exec
	if err := pc.Expect(proto.TExec, &req); err != nil {
		return err
	}
	ctx, span := s.startExecSpan(ctx, req)
	defer func() { tracing.End(span, err) }()
	prepErr := func(err error) error {
		pc.SendError("prepare", err)
		return err
	}
	// A session token runs sessions of its own lease only.
	if req.Lease == "" && claims.Scope == auth.ScopeSession {
		req.Lease = claims.Lease
	}
	if !claims.AllowsLease(req.Lease) {
		err := fmt.Errorf("%w: token is not valid for lease %q", auth.ErrInvalidToken, req.Lease)
		pc.SendError("auth", err)
		return err
	}
	if len(req.Argv) == 0 {
		return prepErr(errors.New("empty command"))
	}
	s.mu.Lock()
	ready, draining, full := s.ready, s.draining, s.diskFull
	s.mu.Unlock()
	if !ready {
		s.metrics.rejected.Inc("not_ready")
		return prepErr(errors.New("machine is not ready"))
	}
	if draining {
		s.metrics.rejected.Inc("draining")
		return prepErr(errors.New("machine is draining"))
	}
	if full {
		s.metrics.rejected.Inc("disk_full")
		return prepErr(errors.New("machine cache disk is full"))
	}
	// Barrier: every root must have applied the client's manifest version.
	bctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	for root, v := range req.Roots {
		s.mu.Lock()
		p := s.mounts[root]
		s.mu.Unlock()
		if p == nil {
			cancel()
			return prepErr(fmt.Errorf("root %s is not attached", root))
		}
		if err := p.WaitVersion(bctx, v); err != nil {
			cancel()
			if bctx.Err() != nil && ctx.Err() == nil {
				// Diagnostics for a stalled sync stream (seen rarely under
				// extreme host load): where are the sync loop and the
				// FUSE handlers stuck?
				buf := make([]byte, 256<<10)
				buf = buf[:runtime.Stack(buf, true)]
				s.log.Warn("manifest barrier timed out", "root", root, "want", v, "have", p.Version(), "goroutines", string(buf))
			}
			return prepErr(err)
		}
	}
	cancel()
	barrier := time.Since(begin)
	span.AddEvent("barrier passed", trace.WithAttributes(attribute.Int64("offshore.barrier_ms", barrier.Milliseconds())))
	if _, ok := req.Roots[req.Repo]; !ok {
		return prepErr(fmt.Errorf("repository root %s is not attached", req.Repo))
	}
	if !strings.HasPrefix(req.Cwd+"/", req.Repo+"/") {
		inRoot := false
		for r := range req.Roots {
			if strings.HasPrefix(req.Cwd+"/", r+"/") {
				inRoot = true
			}
		}
		if !inRoot {
			return prepErr(fmt.Errorf("working directory %s is outside the attached roots", req.Cwd))
		}
	}
	// Admission: memory is never overcommitted.
	se := &session{id: newID(), req: req, started: time.Now(), cg: s.cg, done: make(chan struct{})}
	s.mu.Lock()
	var used int64
	for _, o := range s.sessions {
		used += o.req.Grant.Reserve.Memory
	}
	limit := s.cfg.Memory
	if req.MemoryOvercommit > 1 {
		limit = int64(float64(limit) * req.MemoryOvercommit)
	}
	if limit > 0 && used+req.Grant.Reserve.Memory > limit {
		s.mu.Unlock()
		s.metrics.rejected.Inc("full")
		return prepErr(fmt.Errorf("machine full: %d of %d bytes memory reserved", used, limit))
	}
	s.sessions[se.id] = se
	s.mu.Unlock()
	defer se.end()
	// unregister frees the session's reservation. It runs before the exit
	// is sent: the client releases its lease once it has the exit, and
	// the next session placed in that capacity must not find it still
	// reserved here (a slow teardown on a loaded host rejected it as
	// "machine full").
	var unregisterOnce sync.Once
	unregister := func() {
		unregisterOnce.Do(func() {
			s.mu.Lock()
			delete(s.sessions, se.id)
			if len(s.sessions) == 0 {
				s.idleSince = s.cfg.now()
			}
			s.mu.Unlock()
		})
	}
	defer unregister()

	s.noteGrant(req.RepoID, req.Store)
	ttl := s.cfg.BuildTTL
	if d, err := time.ParseDuration(req.SlotTTL); err == nil && d > 0 && d < ttl {
		ttl = d
	}
	slotBegin := time.Now()
	slot, _, err := s.slots.lease(req.RepoID, ttl)
	if err != nil {
		return prepErr(fmt.Errorf("build slot: %w", err))
	}
	warm, err := s.cfs.prepare(ctx, s.slots, slot, req.RepoID, ttl)
	if err != nil {
		s.slots.release(slot)
		return prepErr(fmt.Errorf("build slot: %w", err))
	}
	slotWait := time.Since(slotBegin)
	if slotWait > 10*time.Second {
		s.log.Info("build slot wait", "slot", slot, "seconds", slotWait.Seconds())
	}
	data := filepath.Join(slot, "data")
	var restored bool
	var restoreSecs float64
	if m, err := readMeta(slot); err == nil {
		restored, restoreSecs = restoreUsage(m)
	}
	slotBefore := s.slots.size(slot)
	faultsBefore, refaultsBefore := s.cfs.faultBytes(), s.cfs.refaultBytes()
	slotAfter := int64(-1)
	s.slots.update(slot, func(m *slotMeta) {
		m.Caches = slotCaches(req)
		m.Worktree, m.Branch = req.Labels[labels.Worktree], req.Labels[labels.Branch]
	})
	defer func() {
		// Capture the session before the next one can change the slot
		// (committed in the background by slotReleased).
		s.slots.finishing(slot) // also on early returns
		if err := s.snapshotSlot(context.Background(), slot, req.RepoID); err != nil && !errors.Is(err, errNoSnapshots) {
			s.log.Warn("build slot session snapshot", "slot", slot, "err", err)
		}
		s.slots.releaseSized(slot, slotAfter)
		go s.slotReleased(slot, req.RepoID)
		go s.enforceBudgets(slot)
	}()

	env := s.sessionEnv(req, se.id)
	if err := os.MkdirAll(s.sessionTmp(se.id), 0o1777); err != nil {
		return prepErr(fmt.Errorf("session temporary directory: %w", err))
	}
	defer os.RemoveAll(s.sessionTmp(se.id))
	sccache := req.GlobalCache(config.CacheToolSccache) != nil && s.ccEndpoint != ""
	if sccache {
		if err := os.MkdirAll(s.sccacheDir(se.id), 0o700); err != nil {
			return prepErr(fmt.Errorf("compiler cache: %w", err))
		}
	}
	// Toolchain first: dependency caches ($CARGO_HOME/registry) nest in it.
	binds := s.relocation(req).binds
	cb, err := s.cacheBinds(req, slot, env, binds, frameWriter{pc, proto.TStderr})
	if err != nil {
		return prepErr(err)
	}
	binds = append(binds, cb...)
	s.cfs.sessionStarted(slot, binds)
	cgDir, err := s.cg.create(se.id, req.Grant)
	if err != nil {
		s.log.Warn("session cgroup", "err", err)
	}
	se.cgroup = cgDir
	defer s.cg.remove(cgDir)
	oomBefore := machineOOMKills()
	if cgDir != "" {
		go se.sample(se.done)
	}

	cmd, err := sandbox.Command(s.self, sandbox.Spec{Argv: req.Argv, Env: env, Cwd: req.Cwd, Binds: binds, Cgroup: cgDir})
	if err != nil {
		return prepErr(err)
	}
	cmd.WaitDelay = 2 * time.Second
	se.cmd = cmd

	// Frames from the client are read by one goroutine.
	frames := make(chan frame, 64)
	go func() {
		defer close(frames)
		for {
			t, p, err := pc.Read()
			if err != nil {
				return
			}
			frames <- frame{t, p}
		}
	}()

	var ptmx *os.File
	var stdin io.WriteCloser
	units := &unitScanner{}
	outDone := make(chan struct{})
	if req.TTY != nil {
		ptmx, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: req.TTY.Rows, Cols: req.TTY.Cols})
		if err != nil {
			return prepErr(fmt.Errorf("start: %w", err))
		}
		stdin = ptmx
		go func() {
			defer close(outDone)
			buf := make([]byte, 32<<10)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					units.Write(buf[:n])
					pc.Write(proto.TStdout, buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
	} else {
		cmd.SysProcAttr.Setpgid = true
		cmd.Stdout = frameWriter{pc, proto.TStdout}
		cmd.Stderr = teeWriter{frameWriter{pc, proto.TStderr}, units}
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return prepErr(err)
		}
		if err := cmd.Start(); err != nil {
			return prepErr(fmt.Errorf("start: %w", err))
		}
		close(outDone)
	}
	se.pgid = cmd.Process.Pid
	_, pspan := tracing.Start(ctx, "worker.process")
	span.SetAttributes(attribute.String("offshore.session", se.id), attribute.String("offshore.slot", filepath.Base(slot)), attribute.Bool("offshore.warm", warm))
	s.log.Info("session started", "session", se.id, "argv", req.Argv, "cwd", req.Cwd, "slot", slot)
	pc.Send(proto.TStarted, proto.Started{Session: se.id, Machine: s.cfg.MachineID, Slot: filepath.Base(slot),
		WaitMs: time.Since(begin).Milliseconds(), BarrierMs: barrier.Milliseconds(), SlotMs: slotWait.Milliseconds()})

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	var waitErr error
	clientGone := false
loop:
	for {
		select {
		case waitErr = <-waitDone:
			break loop
		case f, ok := <-frames:
			if !ok {
				if !clientGone {
					clientGone = true
					s.log.Info("client disconnected, killing session", "session", se.id)
					se.killFor(killerClient)
				}
				frames = nil
				continue
			}
			switch f.t {
			case proto.TStdin:
				stdin.Write(f.p)
			case proto.TStdinEOF:
				if ptmx == nil {
					stdin.Close()
				}
			case proto.TSignal:
				var sig proto.Signal
				if json.Unmarshal(f.p, &sig) == nil && sig.Sig > 0 {
					se.clientSignal.Store(int32(sig.Sig))
					syscall.Kill(-se.pgid, syscall.Signal(sig.Sig))
				}
			case proto.TResize:
				var rs proto.Resize
				if json.Unmarshal(f.p, &rs) == nil && ptmx != nil {
					pty.Setsize(ptmx, &pty.Winsize{Rows: rs.Rows, Cols: rs.Cols})
				}
			}
		}
	}
	if ptmx != nil {
		<-outDone
		ptmx.Close()
	}
	exit := proto.Exit{}
	if sccache {
		// Before the kill: the session's sccache server runs in its cgroup.
		if clientGone {
			s.stopSccache(env, se.id)
		} else {
			exit.Cache = s.sccacheStats(env, se.id)
		}
	}
	// Nothing of the session may outlive it, forwarded ports included.
	se.end()
	se.kill()
	if cmd.ProcessState != nil {
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		switch {
		case ws.Signaled():
			exit.Signal = int(ws.Signal())
			exit.Code = 128 + exit.Signal
		default:
			exit.Code = ws.ExitStatus()
		}
	} else if waitErr != nil {
		exit.Code = 127
	}
	pspan.End()
	exit.Usage.Wall = time.Since(se.started)
	exit.Usage.PeakMemory, exit.Usage.CPUSeconds = s.cg.usage(cgDir)
	exit.Usage.CPUStall, exit.Usage.MemoryStall = s.cg.stall(cgDir)
	exit.Usage.Warm = warm
	exit.Usage.Units, exit.Usage.UnitsSample, exit.Usage.DirtyReasons = units.result()
	reads, readBytes, tracked := s.cfs.sessionEnded(slot, req.RepoID)
	if !tracked {
		reads = nil // the read index falls back to atime
	}
	slotAfter = s.recordSlotRun(slot, se.started, reads)
	exit.Usage.SlotBytesBefore, exit.Usage.SlotBytesAfter = slotBefore, slotAfter
	// Working set: the files the session read (tracked) or the slot
	// content faulted in during it (machine wide, an upper bound with
	// concurrent sessions), plus what it grew.
	if tracked {
		exit.Usage.WorkingSetBytes = readBytes + max(0, slotAfter-slotBefore)
	} else {
		exit.Usage.WorkingSetBytes = s.cfs.faultBytes() - faultsBefore + max(0, slotAfter-slotBefore)
	}
	// Thrashing: pages refaulted during the session (machine wide); the
	// orchestrator turns them into thrash time with the size's stack.
	exit.Usage.RefaultBytes = max(0, s.cfs.refaultBytes()-refaultsBefore)
	exit.Usage.LocalCapacity = s.cfs.capacity()
	exit.Usage.Restored, exit.Usage.RestoreSeconds = restored, restoreSecs
	exit.Usage.ExitCode = exit.Code
	mev := s.cg.memoryEvents(cgDir)
	oomDelta := int64(-1)
	if after := machineOOMKills(); oomBefore >= 0 && after >= 0 {
		oomDelta = after - oomBefore
	}
	exit.Usage.Signal, exit.Usage.OOM, exit.Usage.Killer = exitCause(exit.Code, exit.Signal, mev, oomDelta, se.killReason(), int(se.clientSignal.Load()))
	exit.Usage.OOMKills = mev.OOMKill + mev.OOMGroupKill
	exit.Usage.MemoryMaxEvents, exit.Usage.MemoryHighEvents = mev.Max, mev.High
	if exit.Usage.OOM {
		msg := fmt.Sprintf("offshore: session exceeded its memory limit (%d bytes); the next run reserves more\n", req.Grant.MemoryMax)
		if exit.Usage.Killer == killerMachineOOM {
			msg = "offshore: the machine ran out of memory and the kernel killed the session; the next run reserves more\n"
		}
		pc.Write(proto.TStderr, []byte(msg))
	}
	s.log.Info("session finished", "session", se.id, "code", exit.Code, "oom", exit.Usage.OOM, "killer", exit.Usage.Killer, "duration", time.Since(se.started).Round(time.Millisecond))
	s.metrics.sessionEnded(exit.Code, exit.Usage.Wall.Seconds(), exit.Usage.OOM, exit.Cache)
	unregister() // the session's processes are gone
	// The client starts its next session as soon as it has the exit (and
	// the write back): that session waits for this slot instead of taking
	// another while the snapshot runs.
	s.slots.finishing(slot)
	exitAttrs(span, exit, req, clientGone)
	if clientGone {
		return nil
	}
	if err := pc.Send(proto.TExit, exit); err != nil {
		return err
	}
	_, wspan := tracing.Start(ctx, "worker.writeback")
	err = s.writeback(pc, frames, req)
	tracing.End(wspan, err)
	if err != nil {
		return err
	}
	_, ospan := tracing.Start(ctx, "worker.outputs")
	err = s.sendOutputs(pc, frames, req.Repo, data, req.SlotPaths(), req.Eager)
	tracing.End(ospan, err)
	if err != nil {
		return err
	}
	return pc.Write(proto.TDone, nil)
}

func (s *Worker) sessionEnv(req proto.Exec, id string) []string {
	m := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	for _, kv := range s.env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for k, v := range req.Env {
		set(k, v)
	}
	if _, ok := m["HOME"]; !ok {
		set("HOME", "/root")
	}
	if req.TTY != nil && req.TTY.Term != "" {
		set("TERM", req.TTY.Term)
	}
	if j := req.Grant.Jobs; j > 0 {
		set("OFFSHORE_JOBS", strconv.Itoa(j))
		if _, ok := req.Env["CARGO_BUILD_JOBS"]; !ok {
			set("CARGO_BUILD_JOBS", strconv.Itoa(j))
		}
	}
	for _, kv := range s.relocation(req).env {
		set(kv[0], kv[1])
	}
	if l := req.Linker; l != "" && req.Toolchain != nil && validLinker(l) {
		if dir, err := linkerShim(l); err != nil {
			s.log.Warn("linker shim", "err", err)
		} else {
			set("PATH", dir+":"+m["PATH"])
		}
	}
	for _, kv := range s.compilerCacheEnv(req, id, func(k string) bool { _, ok := m[k]; return ok }) {
		set(kv[0], kv[1])
	}
	// Temporary files go to the session's directory on the cache medium
	// (the client's $TMPDIR is meaningless here).
	set("TMPDIR", s.sessionTmp(id))
	set("PWD", req.Cwd)
	set("OFFSHORE_ACTIVE", "1")
	set("OFFSHORE_SESSION", id)
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

func expandPath(p string, env []string) string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	if strings.HasPrefix(p, "~/") {
		p = m["HOME"] + p[1:]
	}
	return filepath.Clean(os.Expand(p, func(k string) string { return m[k] }))
}

type frameWriter struct {
	pc *proto.Conn
	t  byte
}

func (w frameWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += proto.ChunkSize {
		end := min(off+proto.ChunkSize, len(p))
		if err := w.pc.Write(w.t, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// next returns the next client frame of type t.
func next(frames chan frame, t byte, v any) error {
	for f := range frames {
		switch f.t {
		case proto.TStdin, proto.TStdinEOF, proto.TSignal, proto.TResize:
			continue // late input after exit
		}
		return proto.Decode(f.t, f.p, t, v)
	}
	return io.ErrUnexpectedEOF
}

// writeback sends local modifications of the projected roots to the client.
func (s *Worker) writeback(pc *proto.Conn, frames chan frame, req proto.Exec) error {
	var wb proto.Writeback
	blobs := map[string]string{} // hash -> file
	for root := range req.Roots {
		s.mu.Lock()
		p := s.mounts[root]
		s.mu.Unlock()
		if p == nil {
			continue
		}
		for _, d := range p.Changes() {
			c := proto.Change{Root: root, Path: d.Path, Deleted: d.Deleted, Kind: d.Kind, Mode: d.Mode, Target: d.Target, BaseHash: d.BaseDigest}
			if !d.BaseExists {
				c.BaseHash = ""
			} else if c.BaseHash == "" {
				c.BaseHash = "-" // existed, not a regular file
			}
			if !d.Deleted && d.Kind == manifest.File {
				h, err := s.store.PutFile(d.Upper)
				if err != nil {
					continue // removed concurrently
				}
				fi, _ := os.Stat(d.Upper)
				c.Hash = h
				if fi != nil {
					c.Size = fi.Size()
				}
				blobs[h] = s.store.Path(h)
			}
			wb.Changes = append(wb.Changes, c)
		}
	}
	if err := pc.Send(proto.TWriteback, wb); err != nil {
		return err
	}
	if err := s.sendNeeded(pc, frames, blobs, transferWriteback); err != nil {
		return err
	}
	var res proto.WritebackResult
	if err := next(frames, proto.TWritebackResult, &res); err != nil {
		return err
	}
	byRoot := map[string][]string{}
	for _, r := range res.Rejected {
		root, p, _ := strings.Cut(r, "\x00")
		byRoot[root] = append(byRoot[root], p)
	}
	for root, paths := range byRoot {
		s.mu.Lock()
		p := s.mounts[root]
		s.mu.Unlock()
		if p != nil {
			p.Discard(paths)
		}
	}
	return nil
}

func (s *Worker) sendNeeded(pc *proto.Conn, frames chan frame, blobs map[string]string, direction string) error {
	var need proto.Need
	if err := next(frames, proto.TNeed, &need); err != nil {
		return err
	}
	for _, h := range need.Hashes {
		file, ok := blobs[h]
		if !ok {
			return pc.SendError("transfer", fmt.Errorf("unknown blob %s", h))
		}
		f, err := os.Open(file)
		if err != nil {
			return pc.SendError("transfer", err)
		}
		fi, _ := f.Stat()
		err = sendBlob(pc, h, fi.Size(), f, need.Compress)
		f.Close()
		if err != nil {
			return err
		}
		s.countBytes(float64(fi.Size()), direction)
	}
	return nil
}

// sendBlob sends one blob, compressed when the receiver asked for it.
func sendBlob(pc *proto.Conn, hash string, size int64, r io.Reader, compress string) error {
	if compress != proto.CompressZstd {
		return pc.SendBlob(hash, size, r)
	}
	pr, pw := io.Pipe()
	go func() {
		enc, err := zstd.NewWriter(pw, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(enc, r); err != nil {
			enc.Close()
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(enc.Close())
	}()
	err := pc.SendBlob(hash, size, pr)
	pr.Close()
	return err
}

// collectOutputs resolves output globs inside worker-local build directories.
func collectOutputs(data string, build, globs []string) ([]manifest.Entry, map[string]string) {
	var files []manifest.Entry
	blobs := map[string]string{}
	seen := map[string]bool{}
	for _, g := range globs {
		g = strings.TrimPrefix(filepath.ToSlash(g), "/")
		for _, b := range build {
			b = filepath.ToSlash(filepath.Clean(b))
			rest, ok := strings.CutPrefix(g, b+"/")
			if !ok {
				continue
			}
			base := filepath.Join(data, b)
			matches, _ := doublestar.Glob(os.DirFS(base), rest, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
			for _, m := range matches {
				rel := b + "/" + m
				if seen[rel] {
					continue
				}
				full := filepath.Join(base, m)
				fi, err := os.Lstat(full)
				if err != nil || !fi.Mode().IsRegular() {
					continue
				}
				h, n, err := manifest.HashFile(full)
				if err != nil {
					continue
				}
				seen[rel] = true
				files = append(files, manifest.Entry{Path: rel, Kind: manifest.File, Mode: uint32(fi.Mode().Perm()), Size: n, MtimeNs: fi.ModTime().UnixNano(), Hash: h})
				blobs[h] = full
			}
		}
	}
	return files, blobs
}

func (s *Worker) sendOutputs(pc *proto.Conn, frames chan frame, repo, data string, build, globs []string) error {
	files, blobs := collectOutputs(data, build, globs)
	if err := pc.Send(proto.TOutputs, proto.Outputs{Files: files}); err != nil {
		return err
	}
	if err := s.sendNeeded(pc, frames, blobs, transferOutput); err != nil {
		return err
	}
	return next(frames, proto.TDone, nil)
}

// serveFetch transfers output files from the most recent build slot.
func (s *Worker) serveFetch(ctx context.Context, pc *proto.Conn) error {
	var req proto.Fetch
	if err := pc.Expect(proto.TFetch, &req); err != nil {
		return err
	}
	slot, err := s.slots.latest(req.RepoID, s.cfg.BuildTTL)
	if err != nil {
		return pc.SendError("fetch", err)
	}
	s.mountSlot(slot, req.RepoID)
	// Build directories are the top-level directories of the slot.
	var build []string
	des, _ := os.ReadDir(filepath.Join(slot, "data"))
	for _, de := range des {
		if de.IsDir() {
			build = append(build, de.Name())
		}
	}
	frames := make(chan frame, 8)
	go func() {
		defer close(frames)
		for {
			t, p, err := pc.Read()
			if err != nil {
				return
			}
			frames <- frame{t, p}
		}
	}()
	return s.sendOutputs(pc, frames, req.Repo, filepath.Join(slot, "data"), build, req.Paths)
}

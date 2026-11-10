package server

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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/creack/pty"
	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/auth"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/sandbox"
)

type session struct {
	id      string
	req     proto.Exec
	started time.Time
	cmd     *exec.Cmd
	cgroup  string
	cg      *cgroups
	pgid    int
}

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
func (s *Server) serveExec(ctx context.Context, pc *proto.Conn, st *quic.Stream, claims auth.Claims) error {
	begin := time.Now()
	var req proto.Exec
	if err := pc.Expect(proto.TExec, &req); err != nil {
		return err
	}
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
	ready, draining := s.ready, s.draining
	s.mu.Unlock()
	if !ready {
		return prepErr(errors.New("machine is not ready"))
	}
	if draining {
		return prepErr(errors.New("machine is draining"))
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
			return prepErr(err)
		}
	}
	cancel()
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
	se := &session{id: newID(), req: req, started: time.Now(), cg: s.cg}
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
		return prepErr(fmt.Errorf("machine full: %d of %d bytes memory reserved", used, limit))
	}
	s.sessions[se.id] = se
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, se.id)
		if len(s.sessions) == 0 {
			s.idleSince = time.Now()
		}
		s.mu.Unlock()
	}()

	ttl := s.cfg.BuildTTL
	if d, err := time.ParseDuration(req.BuildTTL); err == nil && d > 0 && d < ttl {
		ttl = d
	}
	slot, warm, err := s.slots.lease(req.RepoID, ttl)
	if err != nil {
		return prepErr(fmt.Errorf("build slot: %w", err))
	}
	defer s.slots.release(slot)
	data := filepath.Join(slot, "data")

	env := s.sessionEnv(req, se.id)
	var binds []sandbox.Bind
	for _, b := range req.Build {
		b = filepath.Clean(b)
		binds = append(binds, sandbox.Bind{Src: filepath.Join(data, b), Dst: filepath.Join(req.Repo, b)})
	}
	for _, d := range req.Deps {
		dst := expandPath(d, env)
		if !filepath.IsAbs(dst) {
			continue
		}
		src := filepath.Join(s.cfg.DepsDir, strings.ReplaceAll(strings.Trim(dst, "/"), "/", "__"))
		binds = append(binds, sandbox.Bind{Src: src, Dst: dst})
	}
	cgDir, err := s.cg.create(se.id, req.Grant)
	if err != nil {
		s.log.Warn("session cgroup", "err", err)
	}
	se.cgroup = cgDir
	defer s.cg.remove(cgDir)

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
		cmd.Stderr = frameWriter{pc, proto.TStderr}
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
	s.log.Info("session started", "session", se.id, "argv", req.Argv, "cwd", req.Cwd, "slot", slot)
	pc.Send(proto.TStarted, proto.Started{Session: se.id, Machine: s.cfg.MachineID, Slot: filepath.Base(slot), WaitMs: time.Since(begin).Milliseconds()})

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
					se.kill()
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
	// Nothing of the session may outlive it.
	se.kill()
	exit := proto.Exit{}
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
	exit.Usage.Wall = time.Since(se.started)
	exit.Usage.PeakMemory, exit.Usage.CPUSeconds = s.cg.usage(cgDir)
	exit.Usage.Warm = warm
	exit.Usage.ExitCode = exit.Code
	if s.cg.oomKilled(cgDir) {
		exit.Usage.OOM = true
		pc.Write(proto.TStderr, []byte(fmt.Sprintf("cloudsourced: session exceeded its memory limit (%d bytes); the next run reserves more\n", req.Grant.MemoryMax)))
	}
	s.log.Info("session finished", "session", se.id, "code", exit.Code, "duration", time.Since(se.started).Round(time.Millisecond))
	if clientGone {
		return nil
	}
	if err := pc.Send(proto.TExit, exit); err != nil {
		return err
	}
	if err := s.writeback(pc, frames, req); err != nil {
		return err
	}
	if err := s.sendOutputs(pc, frames, req.Repo, data, req.Build, req.Eager); err != nil {
		return err
	}
	return pc.Write(proto.TDone, nil)
}

func (s *Server) sessionEnv(req proto.Exec, id string) []string {
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
		set("CLOUDSOURCED_JOBS", strconv.Itoa(j))
		if _, ok := req.Env["CARGO_BUILD_JOBS"]; !ok {
			set("CARGO_BUILD_JOBS", strconv.Itoa(j))
		}
	}
	set("PWD", req.Cwd)
	set("CLOUDSOURCED_ACTIVE", "1")
	set("CLOUDSOURCED_SESSION", id)
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
func (s *Server) writeback(pc *proto.Conn, frames chan frame, req proto.Exec) error {
	var wb proto.Writeback
	blobs := map[string]string{} // hash -> file
	for root := range req.Roots {
		s.mu.Lock()
		p := s.mounts[root]
		s.mu.Unlock()
		if p == nil {
			continue
		}
		for _, d := range p.Dirty() {
			c := proto.Change{Root: root, Path: d.Path, Deleted: d.Deleted, Kind: d.Kind, Mode: d.Mode, Target: d.Target, BaseHash: d.BaseHash}
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
	if err := s.sendNeeded(pc, frames, blobs); err != nil {
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

func (s *Server) sendNeeded(pc *proto.Conn, frames chan frame, blobs map[string]string) error {
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
		err = pc.SendBlob(h, fi.Size(), f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
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

func (s *Server) sendOutputs(pc *proto.Conn, frames chan frame, repo, data string, build, globs []string) error {
	files, blobs := collectOutputs(data, build, globs)
	if err := pc.Send(proto.TOutputs, proto.Outputs{Files: files}); err != nil {
		return err
	}
	if err := s.sendNeeded(pc, frames, blobs); err != nil {
		return err
	}
	return next(frames, proto.TDone, nil)
}

// serveFetch transfers output files from the most recent build slot.
func (s *Server) serveFetch(ctx context.Context, pc *proto.Conn) error {
	var req proto.Fetch
	if err := pc.Expect(proto.TFetch, &req); err != nil {
		return err
	}
	slot, err := s.slots.latest(req.RepoID, s.cfg.BuildTTL)
	if err != nil {
		return pc.SendError("fetch", err)
	}
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

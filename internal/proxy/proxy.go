// Package proxy implements the transparent command proxy: it decides
// whether a command runs locally or remotely and, for remote runs, behaves
// exactly like the local process (streams, terminal, signals, exit code),
// then writes back changed sources and transfers outputs.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/traumventures/offshore/internal/adapter"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/daemon"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/tracing"
	"github.com/traumventures/offshore/internal/transport"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/term"
)

// progressOut receives the daemon's progress messages (machine start,
// image build) during preparation.
var progressOut io.Writer = os.Stderr

// defaultEnv is always forwarded in addition to the tool's allowlist.
var defaultEnv = []string{"TERM", "COLORTERM", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "LANG", "LC_*"}

// Main runs tool with args and returns the exit code.
func Main(tool string, args []string) int {
	if os.Getenv("OFFSHORE_ACTIVE") != "" {
		return execLocal(tool, args) // nested inside a remote session
	}
	tr := startTrace(tool, args)
	code := mainRun(tr, tool, args)
	tr.finish(code)
	return code
}

func mainRun(tr *runTrace, tool string, args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return execLocal(tool, args)
	}
	rootDir, err := config.FindRepo(cwd)
	if err != nil {
		return execLocal(tool, args)
	}
	rec := &events.Run{Time: time.Now(), Repo: rootDir, Cwd: cwd, Tool: tool, Args: args, Mode: events.Local,
		Labels: labels.Detect(rootDir, "", os.Getenv)}
	tr.rec = rec
	local := func(why string) int {
		rec.Why = why
		recordLocal(rec)
		return execLocal(tool, args)
	}
	if os.Getenv("OFFSHORE_DISABLE") != "" {
		return local("OFFSHORE_DISABLE is set")
	}
	repo, err := config.LoadRepo(rootDir)
	if err != nil {
		warn("invalid configuration, running locally: %v", err)
		return local("invalid configuration: " + err.Error())
	}
	rec.Repo = repo.Root
	profile, ok := repo.Tools[tool]
	if !ok {
		return local(fmt.Sprintf("no [tool.%s] section in %s", tool, config.RepoFileName))
	}
	plan := adapter.Classify(adapter.Command{Tool: tool, Args: args, Config: profile})
	rec.Adapter, rec.Phases = plan.Adapter, plan.String()
	if !plan.Remote() {
		return local(plan.Why)
	}
	rec.Mode, rec.Why = events.Remote, plan.Why
	rt := &phaseRuntime{repo: repo, profile: profile, tool: tool, args: args, cwd: cwd, rec: rec}
	code, err := plan.Execute(tr.ctx, rt)
	if err != nil {
		var fb *fallback
		if errors.As(err, &fb) {
			if os.Getenv("OFFSHORE_REQUIRE_REMOTE") != "" {
				warn("remote execution failed: %v", fb.err)
				rec.Mode, rec.Why = events.Failed, "remote required but failed: "+fb.err.Error()
				recordLocal(rec)
				return 125
			}
			warn("running locally: %v", fb.err)
			rec.Mode = events.Local
			return local("fallback: " + fb.err.Error())
		}
		warn("%v", err)
		if code == 0 {
			code = 125
		}
	}
	return code
}

// recordLocal appends a run that did not reach the daemon's release call
// to the run log in the daemon's state directory.
func recordLocal(rec *events.Run) {
	rec.Timings.Total = events.Ms(time.Since(rec.Time))
	events.Append(daemon.StateDir(), *rec)
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "offshore: "+format+"\n", a...)
}

// fallback marks errors that happened before anything ran remotely.
type fallback struct{ err error }

func (f *fallback) Error() string { return f.err.Error() }

func execLocal(tool string, args []string) int {
	bin, err := adapter.RealBinary(tool)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	active.finish(-1) // the process is replaced: export spans now
	err = syscall.Exec(bin, append([]string{tool}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "offshore: exec %s: %v\n", bin, err)
	return 126
}

func allowedEnv(patterns []string) map[string]string {
	out := map[string]string{}
	all := append(append([]string{}, defaultEnv...), patterns...)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, p := range all {
			if p == k || (strings.HasSuffix(p, "*") && strings.HasPrefix(k, strings.TrimSuffix(p, "*"))) {
				out[k] = v
				break
			}
		}
	}
	return out
}

// remoteRun adjusts one remote execution. The zero value runs the
// command as typed, attached to the local terminal.
type remoteRun struct {
	argv   []string          // command line instead of tool and args
	stdout io.Writer         // instead of os.Stdout
	stderr io.Writer         // instead of os.Stderr
	noTTY  bool              // never allocate a remote terminal
	stdin  bool              // with noTTY: still forward stdin by the usual policy
	env    map[string]string // additional environment
	// after runs once the remote command exited and its outputs arrived,
	// while the lease is still held; code is the remote exit code.
	after func(ctx context.Context, s *session, code int) error
}

// session is a finished remote session whose connection and lease are
// still open.
type session struct {
	conn   *quic.Conn
	token  string
	repoID string
	root   string
}

// runRemote runs the command on a machine. When the machine is lost while
// the command runs and nothing was written back yet, it runs it once more
// on another machine (see retryable).
func runRemote(parent context.Context, repo *config.Repo, profile config.Tool, tool string, args []string, cwd string, rec *events.Run, opt remoteRun) (int, error) {
	for attempt := 1; ; attempt++ {
		st := &attemptState{}
		code, err := runAttempt(parent, repo, profile, tool, args, cwd, rec, opt, st)
		var lost *machineLost
		if !errors.As(err, &lost) {
			return code, err
		}
		ok, why := retryable(st, attempt, os.Getenv("OFFSHORE_NO_RETRY") != "", lost.probe)
		if !ok {
			rec.Error = lost.Error() + " (not retried: " + why + ")"
			daemon.Call(context.Background(), daemon.Request{Op: "record", Run: rec}, nil)
			return code, lost
		}
		warn("machine %s was lost while the command ran (%v); nothing was written back, running it again on another machine", lost.machine, lost.err)
		rec.LostMachine = lost.machine
	}
}

// attemptState is what a remote attempt did that decides whether it may be
// repeated.
type attemptState struct {
	started     atomic.Bool // the session started on the machine
	writeback   atomic.Bool // the write-back phase began
	stdinUsed   atomic.Bool // local stdin bytes were forwarded (cannot be replayed)
	interrupted atomic.Bool // the user signalled the command
}

// machineLost is a session that ended without an exit status because the
// connection to its machine failed.
type machineLost struct {
	machine string
	err     error
	// probe reports whether the machine still answers (then the session
	// failed for another reason and is not retried).
	probe func() bool
}

func (m *machineLost) Error() string {
	return fmt.Sprintf("remote session: connection to machine %s lost: %v", m.machine, m.err)
}

func (m *machineLost) Unwrap() error { return m.err }

// retryable decides whether a lost attempt runs again on another machine.
// Write-back stays exactly-once: local files only change in the write-back
// phase of the attempt that reported an exit status, so a lost attempt has
// applied nothing, and an attempt whose write-back began is never repeated.
func retryable(st *attemptState, attempt int, disabled bool, probe func() bool) (bool, string) {
	switch {
	case disabled:
		return false, "OFFSHORE_NO_RETRY is set"
	case attempt > 1:
		return false, "already retried once"
	case !st.started.Load():
		return false, "the session had not started"
	case st.writeback.Load():
		return false, "write-back had begun"
	case st.stdinUsed.Load():
		return false, "standard input was consumed and cannot be replayed"
	case st.interrupted.Load():
		return false, "the command was interrupted"
	case probe != nil && probe():
		return false, "the machine is still reachable"
	}
	return true, ""
}

// machineAnswers reports whether a machine accepts connections again.
func machineAnswers(addr, fingerprint string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, addr, fingerprint)
	if err != nil {
		return false
	}
	conn.CloseWithError(0, "")
	return true
}

func runAttempt(parent context.Context, repo *config.Repo, profile config.Tool, tool string, args []string, cwd string, rec *events.Run, opt remoteRun, st *attemptState) (code int, err error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// Ctrl+C during preparation aborts cleanly.
	prepSig := make(chan os.Signal, 1)
	signal.Notify(prepSig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if _, ok := <-prepSig; ok {
			cancel()
		}
	}()
	t0 := time.Now()
	pctx, pspan := tracing.Start(ctx, "proxy.prepare")
	if err := daemon.EnsureDaemon(pctx); err != nil {
		tracing.End(pspan, err)
		return 0, &fallback{err}
	}
	raw, err := daemon.Call(pctx, daemon.Request{Op: "prepare", Root: repo.Root, Cwd: cwd, Tool: tool, Args: args,
		Labels: rec.Labels, TraceParent: tracing.Inject(pctx)}, progressOut)
	tracing.End(pspan, err)
	signal.Stop(prepSig)
	close(prepSig)
	if err != nil {
		if ctx.Err() != nil {
			return 130, nil
		}
		return 0, &fallback{err}
	}
	var prep daemon.Prepared
	if err := json.Unmarshal(raw, &prep); err != nil {
		return 0, &fallback{err}
	}
	rec.Machine, rec.Provider, rec.Size, rec.Price, rec.Placement = prep.Machine, prep.Provider, prep.Size, prep.PricePerHour, prep.Reason
	rec.Lease, rec.Timings = prep.Lease, prep.Timings
	if prep.Labels != nil {
		rec.Labels = prep.Labels
	}
	release := daemon.Request{Op: "release", Lease: prep.Lease, UsageKey: prep.UsageKey, TraceParent: tracing.Inject(ctx)}
	started := false
	defer func() {
		var lost *machineLost
		if errors.As(err, &lost) {
			// The lease's machine is gone; runRemote records the run
			// unless it retries.
			daemon.Call(context.Background(), daemon.Request{Op: "unreachable", Lease: prep.Lease, Machine: prep.Machine}, nil)
		}
		// Runs that fall back to local execution are recorded by Main.
		if started {
			if err != nil {
				rec.Error = err.Error()
			}
			c := code
			rec.Exit = &c
			rec.Timings.Total = events.Ms(time.Since(t0))
			release.Run = rec
		}
		if lost != nil {
			release.Run = nil
		}
		daemon.Call(context.Background(), release, nil)
	}()
	fwdPorts, err := ports(profile)
	if err != nil {
		return 0, &fallback{err}
	}
	fwd := listenPorts(fwdPorts)
	defer fwd.close()
	rec.Ports = fwd.forwarded()

	env := allowedEnv(profile.Env)
	for k, v := range opt.env {
		env[k] = v
	}
	for name, ref := range repo.Secrets {
		v, err := resolveSecret(ref, repo.Root)
		if err != nil {
			return 0, &fallback{fmt.Errorf("secret %s: %w", name, err)}
		}
		env[name] = v
	}
	useTTY := !opt.noTTY && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	argv := opt.argv
	if argv == nil {
		argv = append([]string{tool}, args...)
	}
	req := proto.Exec{
		Repo: repo.Root, RepoID: prep.RepoID, Roots: prep.Roots,
		Argv: argv, Cwd: cwd, Env: env,
		Grant: prep.Grant, MemoryOvercommit: prep.MemoryOvercommit,
		Caches: ProtoCaches(repo), SlotTTL: prep.BuildTTL, Store: prep.Store,
		Eager: profile.Eager, Lease: prep.Lease,
		Linker: repo.Environment.Linker, TraceParent: tracing.Inject(ctx), Labels: rec.Labels,
	}
	if repo.Outputs.Mode == config.OutputsFuse {
		// The remote build directory appears locally (output overlay):
		// align what cargo fingerprints so local cargo finds it fresh.
		req.Toolchain = clientToolchain()
		for k, v := range cargoHomeRustflags(req.Toolchain.CargoHome, env) {
			env[k] = v
		}
	}
	if useTTY {
		cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			cols, rows = 80, 24
		}
		req.TTY = &proto.TTY{Rows: uint16(rows), Cols: uint16(cols), Term: os.Getenv("TERM")}
	}
	tc := time.Now()
	cctx, cspan := tracing.Start(ctx, "proxy.connect", trace.WithAttributes(attribute.String("offshore.machine", prep.Machine)))
	dctx, dcancel := context.WithTimeout(cctx, 15*time.Second)
	conn, err := transport.Dial(dctx, prep.Addr, prep.Fingerprint)
	dcancel()
	if err != nil {
		tracing.End(cspan, err)
		return 0, &fallback{fmt.Errorf("connect to machine %s: %w", prep.Machine, err)}
	}
	defer conn.CloseWithError(0, "")
	qs, pc, err := transport.OpenStream(cctx, conn, prep.Token, proto.KindExec)
	if err != nil {
		tracing.End(cspan, err)
		return 0, &fallback{err}
	}
	defer qs.Close()
	if err := pc.Send(proto.TExec, req); err != nil {
		tracing.End(cspan, err)
		return 0, &fallback{err}
	}
	var sstarted proto.Started
	if err := pc.Expect(proto.TStarted, &sstarted); err != nil {
		tracing.End(cspan, err)
		return 0, &fallback{err}
	}
	cspan.SetAttributes(attribute.String("offshore.session", sstarted.Session), attribute.Int64("offshore.server_wait_ms", sstarted.WaitMs))
	cspan.End()
	started = true
	st.started.Store(true)
	verbose := os.Getenv("OFFSHORE_VERBOSE") != ""
	t1 := time.Now()
	rec.Slot = sstarted.Slot
	rec.Timings.Barrier = sstarted.WaitMs
	rec.Timings.Connect = max(0, events.Ms(t1.Sub(tc))-sstarted.WaitMs)
	fwd.serve(conn, prep.Token, sstarted.Session)
	if verbose {
		warn("running on %s (slot %s, prepare %s, worker wait %dms)", sstarted.Machine, sstarted.Slot, t1.Sub(t0).Round(time.Millisecond), sstarted.WaitMs)
		if len(rec.Ports) > 0 {
			warn("forwarding 127.0.0.1 ports %s", strings.Join(rec.Ports, ", "))
		}
	}
	_, xspan := tracing.Start(ctx, "proxy.exec")
	stdout, stderr := opt.stdout, opt.stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	exit, err := stream(pc, useTTY, opt.noTTY && !opt.stdin, stdout, stderr, st)
	code = exit.Code
	execAttrs(xspan, exit)
	tracing.End(xspan, err)
	// Forwarded connections end with the remote process.
	fwd.close()
	t2 := time.Now()
	rec.Timings.Run = events.Ms(t2.Sub(t1))
	if err != nil {
		var perr *proto.Error
		if !errors.As(err, &perr) {
			// No exit status: the connection broke. Whether the machine
			// is gone is probed only when a retry is otherwise possible.
			addr, fp := prep.Addr, prep.Fingerprint
			return code, &machineLost{machine: prep.Machine, err: err, probe: func() bool { return machineAnswers(addr, fp) }}
		}
		return code, fmt.Errorf("remote session: %w", err)
	}
	usage := exit.Usage
	usage.Cache = exit.Cache
	release.Usage = &usage
	rec.Usage, rec.Cache, rec.Warm = &usage, exit.Cache, usage.Warm
	st.writeback.Store(true)
	_, wspan := tracing.Start(ctx, "proxy.writeback")
	touched, err := receiveWriteback(pc, repo)
	wspan.SetAttributes(attribute.Int("offshore.roots_touched", len(touched)))
	tracing.End(wspan, err)
	if err != nil {
		return code, fmt.Errorf("write back: %w", err)
	}
	for _, r := range touched {
		daemon.Call(context.Background(), daemon.Request{Op: "sync", Root: r}, nil)
	}
	t3 := time.Now()
	rec.Timings.Writeback = events.Ms(t3.Sub(t2))
	_, ospan := tracing.Start(ctx, "proxy.outputs")
	err = receiveOutputs(pc, repo.Root)
	tracing.End(ospan, err)
	if err != nil {
		return code, fmt.Errorf("outputs: %w", err)
	}
	rec.Timings.Outputs = events.Ms(time.Since(t3))
	if err := pc.Write(proto.TDone, nil); err != nil {
		return code, err
	}
	if err := pc.Expect(proto.TDone, nil); err != nil {
		return code, err
	}
	if opt.after != nil {
		if err := opt.after(ctx, &session{conn: conn, token: prep.Token, repoID: prep.RepoID, root: repo.Root}, code); err != nil {
			return code, err
		}
	}
	if verbose {
		warn("exit %d (run %s, write back and outputs %s)", code, t2.Sub(t1).Round(time.Millisecond), time.Since(t2).Round(time.Millisecond))
		if line := FormatCompilerCache(exit.Cache); line != "" {
			warn("%s", line)
		}
	}
	return code, nil
}

// FormatCompilerCache renders the sccache counters of Exit.Cache for
// humans, e.g. "sccache: 3 hits / 1 misses (5 requests, 1 not cacheable)".
// It returns "" when the run did not use sccache.
func FormatCompilerCache(c map[string]int64) string {
	if _, ok := c["sccache_requests"]; !ok {
		return ""
	}
	s := fmt.Sprintf("sccache: %d hits / %d misses (%d requests", c["sccache_hits"], c["sccache_misses"], c["sccache_requests"])
	if n := c["sccache_not_cacheable"]; n > 0 {
		s += fmt.Sprintf(", %d not cacheable", n)
	}
	if n := c["sccache_errors"]; n > 0 {
		s += fmt.Sprintf(", %d errors", n)
	}
	return s + ")"
}

// stream forwards stdio, terminal and signals until the remote exits.
// noStdin closes the remote stdin right away.
func stream(pc *proto.Conn, useTTY, noStdin bool, stdout, stderr io.Writer, st *attemptState) (proto.Exit, error) {
	if useTTY {
		if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			defer term.Restore(int(os.Stdin.Fd()), old)
		}
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			sig := s.(syscall.Signal)
			if sig == syscall.SIGWINCH {
				if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					pc.Send(proto.TResize, proto.Resize{Rows: uint16(rows), Cols: uint16(cols)})
				}
				continue
			}
			st.interrupted.Store(true)
			pc.Send(proto.TSignal, proto.Signal{Sig: int(sig)})
		}
	}()
	if noStdin || !forwardStdin(useTTY) {
		pc.Write(proto.TStdinEOF, nil)
	} else {
		go copyStdin(pc, os.Stdin, st)
	}
	for {
		t, p, err := pc.Read()
		if err != nil {
			return proto.Exit{Code: 255}, err
		}
		switch t {
		case proto.TStdout:
			stdout.Write(p)
		case proto.TStderr:
			stderr.Write(p)
		case proto.TExit:
			var ex proto.Exit
			if err := json.Unmarshal(p, &ex); err != nil {
				return proto.Exit{Code: 255}, err
			}
			return ex, nil
		case proto.TError:
			return proto.Exit{Code: 255}, proto.Decode(t, p, proto.TExit, nil)
		}
	}
}

// forwardStdin decides whether local stdin is streamed to the remote
// process. Terminals and pipes are forwarded; regular files and /dev/null
// are not, so loops like `while read x; do cargo build; done < list` keep
// their input. OFFSHORE_STDIN=always|never overrides.
func forwardStdin(useTTY bool) bool {
	switch os.Getenv("OFFSHORE_STDIN") {
	case "always":
		return true
	case "never":
		return false
	}
	if useTTY {
		return true
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	m := fi.Mode()
	return m&os.ModeNamedPipe != 0 || (m&os.ModeCharDevice != 0 && term.IsTerminal(int(os.Stdin.Fd())))
}

func copyStdin(pc *proto.Conn, in *os.File, st *attemptState) {
	{
		buf := make([]byte, 32<<10)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				st.stdinUsed.Store(true)
				if pc.Write(proto.TStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				pc.Write(proto.TStdinEOF, nil)
				return
			}
		}
	}
}

// resolveSecret reads a secret reference locally. Secret values only travel
// inside the session request and are never written to the machine's disk.
func resolveSecret(ref, root string) (string, error) { return config.ResolveRef(ref, root) }

// Prewarm runs argv remotely in repo's root like a proxied command (image,
// machine lease and start, source sync, build slot), labelled
// purpose=prewarm, but never falls back to local execution: an error
// before the remote start is returned as is. progress receives the
// daemon's preparation messages.
func Prewarm(ctx context.Context, repo *config.Repo, argv []string, progress io.Writer) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("prewarm: empty command")
	}
	progressOut = progress
	l := labels.Merge(labels.Detect(repo.Root, "", os.Getenv), map[string]string{labels.Purpose: labels.PurposePrewarm})
	rec := &events.Run{Time: time.Now(), Repo: repo.Root, Cwd: repo.Root, Tool: argv[0], Args: argv[1:],
		Mode: events.Remote, Why: "offshore prewarm", Labels: labels.Clean(l)}
	code, err := runRemote(ctx, repo, config.Tool{}, argv[0], argv[1:], repo.Root, rec, remoteRun{})
	var fb *fallback
	if errors.As(err, &fb) {
		return 0, fb.err
	}
	return code, err
}

// ProtoCaches converts the repository caches for the worker.
func ProtoCaches(repo *config.Repo) []proto.Cache {
	out := make([]proto.Cache, 0, len(repo.Caches))
	for _, c := range repo.Caches {
		out = append(out, proto.Cache{
			Name: c.Name, Scope: c.Scope, Paths: c.Paths, Digest: c.Digest, Install: c.Install,
			Tool: c.Tool, MaxBytes: c.MaxBytes(), Prune: c.Prune, TTLSeconds: int64(c.TTLDuration().Seconds()),
		})
	}
	return out
}

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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/traumventures/cloudsourced/internal/client"
	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/transport"
	"golang.org/x/term"
)

// defaultEnv is always forwarded in addition to the tool's allowlist.
var defaultEnv = []string{"TERM", "COLORTERM", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "LANG", "LC_*"}

// Main runs tool with args and returns the exit code.
func Main(tool string, args []string) int {
	if os.Getenv("CLOUDSOURCED_ACTIVE") != "" || os.Getenv("CLOUDSOURCED_DISABLE") != "" {
		return execLocal(tool, args)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return execLocal(tool, args)
	}
	rootDir, err := config.FindRepo(cwd)
	if err != nil {
		return execLocal(tool, args)
	}
	repo, err := config.LoadRepo(rootDir)
	if err != nil {
		warn("invalid configuration, running locally: %v", err)
		return execLocal(tool, args)
	}
	profile, ok := repo.Tools[tool]
	if !ok || !remoteAllowed(profile, args) {
		return execLocal(tool, args)
	}
	code, err := runRemote(repo, profile, tool, args, cwd)
	if err != nil {
		var fb *fallback
		if errors.As(err, &fb) {
			if os.Getenv("CLOUDSOURCED_REQUIRE_REMOTE") != "" {
				warn("remote execution failed: %v", fb.err)
				return 125
			}
			warn("running locally: %v", fb.err)
			return execLocal(tool, args)
		}
		warn("%v", err)
		if code == 0 {
			code = 125
		}
	}
	return code
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cloudsourced: "+format+"\n", a...)
}

// fallback marks errors that happened before anything ran remotely.
type fallback struct{ err error }

func (f *fallback) Error() string { return f.err.Error() }

// subcommand returns the first non-flag argument (skipping "+toolchain").
func subcommand(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+") {
			continue
		}
		return a
	}
	return ""
}

func remoteAllowed(p config.Tool, args []string) bool {
	sub := subcommand(args)
	for _, l := range p.Local {
		if l == sub {
			return false
		}
	}
	if len(p.Remote) == 0 {
		return true
	}
	for _, r := range p.Remote {
		if r == sub || r == "*" {
			return true
		}
	}
	return false
}

// RealBinary finds tool in PATH, skipping cloudsourced shims.
func RealBinary(tool string) (string, error) {
	self, _ := os.Executable()
	selfReal, _ := filepath.EvalSymlinks(self)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, tool)
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if r, err := filepath.EvalSymlinks(cand); err == nil && r == selfReal {
			continue
		}
		return cand, nil
	}
	return "", fmt.Errorf("%s: command not found", tool)
}

func execLocal(tool string, args []string) int {
	bin, err := RealBinary(tool)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	err = syscall.Exec(bin, append([]string{tool}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "cloudsourced: exec %s: %v\n", bin, err)
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

func runRemote(repo *config.Repo, profile config.Tool, tool string, args []string, cwd string) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
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
	if err := client.EnsureDaemon(ctx); err != nil {
		return 0, &fallback{err}
	}
	raw, err := client.Call(ctx, client.Request{Op: "prepare", Root: repo.Root, Cwd: cwd, Tool: tool}, os.Stderr)
	signal.Stop(prepSig)
	close(prepSig)
	if err != nil {
		if ctx.Err() != nil {
			return 130, nil
		}
		return 0, &fallback{err}
	}
	var prep client.Prepared
	if err := json.Unmarshal(raw, &prep); err != nil {
		return 0, &fallback{err}
	}
	defer client.Call(context.Background(), client.Request{Op: "release", Lease: prep.Lease}, nil)

	useTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	req := proto.Exec{
		Repo: repo.Root, RepoID: prep.RepoID, Roots: prep.Roots,
		Argv: append([]string{tool}, args...), Cwd: cwd, Env: allowedEnv(profile.Env),
		CPU: repo.Resources.CPU, Memory: repo.MemoryBytes(),
		Build: repo.Cache.Build.Paths, BuildTTL: prep.BuildTTL, Deps: repo.Cache.Deps.Paths,
		Eager: profile.Eager, Lease: prep.Lease,
	}
	if useTTY {
		cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			cols, rows = 80, 24
		}
		req.TTY = &proto.TTY{Rows: uint16(rows), Cols: uint16(cols), Term: os.Getenv("TERM")}
	}
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := transport.Dial(dctx, prep.Addr, prep.Fingerprint)
	dcancel()
	if err != nil {
		return 0, &fallback{fmt.Errorf("connect to machine %s: %w", prep.Machine, err)}
	}
	defer conn.CloseWithError(0, "")
	st, pc, err := transport.OpenStream(ctx, conn, prep.Token, proto.KindExec)
	if err != nil {
		return 0, &fallback{err}
	}
	defer st.Close()
	if err := pc.Send(proto.TExec, req); err != nil {
		return 0, &fallback{err}
	}
	var started proto.Started
	if err := pc.Expect(proto.TStarted, &started); err != nil {
		return 0, &fallback{err}
	}
	verbose := os.Getenv("CLOUDSOURCED_VERBOSE") != ""
	t1 := time.Now()
	if verbose {
		warn("running on %s (slot %s, prepare %s, server wait %dms)", started.Machine, started.Slot, t1.Sub(t0).Round(time.Millisecond), started.WaitMs)
	}
	code, err := stream(pc, useTTY)
	t2 := time.Now()
	if err != nil {
		return code, fmt.Errorf("remote session: %w", err)
	}
	touched, err := receiveWriteback(pc, repo)
	if err != nil {
		return code, fmt.Errorf("write back: %w", err)
	}
	for _, r := range touched {
		client.Call(context.Background(), client.Request{Op: "sync", Root: r}, nil)
	}
	if err := receiveOutputs(pc, repo.Root); err != nil {
		return code, fmt.Errorf("outputs: %w", err)
	}
	if err := pc.Write(proto.TDone, nil); err != nil {
		return code, err
	}
	if err := pc.Expect(proto.TDone, nil); err != nil {
		return code, err
	}
	if verbose {
		warn("exit %d (run %s, write back and outputs %s)", code, t2.Sub(t1).Round(time.Millisecond), time.Since(t2).Round(time.Millisecond))
	}
	return code, nil
}

// stream forwards stdio, terminal and signals until the remote exits.
func stream(pc *proto.Conn, useTTY bool) (int, error) {
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
			pc.Send(proto.TSignal, proto.Signal{Sig: int(sig)})
		}
	}()
	if !forwardStdin(useTTY) {
		pc.Write(proto.TStdinEOF, nil)
	} else {
		go copyStdin(pc)
	}
	for {
		t, p, err := pc.Read()
		if err != nil {
			return 255, err
		}
		switch t {
		case proto.TStdout:
			os.Stdout.Write(p)
		case proto.TStderr:
			os.Stderr.Write(p)
		case proto.TExit:
			var ex proto.Exit
			if err := json.Unmarshal(p, &ex); err != nil {
				return 255, err
			}
			return ex.Code, nil
		case proto.TError:
			return 255, proto.Decode(t, p, proto.TExit, nil)
		}
	}
}

// forwardStdin decides whether local stdin is streamed to the remote
// process. Terminals and pipes are forwarded; regular files and /dev/null
// are not, so loops like `while read x; do cargo build; done < list` keep
// their input. CLOUDSOURCED_STDIN=always|never overrides.
func forwardStdin(useTTY bool) bool {
	switch os.Getenv("CLOUDSOURCED_STDIN") {
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

func copyStdin(pc *proto.Conn) {
	{
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
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

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/events"
	"golang.org/x/term"
)

// Test splitting: `cargo test` and `cargo nextest run` build remotely and
// run the tests on this machine, so tests use the local environment and
// local services. The remote phase builds the test binaries (cargo test:
// `cargo test --no-run --message-format=json-render-diagnostics`; nextest:
// `cargo nextest archive`), the artifacts transport makes them available
// locally, and the local phase runs them like cargo or nextest would.

// splitKind is the kind of a split test command.
type splitKind int

const (
	splitCargoTest splitKind = iota
	splitNextest
)

// splitPlan describes how a test command is split.
type splitPlan struct {
	kind    splitKind
	test    cargoTest
	nextest nextestRun
	// remoteWhy, when set, runs the whole command remotely and tells the
	// user why the tests do not run locally.
	remoteWhy string
}

// planSplit decides whether a command is split into a remote build and a
// local test run; nil runs it remotely as typed.
func planSplit(p config.Tool, tool string, args []string) *splitPlan {
	if tool != "cargo" {
		return nil
	}
	switch strings.ToLower(os.Getenv("CLOUDSOURCED_SPLIT")) {
	case "0", "off", "false", "no", "never":
		return nil
	}
	enabled := map[string]bool{}
	for _, s := range p.SplitSubcommands(tool) {
		enabled[s] = true
	}
	c := parseCargo(args)
	var plan *splitPlan
	switch c.Sub {
	case "test", "t":
		if !enabled[config.SplitCargoTest] {
			return nil
		}
		t := parseCargoTest(c.Global, c.Rest)
		if t.Unsplit != "" {
			return nil
		}
		plan = &splitPlan{kind: splitCargoTest, test: t}
	case "nextest":
		if !enabled[config.SplitCargoNextest] {
			return nil
		}
		n, ok := parseNextest(c.Global, c.Rest)
		if !ok || n.Unsplit != "" {
			return nil
		}
		plan = &splitPlan{kind: splitNextest, nextest: n}
	default:
		return nil
	}
	if runtime.GOOS != "linux" {
		plan.remoteWhy = fmt.Sprintf("this machine runs %s, the test binaries are built for Linux", runtime.GOOS)
	}
	return plan
}

// splitArtifacts is the transport of test binaries; see artifacts.
var splitArtifacts artifacts = fetchArtifacts{}

func runSplit(ctx context.Context, repo *config.Repo, profile config.Tool, tool string, args []string, cwd string, rec *events.Run, plan *splitPlan) (int, error) {
	if plan.remoteWhy == "" && len(repo.SlotPaths()) == 0 {
		plan.remoteWhy = "no slot-scope cache paths: test binaries cannot be transferred"
	}
	if plan.remoteWhy != "" {
		return runTestsRemotely(ctx, repo, profile, tool, args, cwd, rec, plan.remoteWhy)
	}
	switch plan.kind {
	case splitNextest:
		return splitNextestRun(ctx, repo, profile, args, cwd, rec, plan.nextest)
	default:
		return splitCargoTestRun(ctx, repo, profile, args, cwd, rec, plan.test)
	}
}

// runTestsRemotely runs the original command remotely after saying once
// why the tests do not run locally.
func runTestsRemotely(ctx context.Context, repo *config.Repo, profile config.Tool, tool string, args []string, cwd string, rec *events.Run, why string) (int, error) {
	warn("running the tests remotely: %s", why)
	r := *rec
	r.Why = "tests run remotely: " + why
	code, err := runRemote(ctx, repo, profile, tool, args, cwd, &r, remoteRun{})
	*rec = r
	return code, err
}

// buildEnv is the extra environment of a remote build phase: it runs
// without a terminal, so colors and the progress bar are requested
// explicitly when the local stderr is a terminal.
func buildEnv(patterns ...string) map[string]string {
	env := allowedEnv(append([]string{"CARGO_TERM_*"}, patterns...))
	if term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("NO_COLOR") == "" {
		if _, ok := env["CARGO_TERM_COLOR"]; !ok {
			env["CARGO_TERM_COLOR"] = "always"
		}
		if _, ok := env["CARGO_TERM_PROGRESS_WHEN"]; !ok {
			env["CARGO_TERM_PROGRESS_WHEN"] = "always"
			if cols, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil {
				env["CARGO_TERM_PROGRESS_WIDTH"] = fmt.Sprint(cols)
			}
		}
	}
	return env
}

// insideBuildPaths returns the path relative to root if abs lies inside
// one of the build directories.
func insideBuildPaths(root string, build []string, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	for _, b := range build {
		b = filepath.Clean(b)
		if strings.HasPrefix(rel, b+"/") {
			return rel, true
		}
	}
	return "", false
}

// --- nextest ---

func splitNextestRun(ctx context.Context, repo *config.Repo, profile config.Tool, args []string, cwd string, rec *events.Run, n nextestRun) (int, error) {
	if _, err := nextestBinary(); err != nil {
		return runTestsRemotely(ctx, repo, profile, "cargo", args, cwd, rec, "cargo-nextest is not installed locally")
	}
	h := fnv.New64a()
	h.Write([]byte(cwd))
	rel := filepath.Join(filepath.Clean(repo.SlotPaths()[0]), "cloudsourced", fmt.Sprintf("nextest-%016x.tar.zst", h.Sum64()))
	archive := filepath.Join(repo.Root, rel)
	fetched := false
	stderr := newLineFilter(os.Stderr, "Archiving ", "Archived ")
	code, err := runRemote(ctx, repo, profile, "cargo", n.archiveArgs(archive), cwd, rec, remoteRun{
		noTTY: true, stderr: stderr, env: buildEnv("NEXTEST_*"),
		after: func(ctx context.Context, s *session, code int) error {
			if code != 0 {
				return nil
			}
			if err := splitArtifacts.Fetch(ctx, s, []string{rel}); err != nil {
				return fmt.Errorf("fetch test archive: %w", err)
			}
			fetched = true
			return nil
		},
	})
	stderr.Flush()
	if err != nil || code != 0 {
		return code, err
	}
	if !fetched {
		return 125, fmt.Errorf("test archive %s missing", rel)
	}
	code = runLocalCommand(cwd, nil, "cargo", n.runArgs(archive), "Extracting ", "Extracted ")
	if code == nextestListFailed {
		return runTestsRemotely(ctx, repo, profile, "cargo", args, cwd, rec, "the test binaries do not run on this machine (nextest could not list the tests)")
	}
	return code, nil
}

// nextestListFailed is nextest's exit code when listing the tests of a
// binary fails, e.g. because it cannot be executed.
const nextestListFailed = 104

// nextestBinary finds cargo-nextest the way cargo does: PATH, then
// $CARGO_HOME/bin.
func nextestBinary() (string, error) {
	if p, err := exec.LookPath("cargo-nextest"); err == nil {
		return p, nil
	}
	home := os.Getenv("CARGO_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".cargo")
		}
	}
	p := filepath.Join(home, "bin", "cargo-nextest")
	if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
		return p, nil
	}
	return "", fmt.Errorf("cargo-nextest not found")
}

// runLocalCommand runs a local tool with the terminal attached and
// returns its exit code like a shell would. When stderr is not a terminal,
// lines starting with one of drop are removed from it.
func runLocalCommand(dir string, env []string, tool string, args []string, drop ...string) int {
	bin, err := RealBinary(tool)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	cmd := exec.Command(bin, args...)
	cmd.Args[0] = tool
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	var f *lineFilter
	if len(drop) > 0 && !term.IsTerminal(int(os.Stderr.Fd())) {
		f = newLineFilter(os.Stderr, drop...)
		cmd.Stderr = f
	}
	code := runChild(cmd)
	if f != nil {
		f.Flush()
	}
	return code
}

// runChild runs cmd in the foreground: terminal signals reach the child
// (same process group), the proxy ignores them while it waits. The exit
// code follows the shell convention (128+N for signal N).
func runChild(cmd *exec.Cmd) int {
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Reset(syscall.SIGINT, syscall.SIGQUIT)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "cloudsourced: %v\n", err)
	return 126
}

// --- output filtering ---

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// lineFilter writes through to w, dropping lines (terminated by \n or \r)
// whose text without ANSI escapes and leading blanks starts with one of
// the prefixes. Status lines of the build phase that a normal run would
// not print are removed this way.
type lineFilter struct {
	w    io.Writer
	drop []string
	buf  []byte
}

func newLineFilter(w io.Writer, drop ...string) *lineFilter { return &lineFilter{w: w, drop: drop} }

func (f *lineFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexAny(f.buf, "\r\n")
		if i < 0 {
			return len(p), nil
		}
		line := f.buf[:i+1]
		if !f.dropped(line) {
			if _, err := f.w.Write(line); err != nil {
				return len(p), err
			}
		}
		f.buf = f.buf[i+1:]
	}
}

func (f *lineFilter) dropped(line []byte) bool {
	s := strings.TrimLeft(ansiRE.ReplaceAllString(string(line), ""), " \t\r")
	for _, d := range f.drop {
		if strings.HasPrefix(s, d) {
			return true
		}
	}
	return false
}

// Flush writes a pending partial line.
func (f *lineFilter) Flush() {
	if len(f.buf) > 0 && !f.dropped(f.buf) {
		f.w.Write(f.buf)
	}
	f.buf = nil
}

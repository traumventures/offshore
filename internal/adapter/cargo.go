package adapter

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/traumventures/cloudsourced/internal/config"
	"golang.org/x/term"
)

// Cargo is the reference adapter. `cargo test` and `cargo nextest run`
// split into a remote build and a local test run, so tests use the local
// environment and local services: the remote phase builds the test
// binaries (cargo test: `cargo test --no-run
// --message-format=json-render-diagnostics`; nextest: `cargo nextest
// archive`), the runtime makes them available locally (fetch with zstd,
// or the output overlay), and the local phase runs them like cargo or
// nextest would. Everything else runs remotely as typed.
type Cargo struct{}

func (Cargo) Name() string    { return "cargo" }
func (Cargo) Tools() []string { return []string{"cargo"} }

func (Cargo) Detect(root string) bool { return fileExists(filepath.Join(root, "Cargo.toml")) }

// DefaultCaches: the target directory per build slot, the registry per
// machine (keyed by Cargo.lock) and sccache as global compiler cache.
func (Cargo) DefaultCaches(root string) []Cache {
	return []Cache{
		{config.Cache{Name: "target", Scope: ScopeSlot, Paths: []string{"target"}, Prune: config.PruneRust},
			"build directory: stays on the machine (24 h), never synchronised"},
		{config.Cache{Name: "deps", Scope: ScopeMachine, Paths: []string{"$CARGO_HOME/registry", "$CARGO_HOME/git"}},
			"downloaded crates, shared by all sessions of a machine"},
		{config.Cache{Name: "sccache", Scope: ScopeGlobal, Tool: config.CacheToolSccache},
			"compiler results shared across machines"},
	}
}

// cargoPhase is the phase kind of a cargo subcommand run as typed.
func cargoPhase(sub string) PhaseName {
	switch sub {
	case "fetch", "vendor", "generate-lockfile", "update":
		return Deps
	case "run", "r", "test", "t", "bench", "nextest":
		return Run
	}
	return Build
}

var (
	cargoTarget   = Artifact{Path: "target", Scope: ScopeSlot}
	cargoRegistry = Artifact{Path: "$CARGO_HOME/registry", Scope: ScopeMachine}
)

func (a Cargo) Classify(c Command) Plan {
	cc := parseCargo(c.Args)
	sub := cc.Sub
	plan := asTyped(c, sub, cargoPhase(sub), cargoTarget, cargoRegistry)
	if plan.Phases[0].Where == Local {
		return plan
	}
	switch sub {
	case "test", "t":
		if !splitEnabled(c, config.SplitCargoTest) {
			return plan
		}
		t := parseCargoTest(cc.Global, cc.Rest)
		if t.Unsplit != "" {
			plan.Why += "; tests run remotely as typed: " + t.Unsplit
			return plan
		}
		return a.splitPlan(c, plan, Plan{
			Phases: append([]Phase{
				{Name: Build, Where: Remote, Argv: append([]string{"cargo"}, t.buildArgs()...),
					Artifacts: []Artifact{cargoTarget, cargoRegistry, {Path: "target/**/deps/*-<hash> (test executables)", Scope: ScopeLocal}}},
				{Name: Run, Where: Local, Needs: []string{"test executables run here (platform check)"}},
			}, docPhase(t)...),
			Why:  "[tool.cargo] split: remote build, local test run",
			exec: func(ctx context.Context, rt Runtime) (int, error) { return runCargoTest(ctx, rt, c.Args, t) },
		})
	case "nextest":
		if !splitEnabled(c, config.SplitCargoNextest) {
			return plan
		}
		n, ok := parseNextest(cc.Global, cc.Rest)
		if !ok {
			return plan
		}
		if n.Unsplit != "" {
			plan.Why += "; tests run remotely as typed: " + n.Unsplit
			return plan
		}
		return a.splitPlan(c, plan, Plan{
			Phases: []Phase{
				{Name: Build, Where: Remote, Argv: append([]string{"cargo"}, n.archiveArgs("<archive>")...),
					Artifacts: []Artifact{cargoTarget, cargoRegistry, {Path: "target/cloudsourced/nextest-<cwd>.tar.zst", Scope: ScopeLocal}}},
				{Name: Run, Where: Local, Argv: append([]string{"cargo"}, n.runArgs("<archive>")...), Needs: []string{"cargo-nextest", "test executables run here"}},
			},
			Why:  "[tool.cargo] split: remote build, local test run",
			exec: func(ctx context.Context, rt Runtime) (int, error) { return runNextest(ctx, rt, c.Args, n) },
		})
	}
	return plan
}

// docPhase is the remote doc test run that follows a split cargo test.
func docPhase(t cargoTest) []Phase {
	if !t.DocTests {
		return nil
	}
	return []Phase{{Name: Run, Where: Remote, Argv: append([]string{"cargo"}, t.docArgs()...)}}
}

// splitPlan returns split unless this machine cannot run Linux test
// binaries at all; then the tests run remotely with one line saying why.
func (Cargo) splitPlan(c Command, remote, split Plan) Plan {
	if h := c.host(); h.OS != "linux" {
		return remotely(remote, fmt.Sprintf("this machine runs %s, the test binaries are built for Linux", h.OS))
	}
	return split
}

// remotely runs a plan's command as typed after telling the user why the
// tests do not run locally.
func remotely(p Plan, why string) Plan {
	p.Why = "tests run remotely: " + why
	p.exec = func(ctx context.Context, rt Runtime) (int, error) { return runTestsRemotely(ctx, rt, why) }
	return p
}

// runTestsRemotely runs the original command remotely after saying once
// why the tests do not run locally.
func runTestsRemotely(ctx context.Context, rt Runtime, why string) (int, error) {
	rt.Warn("running the tests remotely: %s", why)
	return rt.Remote(ctx, nil, RemoteOptions{Why: "tests run remotely: " + why})
}

// buildEnv is the extra environment of a remote build phase: it runs
// without a terminal, so colors and the progress bar are requested
// explicitly when the local stderr is a terminal.
func buildEnv(prefixes ...string) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		for _, p := range append([]string{"CARGO_TERM_"}, prefixes...) {
			if strings.HasPrefix(k, p) {
				env[k] = v
			}
		}
	}
	if colorTerminal() {
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

// --- nextest ---

// nextestMissing is cargo's message when the image has no cargo-nextest.
const nextestMissing = "no such command: `nextest`"

func runNextest(ctx context.Context, rt Runtime, args []string, n nextestRun) (int, error) {
	if _, err := nextestBinary(); err != nil {
		return runTestsRemotely(ctx, rt, "cargo-nextest is not installed locally")
	}
	dirs := rt.BuildDirs()
	if len(dirs) == 0 {
		return runTestsRemotely(ctx, rt, "no build directory ([cache.build] paths): the test archive cannot be transferred")
	}
	h := fnv.New64a()
	h.Write([]byte(rt.Cwd()))
	rel := filepath.Join(filepath.Clean(dirs[0]), "cloudsourced", fmt.Sprintf("nextest-%016x.tar.zst", h.Sum64()))
	archive := filepath.Join(rt.Root(), rel)
	filter := newLineFilter(os.Stderr, "Archiving ", "Archived ")
	sniff := newSniffer(filter, nextestMissing)
	code, err := rt.Remote(ctx, append([]string{"cargo"}, n.archiveArgs(archive)...), RemoteOptions{
		NoTTY: true, Stderr: sniff, Env: buildEnv("NEXTEST_"),
		Collect: func(code int) ([]string, error) {
			if code != 0 {
				return nil, nil
			}
			return []string{rel}, nil
		},
	})
	filter.Flush()
	if err != nil {
		return code, err
	}
	if code != 0 {
		if sniff.found() != "" {
			// The environment image lacks cargo-nextest: run as typed,
			// remotely, so the user sees exactly what cargo says there.
			return runTestsRemotely(ctx, rt, "cargo-nextest is not installed in the environment image (add it in [environment] setup)")
		}
		return code, nil
	}
	if !fileExists(archive) {
		return 125, fmt.Errorf("test archive %s missing", rel)
	}
	code = runLocalCommand(rt.Cwd(), nil, "cargo", n.runArgs(archive), "Extracting ", "Extracted ")
	if code == nextestListFailed {
		return runTestsRemotely(ctx, rt, "the test binaries do not run on this machine (nextest could not list the tests)")
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

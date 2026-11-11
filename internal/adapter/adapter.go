// Package adapter knows how proxied tools work. An adapter classifies a
// command line into phases (deps, build, run), each local or remote, names
// the artifacts a phase produces with their cache scope and what a local
// phase needs on this machine, and executes split plans (remote build,
// local run) through a Runtime the proxy provides. [tool.X] in the
// repository configuration stays the per-tool configuration; its adapter
// is chosen by `adapter = "..."` or the tool name.
package adapter

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/platform"
)

// PhaseName is the kind of work a phase does.
type PhaseName string

// Phases.
const (
	Deps  PhaseName = "deps"  // fetch or install dependencies
	Build PhaseName = "build" // compile
	Run   PhaseName = "run"   // run tests, scripts or programs
)

// Where a phase runs.
type Where string

// Locations.
const (
	Local  Where = "local"
	Remote Where = "remote"
)

// Cache scopes of artifacts (see [cache.<name>] scope).
const (
	ScopeSlot    = "slot"    // build slot: per repository, exclusive per session, <= 24 h
	ScopeMachine = "machine" // machine cache, keyed by a content digest
	ScopeGlobal  = "global"  // shared result cache across machines (sccache, GOCACHE)
	ScopeLocal   = "local"   // transferred to this machine for a local phase
)

// Artifact is something a phase produces.
type Artifact struct {
	Path  string // repository-relative path or glob; "$VAR/..." for tool homes
	Scope string // Scope*
}

// Phase is one step of a command.
type Phase struct {
	Name      PhaseName
	Where     Where
	Argv      []string   // the phase's command line; nil: the command as typed
	Artifacts []Artifact // what it produces for later phases
	Needs     []string   // what a local phase needs on this machine
}

func (p Phase) String() string {
	s := string(p.Name) + "@" + string(p.Where)
	if p.Argv != nil {
		s += " " + strings.Join(p.Argv, " ")
	}
	return s
}

// Plan is how a command runs.
type Plan struct {
	Adapter string
	Phases  []Phase
	// Why explains the decision (recorded with the run, `cloudsourced why`).
	Why string
	// exec runs a split plan; nil runs the command as typed, locally or
	// remotely as the phases say.
	exec func(ctx context.Context, rt Runtime) (int, error)
}

// Remote reports whether any phase runs remotely.
func (p Plan) Remote() bool {
	for _, ph := range p.Phases {
		if ph.Where == Remote {
			return true
		}
	}
	return false
}

// Split reports whether the plan mixes remote and local phases and needs
// Execute.
func (p Plan) Split() bool { return p.exec != nil }

// Execute runs a split plan.
func (p Plan) Execute(ctx context.Context, rt Runtime) (int, error) {
	if p.exec == nil {
		return rt.Remote(ctx, nil, RemoteOptions{})
	}
	return p.exec(ctx, rt)
}

// String renders the phases, e.g. "build@remote cargo test --no-run ... | run@local".
func (p Plan) String() string {
	parts := make([]string, len(p.Phases))
	for i, ph := range p.Phases {
		parts[i] = ph.String()
	}
	return strings.Join(parts, " | ")
}

// Command is a command line to classify.
type Command struct {
	Tool   string
	Args   []string
	Config config.Tool         // [tool.<Tool>]
	Getenv func(string) string // os.Getenv by default
	Host   *platform.Platform  // this machine; platform.Local() by default
}

func (c Command) getenv(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

func (c Command) host() platform.Platform {
	if c.Host != nil {
		return *c.Host
	}
	return platform.Local()
}

// Cache is a cache an adapter proposes for `cloudsourced init`
// ([cache.<Name>]; fields as in the repository configuration).
type Cache struct {
	Name    string
	Scope   string
	Paths   []string
	Digest  []string
	Install string
	Tool    string
	Comment string
}

// Adapter knows one tool ecosystem.
type Adapter interface {
	// Name is the adapter name used by [tool.X] adapter.
	Name() string
	// Tools are the command names the adapter handles by default.
	Tools() []string
	// Classify decides the phases of a command.
	Classify(c Command) Plan
	// DefaultCaches are the caches `cloudsourced init` proposes.
	DefaultCaches(root string) []Cache
	// Detect reports whether the repository at root uses the ecosystem.
	Detect(root string) bool
}

// All adapters in detection order (generic last).
var All = []Adapter{Cargo{}, Go{}, Node{}, Generic{}}

// ByName returns the adapter with this name.
func ByName(name string) (Adapter, bool) {
	for _, a := range All {
		if a.Name() == name {
			return a, true
		}
	}
	return nil, false
}

// For returns the adapter of a tool: [tool.X] adapter, else by tool name.
func For(tool string, cfg config.Tool) Adapter {
	if a, ok := ByName(cfg.AdapterName(tool)); ok {
		return a
	}
	return Generic{}
}

// Classify classifies a command with the adapter of its tool.
func Classify(c Command) Plan {
	p := For(c.Tool, c.Config).Classify(c)
	if p.Adapter == "" {
		p.Adapter = For(c.Tool, c.Config).Name()
	}
	return p
}

// Runtime executes phases for a split plan; the proxy implements it.
type Runtime interface {
	// Root is the repository root, Cwd the command's directory.
	Root() string
	Cwd() string
	// BuildDirs are the repository-relative build directories (slot
	// scope) whose files can be made available locally.
	BuildDirs() []string
	// Remote runs argv (nil: the command as typed) remotely and returns
	// its exit code. Errors before the remote start make the proxy fall
	// back to running the whole command locally.
	Remote(ctx context.Context, argv []string, o RemoteOptions) (int, error)
	// Warn prints one "cloudsourced: ..." line.
	Warn(format string, a ...any)
}

// RemoteOptions adjust one remote phase.
type RemoteOptions struct {
	Stdout, Stderr io.Writer         // instead of os.Stdout/os.Stderr
	NoTTY          bool              // never allocate a remote terminal
	Stdin          bool              // with NoTTY: still forward stdin
	Env            map[string]string // additional environment
	Why            string            // reason recorded with this phase's run ("" = the plan's)
	// Collect runs after the remote command exited with code, while the
	// session is held, and returns repository-relative files inside the
	// build directories that the next local phase needs. The runtime makes
	// them available at their local path (fetch, zstd-compressed, or the
	// output overlay with [outputs] mode = "fuse") before Remote returns.
	Collect func(code int) ([]string, error)
}

// remoteRules applies [tool.X] remote/local; the reason explains a local
// decision.
func remoteRules(p config.Tool, tool, sub string) (string, bool) {
	for _, l := range p.Local {
		if l == sub {
			return fmt.Sprintf("subcommand %q is listed in [tool.%s] local", sub, tool), false
		}
	}
	if len(p.Remote) == 0 {
		return "", true
	}
	for _, r := range p.Remote {
		if r == sub || r == "*" {
			return "", true
		}
	}
	return fmt.Sprintf("subcommand %q is not listed in [tool.%s] remote", sub, tool), false
}

// asTyped is the plan of a command that runs unchanged: remote unless
// [tool.X] local/remote say otherwise.
func asTyped(c Command, sub string, phase PhaseName, arts ...Artifact) Plan {
	if why, ok := remoteRules(c.Config, c.Tool, sub); !ok {
		return Plan{Phases: []Phase{{Name: phase, Where: Local}}, Why: why}
	}
	return Plan{Phases: []Phase{{Name: phase, Where: Remote, Artifacts: arts}}, Why: "[tool." + c.Tool + "] allows remote execution"}
}

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

// splitOff reports CLOUDSOURCED_SPLIT=0 (or off/false/no/never).
func splitOff(c Command) bool {
	switch strings.ToLower(c.getenv("CLOUDSOURCED_SPLIT")) {
	case "0", "off", "false", "no", "never":
		return true
	}
	return false
}

// splitEnabled reports whether sub is in the tool's split list.
func splitEnabled(c Command, sub string) bool {
	if splitOff(c) {
		return false
	}
	for _, s := range c.Config.SplitSubcommands(c.Tool) {
		if s == sub {
			return true
		}
	}
	return false
}

// RealBinary finds tool in PATH, skipping cloudsourced shims (links to
// this executable).
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

// insideDirs returns the path relative to root if abs lies inside one of
// the repository-relative directories.
func insideDirs(root string, dirs []string, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	for _, b := range dirs {
		b = filepath.Clean(b)
		if strings.HasPrefix(rel, b+"/") {
			return rel, true
		}
	}
	return "", false
}

// fileExists reports a regular file or directory at p.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

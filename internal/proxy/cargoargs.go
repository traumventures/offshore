package proxy

import (
	"fmt"
	"strings"
)

// This file parses the command lines of `cargo test` and `cargo nextest run`
// far enough to split them into a remote build and a local test run.
// Arguments the parser does not know stay where cargo or nextest sees them,
// or make the command run unsplit, so they report errors themselves.

// cargoGlobalValueFlags are cargo options before the subcommand that take a
// value as the next argument.
var cargoGlobalValueFlags = map[string]bool{"--color": true, "--config": true, "-C": true, "-Z": true}

// cargoCommand is a cargo command line split at its subcommand.
type cargoCommand struct {
	Global []string // +toolchain and global options before the subcommand
	Sub    string   // subcommand ("test", "nextest", ...)
	Rest   []string // arguments after the subcommand
}

func parseCargo(args []string) cargoCommand {
	var c cargoCommand
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "+") && i == 0:
			c.Global = append(c.Global, a)
		case a == "--":
			c.Rest = args[i:]
			return c
		case strings.HasPrefix(a, "-"):
			c.Global = append(c.Global, a)
			if cargoGlobalValueFlags[a] && i+1 < len(args) {
				i++
				c.Global = append(c.Global, args[i])
			}
		default:
			c.Sub = a
			c.Rest = args[i+1:]
			return c
		}
	}
	return c
}

// flagSpec describes the value of an option.
type flagSpec int

const (
	noValue       flagSpec = iota // --lib
	value                         // -p NAME, -pNAME, --package NAME, --package=NAME
	optionalValue                 // --timings, --timings=html (value only with =)
)

// option is one parsed option with its tokens as written.
type option struct {
	Name   string   // "--package" or "-p"
	Value  string   // the value, if any
	Tokens []string // the original tokens
}

// splitOption recognises the option at args[i]. spec maps long names and
// two-character short names to their value kind. It returns the option
// and the number of tokens consumed, or ok=false for unknown options.
func splitOption(args []string, i int, spec map[string]flagSpec) (option, int, bool) {
	a := args[i]
	if strings.HasPrefix(a, "--") {
		name, val, hasEq := strings.Cut(a, "=")
		kind, ok := spec[name]
		if !ok || (hasEq && kind == noValue) {
			return option{}, 0, false
		}
		switch {
		case hasEq:
			return option{Name: name, Value: val, Tokens: []string{a}}, 1, true
		case kind == value && i+1 < len(args):
			return option{Name: name, Value: args[i+1], Tokens: []string{a, args[i+1]}}, 2, true
		}
		return option{Name: name, Tokens: []string{a}}, 1, true
	}
	if len(a) < 2 || a[0] != '-' {
		return option{}, 0, false
	}
	name := a[:2]
	kind, ok := spec[name]
	if !ok {
		return option{}, 0, false
	}
	if kind == value {
		if len(a) > 2 {
			return option{Name: name, Value: strings.TrimPrefix(a[2:], "="), Tokens: []string{a}}, 1, true
		}
		if i+1 < len(args) {
			return option{Name: name, Value: args[i+1], Tokens: []string{a, args[i+1]}}, 2, true
		}
		return option{Name: name, Tokens: []string{a}}, 1, true
	}
	// Combined short flags such as -qv only when all take no value; the
	// returned option is the first one, callers inspect Tokens.
	for _, c := range a[2:] {
		if k, ok := spec["-"+string(c)]; !ok || k != noValue {
			return option{}, 0, false
		}
	}
	return option{Name: name, Tokens: []string{a}}, 1, true
}

// shortFlags returns the single-letter flags of a combined token like -qv.
func shortFlags(tok string) []string {
	if strings.HasPrefix(tok, "--") || !strings.HasPrefix(tok, "-") {
		return nil
	}
	var out []string
	for _, c := range tok[1:] {
		out = append(out, "-"+string(c))
	}
	return out
}

// cargoTestSpec lists the options of `cargo test`.
var cargoTestSpec = map[string]flagSpec{
	"-p": value, "--package": value, "--workspace": noValue, "--all": noValue, "--exclude": value,
	"--lib": noValue, "--bin": value, "--bins": noValue, "--example": value, "--examples": noValue,
	"--test": value, "--tests": noValue, "--bench": value, "--benches": noValue, "--all-targets": noValue,
	"--doc": noValue, "--no-run": noValue, "--no-fail-fast": noValue,
	"-F": value, "--features": value, "--all-features": noValue, "--no-default-features": noValue,
	"-j": value, "--jobs": value, "-r": noValue, "--release": noValue, "--profile": value,
	"--target": value, "--target-dir": value, "--artifact-dir": value, "--unit-graph": noValue,
	"--timings": optionalValue, "--manifest-path": value, "--lockfile-path": value,
	"--frozen": noValue, "--locked": noValue, "--offline": noValue,
	"--message-format": value, "--color": value, "--config": value, "-Z": value,
	"-q": noValue, "--quiet": noValue, "-v": noValue, "--verbose": noValue,
	"--ignore-rust-version": noValue, "--future-incompat-report": noValue, "--keep-going": noValue,
	"-h": noValue, "--help": noValue,
}

// cargoTargetSelection are the options of `cargo test` that select
// targets; without any of them cargo also runs doc tests.
var cargoTargetSelection = map[string]bool{
	"--lib": true, "--bin": true, "--bins": true, "--example": true, "--examples": true,
	"--test": true, "--tests": true, "--bench": true, "--benches": true, "--all-targets": true, "--doc": true,
}

// cargoTest is a parsed `cargo test` command line.
type cargoTest struct {
	Global   []string // before "test"
	Build    []string // build options: everything but the test name and the arguments after --
	TestName string   // optional positional filter, passed to every test binary
	HasName  bool
	Trailing []string // arguments after --, passed to every test binary
	Quiet    bool     // -q: binaries get --quiet, no status lines
	NoFail   bool     // --no-fail-fast
	Color    string   // --color value ("" = auto)
	Manifest string   // --manifest-path value
	DocTests bool     // no target selection: cargo also runs doc tests
	Unsplit  string   // why the command cannot be split ("" = it can)
}

func parseCargoTest(global, rest []string) cargoTest {
	t := cargoTest{Global: global, DocTests: true}
	for i := 0; i < len(global); i++ {
		g := global[i]
		for _, f := range append(shortFlags(g), g) {
			if f == "-q" || f == "--quiet" {
				t.Quiet = true
			}
		}
		switch {
		case g == "--color" && i+1 < len(global):
			t.Color = global[i+1]
		case strings.HasPrefix(g, "--color="):
			t.Color = strings.TrimPrefix(g, "--color=")
		}
	}
	for i := 0; i < len(rest); {
		a := rest[i]
		if a == "--" {
			t.Trailing = append(t.Trailing, rest[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if t.HasName {
				t.Unsplit = fmt.Sprintf("unexpected argument %q", a)
				t.Build = append(t.Build, a)
			} else {
				t.TestName, t.HasName = a, true
			}
			i++
			continue
		}
		o, n, ok := splitOption(rest, i, cargoTestSpec)
		if !ok {
			t.Unsplit = fmt.Sprintf("unknown option %q", a)
			t.Build = append(t.Build, a)
			i++
			continue
		}
		i += n
		names := []string{o.Name}
		if !strings.HasPrefix(o.Tokens[0], "--") && len(o.Tokens) == 1 && len(o.Tokens[0]) > 2 && cargoTestSpec[o.Name] == noValue {
			names = shortFlags(o.Tokens[0])
		}
		for _, name := range names {
			switch name {
			case "--no-run":
				t.Unsplit = "--no-run only builds"
			case "--doc":
				t.Unsplit = "doc tests are compiled while they run"
			case "--message-format":
				t.Unsplit = "--message-format is set"
			case "-h", "--help":
				t.Unsplit = "help"
			case "--unit-graph":
				t.Unsplit = "--unit-graph does not run tests"
			case "-q", "--quiet":
				t.Quiet = true
			case "--no-fail-fast":
				t.NoFail = true
			case "--color":
				t.Color = o.Value
			case "--manifest-path":
				t.Manifest = o.Value
			}
			if cargoTargetSelection[name] {
				t.DocTests = false
			}
		}
		t.Build = append(t.Build, o.Tokens...)
	}
	return t
}

// binaryArgs returns the arguments cargo passes to every test binary.
func (t cargoTest) binaryArgs() []string {
	var out []string
	if t.Quiet {
		out = append(out, "--quiet")
	}
	if t.HasName {
		out = append(out, t.TestName)
	}
	return append(out, t.Trailing...)
}

// buildArgs returns the remote build command line (without "cargo").
func (t cargoTest) buildArgs() []string {
	out := append([]string{}, t.Global...)
	out = append(out, "test", "--no-run", "--message-format=json-render-diagnostics")
	return append(out, t.Build...)
}

// docArgs returns the remote doc test command line (without "cargo").
func (t cargoTest) docArgs() []string {
	out := append([]string{}, t.Global...)
	out = append(out, "test", "--doc")
	out = append(out, t.Build...)
	if t.HasName {
		out = append(out, t.TestName)
	}
	if len(t.Trailing) > 0 {
		out = append(append(out, "--"), t.Trailing...)
	}
	return out
}

// Options of `cargo nextest run` by destination: cargo build options go to
// the remote archive, configuration options to both commands, reuse
// options make the command run unsplit. Everything else (filters, runner
// and reporter options) belongs to the local run.
var (
	nextestCargoSpec = map[string]flagSpec{
		"-p": value, "--package": value, "--workspace": noValue, "--exclude": value, "--all": noValue,
		"--lib": noValue, "--bin": value, "--bins": noValue, "--example": value, "--examples": noValue,
		"--test": value, "--tests": noValue, "--bench": value, "--benches": noValue, "--all-targets": noValue,
		"-F": value, "--features": value, "--all-features": noValue, "--no-default-features": noValue,
		"--build-jobs": value, "-r": noValue, "--release": noValue, "--cargo-profile": value,
		"--target": value, "--target-dir": value, "--unit-graph": noValue, "--timings": optionalValue,
		"--manifest-path": value, "--frozen": noValue, "--locked": noValue, "--offline": noValue,
		"--cargo-message-format": value, "--cargo-quiet": noValue, "--cargo-verbose": noValue,
		"--ignore-rust-version": noValue, "--future-incompat-report": noValue, "--config": value, "-Z": value,
	}
	nextestBothSpec = map[string]flagSpec{
		"--color": value, "--no-pager": noValue, "-v": noValue, "--verbose": noValue,
		"--config-file": value, "--user-config-file": value, "--tool-config-file": value,
		"--override-version-check": noValue, "-P": value, "--profile": value,
	}
	nextestUnsplitSpec = map[string]flagSpec{
		"--no-run": noValue, "--archive-file": value, "--archive-format": value, "--cargo-metadata": value,
		"--binaries-metadata": value, "--workspace-remap": value, "--target-dir-remap": value,
		"--build-dir-remap": value, "-h": noValue, "--help": noValue,
	}
)

// nextestRun is a parsed `cargo nextest run` command line.
type nextestRun struct {
	Global  []string // cargo options before "nextest"
	Pre     []string // nextest options before "run"
	Sub     string   // "run" or "r"
	Archive []string // options for the remote `cargo nextest archive`
	Run     []string // options, filters and -- arguments for the local run
	Unsplit string
}

// parseNextest parses the arguments after "nextest"; ok is false unless
// the nextest subcommand is run.
func parseNextest(global, rest []string) (n nextestRun, ok bool) {
	n.Global = global
	i := 0
	for ; i < len(rest) && strings.HasPrefix(rest[i], "-"); i++ {
		o, k, known := splitOption(rest, i, nextestBothSpec)
		if !known {
			n.Pre = append(n.Pre, rest[i])
			continue
		}
		n.Pre = append(n.Pre, o.Tokens...)
		i += k - 1
	}
	if i >= len(rest) || (rest[i] != "run" && rest[i] != "r") {
		return n, false
	}
	n.Sub = rest[i]
	rest = rest[i+1:]
	for i := 0; i < len(rest); {
		a := rest[i]
		if a == "--" {
			n.Run = append(n.Run, rest[i:]...)
			break
		}
		if o, k, ok := splitOption(rest, i, nextestUnsplitSpec); ok {
			n.Unsplit = o.Name + " is set"
			n.Run = append(n.Run, o.Tokens...)
			i += k
			continue
		}
		if o, k, ok := splitOption(rest, i, nextestCargoSpec); ok {
			n.Archive = append(n.Archive, o.Tokens...)
			i += k
			continue
		}
		if o, k, ok := splitOption(rest, i, nextestBothSpec); ok {
			n.Archive = append(n.Archive, o.Tokens...)
			n.Run = append(n.Run, o.Tokens...)
			i += k
			continue
		}
		n.Run = append(n.Run, a)
		i++
	}
	return n, true
}

// archiveArgs returns the remote archive command line (without "cargo").
func (n nextestRun) archiveArgs(file string) []string {
	out := append([]string{}, n.Global...)
	out = append(out, "nextest")
	out = append(out, n.Pre...)
	out = append(out, "archive", "--archive-file", file)
	return append(out, n.Archive...)
}

// runArgs returns the local run command line (without "cargo").
func (n nextestRun) runArgs(file string) []string {
	out := append([]string{}, n.Global...)
	out = append(out, "nextest")
	out = append(out, n.Pre...)
	out = append(out, n.Sub, "--archive-file", file)
	return append(out, n.Run...)
}

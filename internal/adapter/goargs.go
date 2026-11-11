package adapter

import (
	"fmt"
	"strings"
)

// This file parses `go test` command lines far enough to split them into
// a remote `go test -c` per package and a local run of the test binaries.
// Flags the parser does not know make the command run unsplit, so go
// reports errors itself.

// goFlagKind is how a go test flag is passed on.
type goFlagKind int

const (
	goBuild   goFlagKind = iota // to go list / go test -c
	goTest                      // to the test binary as -test.<name>
	goUnsplit                   // the command runs remotely as typed
)

// goFlag describes one flag of `go test`.
type goFlag struct {
	kind  goFlagKind
	value bool // takes a value
}

var goTestFlags = map[string]goFlag{
	// build flags
	"a": {goBuild, false}, "race": {goBuild, false}, "msan": {goBuild, false}, "asan": {goBuild, false},
	"trimpath": {goBuild, false}, "linkshared": {goBuild, false}, "x": {goBuild, false}, "work": {goBuild, false},
	"p": {goBuild, true}, "tags": {goBuild, true}, "ldflags": {goBuild, true}, "gcflags": {goBuild, true},
	"asmflags": {goBuild, true}, "mod": {goBuild, true}, "modfile": {goBuild, true}, "pkgdir": {goBuild, true},
	"toolexec": {goBuild, true}, "overlay": {goBuild, true}, "pgo": {goBuild, true}, "buildvcs": {goBuild, true},
	"buildmode": {goBuild, true}, "compiler": {goBuild, true}, "installsuffix": {goBuild, true}, "vet": {goBuild, true},
	"C": {goUnsplit, true},
	// test binary flags
	"run": {goTest, true}, "skip": {goTest, true}, "bench": {goTest, true}, "benchtime": {goTest, true},
	"count": {goTest, true}, "cpu": {goTest, true}, "parallel": {goTest, true}, "timeout": {goTest, true},
	"shuffle": {goTest, true}, "benchmem": {goTest, false}, "v": {goTest, false}, "short": {goTest, false},
	"failfast": {goTest, false}, "fullpath": {goTest, false},
	// flags whose output or behaviour the split cannot reproduce
	"json": {goUnsplit, false}, "c": {goUnsplit, false}, "o": {goUnsplit, true}, "exec": {goUnsplit, true},
	"i": {goUnsplit, false}, "n": {goUnsplit, false}, "list": {goUnsplit, true}, "fuzz": {goUnsplit, true},
	"cover": {goUnsplit, false}, "covermode": {goUnsplit, true}, "coverpkg": {goUnsplit, true},
	"coverprofile": {goUnsplit, true}, "cpuprofile": {goUnsplit, true}, "memprofile": {goUnsplit, true},
	"blockprofile": {goUnsplit, true}, "mutexprofile": {goUnsplit, true}, "trace": {goUnsplit, true},
	"outputdir": {goUnsplit, true}, "benchmemprofile": {goUnsplit, true}, "h": {goUnsplit, false}, "help": {goUnsplit, false},
}

// goTestCmd is a parsed `go test` command line.
type goTestCmd struct {
	Build    []string // flags for go list and go test -c
	Packages []string // package patterns ("." when none)
	Binary   []string // flags for every test binary (-test.*), then -args
	Verbose  bool
	Unsplit  string
}

// parseGoTest parses the arguments after "test".
func parseGoTest(rest []string) goTestCmd {
	var g goTestCmd
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "-args" || a == "--args" {
			g.Binary = append(g.Binary, rest[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			g.Packages = append(g.Packages, a)
			continue
		}
		name, val, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f, ok := goTestFlags[name]
		if !ok {
			if g.Unsplit == "" {
				g.Unsplit = fmt.Sprintf("unknown flag %q", a)
			}
			continue
		}
		if f.value && !hasEq {
			if i+1 >= len(rest) {
				g.Unsplit = fmt.Sprintf("flag %q needs a value", a)
				break
			}
			i++
			val, hasEq = rest[i], true
		}
		switch f.kind {
		case goUnsplit:
			if g.Unsplit == "" {
				g.Unsplit = "-" + name + " is set"
			}
		case goBuild:
			if hasEq {
				g.Build = append(g.Build, "-"+name+"="+val)
			} else {
				g.Build = append(g.Build, "-"+name)
			}
		case goTest:
			if name == "v" && (!hasEq || val == "true") {
				g.Verbose = true
			}
			arg := "-test." + name
			if hasEq {
				arg += "=" + val
			}
			// Binary flags keep their order before -args values.
			g.Binary = append(g.Binary[:g.binaryFlags()], append([]string{arg}, g.Binary[g.binaryFlags():]...)...)
		}
	}
	if len(g.Packages) == 0 {
		g.Packages = []string{"."}
	}
	return g
}

// binaryFlags counts the leading -test.* flags of Binary.
func (g goTestCmd) binaryFlags() int {
	n := 0
	for _, a := range g.Binary {
		if !strings.HasPrefix(a, "-test.") {
			break
		}
		n++
	}
	return n
}

// shellQuote quotes s for sh.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,+@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// buildScript is the remote phase: list the packages, then build one test
// binary per package with tests into out. It writes out/list with
// "importpath<TAB>dir<TAB>binary" lines (binary empty: no test files).
func (g goTestCmd) buildScript(out string) string {
	build := shellJoin(g.Build)
	return fmt.Sprintf(`set -e
out=%s
rm -rf "$out"; mkdir -p "$out"
go list %s -f '{{.ImportPath}}{{"\t"}}{{.Dir}}{{"\t"}}{{if or .TestGoFiles .XTestGoFiles}}t{{end}}' %s > "$out/list.raw"
n=0
while IFS='	' read -r imp dir t; do
  n=$((n+1))
  bin=""
  if [ -n "$t" ]; then
    bin="$out/$n.test"
    go test -c -o "$bin" %s "$imp"
  fi
  printf '%%s\t%%s\t%%s\n' "$imp" "$dir" "$bin" >> "$out/list"
done < "$out/list.raw"
touch "$out/list"
`, shellQuote(out), build, shellJoin(g.Packages), build)
}

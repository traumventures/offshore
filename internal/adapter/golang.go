package adapter

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
)

// Go runs go builds and tests remotely. `go test` is split: one
// `go test -c` per package remotely (results cached in the global go
// cache, GOCACHEPROG), the test binaries run locally in their package
// directories with go test's output (ok/FAIL/? lines, exit code 1).
type Go struct{}

func (Go) Name() string    { return "go" }
func (Go) Tools() []string { return []string{"go"} }

func (Go) Detect(root string) bool {
	return fileExists(filepath.Join(root, "go.mod")) || fileExists(filepath.Join(root, "go.work"))
}

// goOut is the slot-scope directory that carries go test binaries back.
const goOut = ".cloudsourced-build"

// DefaultCaches: the go build cache as global result cache, the module
// cache per machine, and a slot directory for test binaries.
func (Go) DefaultCaches(root string) []Cache {
	return []Cache{
		{config.Cache{Name: "go-build", Scope: ScopeGlobal, Tool: config.CacheToolGo}, "go build results shared across machines (GOCACHEPROG)"},
		{config.Cache{Name: "go-mod", Scope: ScopeMachine, Paths: []string{"$GOPATH/pkg/mod"}}, "downloaded modules (GOMODCACHE), shared by all sessions of a machine"},
		{config.Cache{Name: "go-out", Scope: ScopeSlot, Paths: []string{goOut}}, "test binaries of split `go test` runs (keep it out of git)"},
	}
}

func goPhase(sub string) PhaseName {
	switch sub {
	case "get", "mod":
		return Deps
	case "run", "test":
		return Run
	}
	return Build
}

var (
	goCacheArt = Artifact{Path: "$GOCACHE", Scope: ScopeGlobal}
	goModArt   = Artifact{Path: "$GOMODCACHE", Scope: ScopeMachine}
)

func (Go) Classify(c Command) Plan {
	sub := subcommand(c.Args)
	plan := asTyped(c, sub, goPhase(sub), goCacheArt, goModArt)
	if plan.Phases[0].Where == Local || sub != "test" || !splitEnabled(c, config.SplitGoTest) {
		return plan
	}
	i := 0
	for i < len(c.Args) && c.Args[i] != "test" {
		i++
	}
	g := parseGoTest(c.Args[i+1:])
	if g.Unsplit != "" {
		plan.Why += "; tests run remotely as typed: " + g.Unsplit
		return plan
	}
	if h := c.host(); h.OS != "linux" {
		return remotely(plan, fmt.Sprintf("this machine runs %s, the test binaries are built for Linux", h.OS))
	}
	return Plan{
		Phases: []Phase{
			{Name: Build, Where: Remote, Argv: []string{"sh", "-c", "go list ... && go test -c -o <out>/N.test <pkg> ..."},
				Artifacts: []Artifact{goCacheArt, goModArt, {Path: goOut + "/gotest-<cwd>/*.test", Scope: ScopeLocal}}},
			{Name: Run, Where: Local, Needs: []string{"test binaries run here (platform check)"}},
		},
		Why:  "[tool.go] split: remote build, local test run",
		exec: func(ctx context.Context, rt Runtime) (int, error) { return runGoTest(ctx, rt, g) },
	}
}

// goTestPkg is one package of a split go test.
type goTestPkg struct{ importPath, dir, binary string }

func parseGoList(r io.Reader) []goTestPkg {
	var out []goTestPkg
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 {
			continue
		}
		out = append(out, goTestPkg{f[0], f[1], f[2]})
	}
	return out
}

func runGoTest(ctx context.Context, rt Runtime, g goTestCmd) (int, error) {
	dirs := rt.BuildDirs()
	if len(dirs) == 0 {
		return runTestsRemotely(ctx, rt, "no slot-scope cache directory: test binaries cannot be transferred (add [cache.go-out] paths = [\""+goOut+"\"])")
	}
	h := fnv.New64a()
	h.Write([]byte(rt.Cwd()))
	rel := filepath.Join(filepath.Clean(dirs[0]), "cloudsourced", fmt.Sprintf("gotest-%016x", h.Sum64()))
	out := filepath.Join(rt.Root(), rel)
	code, err := rt.Remote(ctx, []string{"sh", "-c", g.buildScript(out)}, RemoteOptions{
		NoTTY: true,
		Collect: func(code int) ([]string, error) {
			if code != 0 {
				return nil, nil
			}
			return []string{rel + "/**"}, nil
		},
	})
	if err != nil || code != 0 {
		return code, err
	}
	b, err := os.ReadFile(filepath.Join(out, "list"))
	if err != nil {
		return 125, fmt.Errorf("go test: package list missing: %w", err)
	}
	pkgs := parseGoList(bytes.NewReader(b))
	var bins []string
	for _, p := range pkgs {
		if p.binary != "" {
			bins = append(bins, p.binary)
		}
	}
	if why := platformCheck(bins); why != "" {
		return runTestsRemotely(ctx, rt, why)
	}
	failed, interrupted := (&goRunner{verbose: g.Verbose, args: g.Binary, out: os.Stdout}).run(pkgs)
	switch {
	case interrupted:
		return 130, nil
	case failed:
		return 1, nil
	}
	return 0, nil
}

// goRunner runs go test binaries like go test.
type goRunner struct {
	verbose bool
	args    []string
	out     io.Writer
}

// run runs every package and prints go test's summary lines.
func (r *goRunner) run(pkgs []goTestPkg) (failed, interrupted bool) {
	for _, p := range pkgs {
		if p.binary == "" {
			fmt.Fprintf(r.out, "?   \t%s\t[no test files]\n", p.importPath)
			continue
		}
		w := &holdLast{w: r.out}
		cmd := exec.Command(p.binary, r.args...)
		cmd.Dir = p.dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, w, w
		start := time.Now()
		code := runChild(cmd)
		secs := time.Since(start).Seconds()
		if code == 0 {
			if r.verbose || w.last != "PASS\n" {
				w.flush()
			}
			fmt.Fprintf(r.out, "ok  \t%s\t%.3fs\n", p.importPath, secs)
			continue
		}
		w.flush()
		if w.last != "FAIL\n" {
			fmt.Fprintf(r.out, "exit status %d\n", code)
		}
		fmt.Fprintf(r.out, "FAIL\t%s\t%.3fs\n", p.importPath, secs)
		failed = true
		if code == 130 {
			interrupted = true
			break
		}
	}
	if failed {
		fmt.Fprintln(r.out, "FAIL")
	}
	return failed, interrupted
}

// holdLast writes through but holds back the last line until flush, so a
// final "PASS" can be dropped like go test does.
type holdLast struct {
	w    io.Writer
	buf  []byte
	last string
}

func (h *holdLast) Write(p []byte) (int, error) {
	h.buf = append(h.buf, p...)
	if i := bytes.LastIndexByte(h.buf[:max(0, len(h.buf)-1)], '\n'); i >= 0 {
		if _, err := h.w.Write(h.buf[:i+1]); err != nil {
			return 0, err
		}
		h.buf = append([]byte{}, h.buf[i+1:]...)
	}
	h.last = string(h.buf)
	return len(p), nil
}

func (h *holdLast) flush() {
	h.w.Write(h.buf)
	h.buf = nil
}

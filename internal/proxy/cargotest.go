package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/traumventures/cloudsourced/internal/platform"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/events"
	"golang.org/x/term"
)

// cargoArtifact is the part of cargo's compiler-artifact message used here.
type cargoArtifact struct {
	Reason       string `json:"reason"`
	PackageID    string `json:"package_id"`
	ManifestPath string `json:"manifest_path"`
	Target       struct {
		Kind    []string `json:"kind"`
		Name    string   `json:"name"`
		SrcPath string   `json:"src_path"`
		Doctest bool     `json:"doctest"`
	} `json:"target"`
	Profile struct {
		Test bool `json:"test"`
	} `json:"profile"`
	Executable *string `json:"executable"`
}

// testUnit is one test binary cargo test would run.
type testUnit struct {
	pkg      packageInfo
	kind     string // lib, bin, test, bench, example
	name     string
	srcPath  string
	exe      string
	manifest string
}

// packageInfo carries what cargo puts into a test's environment.
type packageInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	ID           string `json:"id"`
	ManifestPath string `json:"manifest_path"`
	Authors      []string
	Description  string `json:"description"`
	Homepage     string `json:"homepage"`
	Repository   string `json:"repository"`
	License      string `json:"license"`
	LicenseFile  string `json:"license_file"`
	RustVersion  string `json:"rust_version"`
	Readme       string `json:"readme"`
}

// testBuild is the result of the remote build phase of cargo test.
type testBuild struct {
	units    []testUnit
	bins     map[string]map[string]string // manifest path -> bin name -> executable
	exes     []string                     // every executable, absolute
	doctests bool                         // a selected package has a library with doc tests
}

// kindOrder sorts targets like cargo's TargetKind.
var kindOrder = map[string]int{"lib": 0, "bin": 1, "test": 2, "bench": 3, "example": 4}

func targetKind(kinds []string) string {
	if len(kinds) == 0 {
		return "lib"
	}
	switch k := kinds[0]; k {
	case "bin", "test", "bench", "example":
		return k
	}
	return "lib"
}

// parseTestBuild reads the JSON messages of `cargo test --no-run`. Lines
// that are not JSON messages are returned unchanged.
func parseTestBuild(r io.Reader, meta map[string]packageInfo) (testBuild, []byte) {
	b := testBuild{bins: map[string]map[string]string{}}
	docLibs := map[string]bool{} // manifests of libraries with doc tests
	var other bytes.Buffer
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var m cargoArtifact
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &m) != nil || m.Reason == "" {
			other.Write(line)
			other.WriteByte('\n')
			continue
		}
		if m.Reason != "compiler-artifact" {
			continue
		}
		kind := targetKind(m.Target.Kind)
		if kind == "lib" && m.Target.Doctest && !m.Profile.Test {
			docLibs[m.ManifestPath] = true
		}
		if m.Executable == nil {
			continue
		}
		exe := *m.Executable
		b.exes = append(b.exes, exe)
		if !m.Profile.Test {
			if kind == "bin" {
				if b.bins[m.ManifestPath] == nil {
					b.bins[m.ManifestPath] = map[string]string{}
				}
				b.bins[m.ManifestPath][m.Target.Name] = exe
			}
			continue
		}
		pkg, ok := meta[m.ManifestPath]
		if !ok {
			pkg = packageFromID(m.PackageID, m.ManifestPath)
		}
		b.units = append(b.units, testUnit{pkg: pkg, kind: kind, name: m.Target.Name, srcPath: m.Target.SrcPath, exe: exe, manifest: m.ManifestPath})
	}
	// Doc tests run for the selected packages (those with test units)
	// that have a library with doc tests; path dependencies outside the
	// selection have libraries too.
	for _, u := range b.units {
		if docLibs[u.manifest] {
			b.doctests = true
		}
	}
	sort.SliceStable(b.units, func(i, j int) bool {
		a, c := b.units[i], b.units[j]
		if a.pkg.Name != c.pkg.Name {
			return a.pkg.Name < c.pkg.Name
		}
		if a.pkg.Version != c.pkg.Version {
			return a.pkg.Version < c.pkg.Version
		}
		if a.pkg.ID != c.pkg.ID {
			return a.pkg.ID < c.pkg.ID
		}
		if kindOrder[a.kind] != kindOrder[c.kind] {
			return kindOrder[a.kind] < kindOrder[c.kind]
		}
		return a.name < c.name
	})
	return b, other.Bytes()
}

// packageFromID derives name and version from a package id such as
// "path+file:///w/a#0.1.0" or "path+file:///w/crates/x#name@0.1.0".
func packageFromID(id, manifest string) packageInfo {
	p := packageInfo{ID: id, ManifestPath: manifest}
	u, frag, _ := strings.Cut(id, "#")
	if name, ver, ok := strings.Cut(frag, "@"); ok {
		p.Name, p.Version = name, ver
	} else {
		p.Version = frag
		if pu, err := url.Parse(u); err == nil {
			p.Name = filepath.Base(pu.Path)
		}
	}
	return p
}

// cargoMetadata reads the workspace packages with the local cargo (no
// dependency resolution); nil when that is not possible.
func cargoMetadata(cwd string, t cargoTest) map[string]packageInfo {
	bin, err := RealBinary("cargo")
	if err != nil {
		return nil
	}
	args := []string{}
	if len(t.Global) > 0 && strings.HasPrefix(t.Global[0], "+") {
		args = append(args, t.Global[0])
	}
	args = append(args, "metadata", "--format-version", "1", "--no-deps", "--offline")
	if t.Manifest != "" {
		args = append(args, "--manifest-path", t.Manifest)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var md struct {
		Packages []packageInfo `json:"packages"`
	}
	if json.Unmarshal(out, &md) != nil {
		return nil
	}
	m := map[string]packageInfo{}
	for _, p := range md.Packages {
		m[p.ManifestPath] = p
	}
	return m
}

func splitCargoTestRun(ctx context.Context, repo *config.Repo, profile config.Tool, args []string, cwd string, rec *events.Run, t cargoTest) (int, error) {
	var stdout bytes.Buffer
	stderr := newLineFilter(os.Stderr, "Executable ")
	var build testBuild
	var outside string
	code, err := runRemote(ctx, repo, profile, "cargo", t.buildArgs(), cwd, rec, remoteRun{
		noTTY: true, stdout: &stdout, stderr: stderr, env: buildEnv(),
		after: func(ctx context.Context, s *session, code int) error {
			if code != 0 {
				return nil
			}
			var rest []byte
			build, rest = parseTestBuild(bytes.NewReader(stdout.Bytes()), nil)
			os.Stdout.Write(rest)
			var rels []string
			for _, exe := range build.exes {
				rel, ok := insideBuildPaths(repo.Root, repo.Cache.Build.Paths, exe)
				if !ok {
					outside = exe
					return nil
				}
				rels = append(rels, rel)
			}
			if len(rels) == 0 {
				return nil
			}
			if err := splitArtifacts.Fetch(ctx, s, rels); err != nil {
				return fmt.Errorf("fetch test binaries: %w", err)
			}
			return nil
		},
	})
	stderr.Flush()
	if err != nil || code != 0 {
		if code != 0 && err == nil {
			// Build errors: cargo already reported them.
			os.Stdout.Write(nonJSON(stdout.Bytes()))
		}
		return code, err
	}
	if outside != "" {
		return runTestsRemotely(ctx, repo, profile, "cargo", args, cwd, rec, fmt.Sprintf("test binary %s is outside the [cache.build] paths", outside))
	}
	if why := platform.CheckRun(build.exes); why != "" {
		return runTestsRemotely(ctx, repo, profile, "cargo", args, cwd, rec, why)
	}
	// Package metadata for the test environment (CARGO_PKG_*).
	if meta := cargoMetadata(cwd, t); meta != nil {
		for i, u := range build.units {
			if p, ok := meta[u.manifest]; ok {
				build.units[i].pkg = p
			}
		}
	}
	r := &testRunner{t: t, cwd: cwd, color: cargoColor(t.Color), current: currentManifest(cwd, t.Manifest), out: os.Stderr, stdout: os.Stdout}
	failed, interrupted := r.run(build)
	if interrupted {
		return 130, nil // Ctrl+C: cargo stops as well
	}
	if len(failed) > 0 && !t.NoFail {
		return 101, nil
	}
	docCode := 0
	if t.DocTests && build.doctests {
		drec := *rec
		docErr := newLineFilter(os.Stderr, "Finished ")
		var derr error
		docCode, derr = runRemote(ctx, repo, profile, "cargo", t.docArgs(), cwd, &drec, remoteRun{noTTY: true, stdin: true, stderr: docErr, env: buildEnv()})
		docErr.Flush()
		if derr != nil {
			return docCode, derr
		}
		if docCode != 0 && t.NoFail {
			failed = append(failed, r.packageArg(build.units, "")+"--doc")
		}
	}
	if len(failed) > 0 {
		r.summary(failed)
		return 101, nil
	}
	return docCode, nil
}

// nonJSON returns the lines of b that are not JSON messages.
func nonJSON(b []byte) []byte {
	_, rest := parseTestBuild(bytes.NewReader(b), nil)
	return rest
}

// cargoColor decides like cargo whether status lines are colored.
func cargoColor(flag string) bool {
	v := flag
	if v == "" {
		v = os.Getenv("CARGO_TERM_COLOR")
	}
	switch v {
	case "always":
		return true
	case "never":
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("TERM") != "dumb"
}

// currentManifest returns the package manifest cargo treats as current:
// --manifest-path, or the nearest Cargo.toml above cwd.
func currentManifest(cwd, manifest string) string {
	if manifest != "" {
		if !filepath.IsAbs(manifest) {
			manifest = filepath.Join(cwd, manifest)
		}
		return filepath.Clean(manifest)
	}
	for d := cwd; ; d = filepath.Dir(d) {
		p := filepath.Join(d, "Cargo.toml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// testRunner runs test binaries like cargo test.
type testRunner struct {
	t       cargoTest
	cwd     string
	color   bool
	current string    // manifest of the current package
	out     io.Writer // cargo's stderr: status lines, errors, test binaries' stderr
	stdout  io.Writer // test binaries' stdout
}

func (r *testRunner) status(verb, msg string) {
	if r.t.Quiet {
		return
	}
	if r.color {
		fmt.Fprintf(r.out, "\x1b[1m\x1b[92m%12s\x1b[0m %s\n", verb, msg)
	} else {
		fmt.Fprintf(r.out, "%12s %s\n", verb, msg)
	}
}

func (r *testRunner) errorf(format string, a ...any) {
	if r.color {
		fmt.Fprintf(r.out, "\x1b[1m\x1b[91merror\x1b[0m: "+format+"\n", a...)
	} else {
		fmt.Fprintf(r.out, "error: "+format+"\n", a...)
	}
}

// display shows a path relative to cwd when it lies below it.
func (r *testRunner) display(p string) string {
	if rel, err := filepath.Rel(r.cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// packageArg is "-p NAME " unless the unit belongs to the current package.
func (r *testRunner) packageArg(units []testUnit, manifest string) string {
	if manifest != "" && manifest == r.current {
		return ""
	}
	for _, u := range units {
		if u.manifest == manifest {
			return "-p " + u.pkg.Name + " "
		}
	}
	return ""
}

// rerunArg is what cargo suggests to rerun a failed unit.
func (r *testRunner) rerunArg(units []testUnit, u testUnit) string {
	p := r.packageArg(units, u.manifest)
	if u.kind == "lib" {
		return p + "--lib"
	}
	return p + "--" + u.kind + " " + u.name
}

// run runs the units in order and returns the rerun arguments of the
// failed ones; without --no-fail-fast it stops at the first failure.
// interrupted reports a test binary killed by SIGINT.
func (r *testRunner) run(b testBuild) (failed []string, interrupted bool) {
	args := r.t.binaryArgs()
	for _, u := range b.units {
		src := u.srcPath
		if rel, err := filepath.Rel(filepath.Dir(u.manifest), src); err == nil && !strings.HasPrefix(rel, "..") {
			src = rel
		}
		desc := fmt.Sprintf("%s (%s)", src, r.display(u.exe))
		if u.kind != "test" && u.kind != "bench" {
			desc = "unittests " + desc
		}
		r.status("Running", desc)
		cmd := exec.Command(u.exe, args...)
		cmd.Dir = filepath.Dir(u.manifest)
		cmd.Env = testEnv(u, b.bins[u.manifest])
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, r.stdout, r.out
		code := runChild(cmd)
		if code == 0 {
			continue
		}
		rerun := r.rerunArg(b.units, u)
		r.errorf("test failed, to rerun pass `%s`", rerun)
		if code > 128 {
			fmt.Fprintf(r.out, "\nCaused by:\n  process didn't exit successfully: `%s` (signal: %d, %s)\n",
				strings.Join(append([]string{u.exe}, args...), " "), code-128, syscall.Signal(code-128))
		}
		if code == 128+int(syscall.SIGINT) {
			return failed, true
		}
		failed = append(failed, rerun)
		if !r.t.NoFail {
			break
		}
	}
	return failed, false
}

// summary prints cargo's --no-fail-fast summary.
func (r *testRunner) summary(failed []string) {
	if len(failed) == 1 {
		r.errorf("1 target failed:\n    `%s`", failed[0])
		return
	}
	lines := make([]string, len(failed))
	for i, f := range failed {
		lines[i] = "    `" + f + "`"
	}
	r.errorf("%d targets failed:\n%s", len(failed), strings.Join(lines, "\n"))
}

// testEnv is the environment cargo gives a test binary.
func testEnv(u testUnit, bins map[string]string) []string {
	env := os.Environ()
	set := func(k, v string) { env = append(env, k+"="+v) }
	if c, err := RealBinary("cargo"); err == nil {
		set("CARGO", c)
	}
	p := u.pkg
	set("CARGO_MANIFEST_DIR", filepath.Dir(u.manifest))
	set("CARGO_MANIFEST_PATH", u.manifest)
	set("CARGO_PKG_NAME", p.Name)
	set("CARGO_PKG_VERSION", p.Version)
	core, pre, _ := strings.Cut(p.Version, "-")
	core, _, _ = strings.Cut(core, "+")
	parts := strings.SplitN(core, ".", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	set("CARGO_PKG_VERSION_MAJOR", parts[0])
	set("CARGO_PKG_VERSION_MINOR", parts[1])
	set("CARGO_PKG_VERSION_PATCH", parts[2])
	set("CARGO_PKG_VERSION_PRE", pre)
	set("CARGO_PKG_AUTHORS", strings.Join(p.Authors, ":"))
	set("CARGO_PKG_DESCRIPTION", p.Description)
	set("CARGO_PKG_HOMEPAGE", p.Homepage)
	set("CARGO_PKG_REPOSITORY", p.Repository)
	set("CARGO_PKG_LICENSE", p.License)
	set("CARGO_PKG_LICENSE_FILE", p.LicenseFile)
	set("CARGO_PKG_RUST_VERSION", p.RustVersion)
	readme := p.Readme
	if readme != "" && filepath.IsAbs(readme) {
		if rel, err := filepath.Rel(filepath.Dir(u.manifest), readme); err == nil {
			readme = rel
		}
	}
	set("CARGO_PKG_README", readme)
	if u.kind == "test" || u.kind == "bench" {
		for name, exe := range bins {
			set("CARGO_BIN_EXE_"+name, exe)
		}
	}
	// Dynamic libraries next to the binaries (dylib crates).
	deps := filepath.Dir(u.exe)
	lib := filepath.Dir(deps) + string(os.PathListSeparator) + deps
	if old := os.Getenv("LD_LIBRARY_PATH"); old != "" {
		lib += string(os.PathListSeparator) + old
	}
	set("LD_LIBRARY_PATH", lib)
	return env
}

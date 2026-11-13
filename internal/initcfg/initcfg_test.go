package initcfg

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/config"
)

// generate detects a fixture, writes the rendered configuration into a copy
// and loads it with the regular (strict) loader.
func generate(t *testing.T, fixture string) (*Detection, *config.Repo, string) {
	t.Helper()
	src := filepath.Join("testdata", fixture)
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	d, err := Detect(dst)
	if err != nil {
		t.Fatal(err)
	}
	out := d.Render()
	if err := os.WriteFile(filepath.Join(dst, config.RepoFileName), []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := config.LoadRepo(dst)
	if err != nil {
		t.Fatalf("generated configuration does not load: %v\n%s", err, out)
	}
	return d, repo, out
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	sort.Strings(s)
	return s
}

// The fixture models a production Rust workspace (CI workflow, setup
// action, toolchain file, git dependencies): the generated configuration
// must match its hand-written .offshore.toml in every detectable item.
func TestRustWorkspaceMatchesReference(t *testing.T) {
	d, got, out := generate(t, "rustws")
	ref := &config.Repo{}
	refDir := t.TempDir()
	data, _ := os.ReadFile("testdata/rustws/reference.offshore.toml")
	os.WriteFile(filepath.Join(refDir, config.RepoFileName), data, 0o644)
	ref, err := config.LoadRepo(refDir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Workspace != "app" {
		t.Errorf("workspace %q", d.Workspace)
	}
	check := func(what string, a, b any) {
		t.Helper()
		if !equal(a, b) {
			t.Errorf("%s: got %v, reference %v", what, a, b)
		}
	}
	check("image", got.Environment.Image, ref.Environment.Image)
	check("linker", got.Environment.Linker, ref.Environment.Linker)
	check("toolchain", got.Environment.Toolchains.Rust, ref.Environment.Toolchains.Rust)
	check("apt", sorted(got.Environment.Packages.Apt), sorted(ref.Environment.Packages.Apt))
	check("cpu", got.Resources.CPU, ref.Resources.CPU)
	check("caches", got.Caches, ref.Caches)
	check("secrets", got.Secrets, ref.Secrets)
	check("cargo local", got.Tools["cargo"].Local, ref.Tools["cargo"].Local)
	check("git fetch env", got.Environment.Env["CARGO_NET_GIT_FETCH_WITH_CLI"], "true")
	pg, rpg := got.Services["postgres"], ref.Services["postgres"]
	check("postgres image", pg.Image, rpg.Image)
	check("postgres port", pg.Port, rpg.Port)
	check("postgres command", pg.Command, rpg.Command)
	if len(got.Environment.Setup) != len(ref.Environment.Setup) {
		t.Fatalf("setup steps: got %d, reference %d\n%s", len(got.Environment.Setup), len(ref.Environment.Setup), out)
	}
	for i, s := range ref.Environment.Setup {
		if norm(got.Environment.Setup[i].Run) != norm(s.Run) {
			t.Errorf("setup[%d]:\n got %q\n ref %q", i, norm(got.Environment.Setup[i].Run), norm(s.Run))
		}
	}
	env := got.Tools["cargo"].Env
	for _, want := range []string{"RUSTFLAGS", "CARGO_TERM_*", "RUST_LOG", "RUST_BACKTRACE", "APP_TEST_*", "NEXTEST_*"} {
		if !slices.Contains(env, want) {
			t.Errorf("cargo env %v lacks %s", env, want)
		}
	}
	for _, want := range []string{"example-org/errkit", "example-org/sandbox-rs", "sudo not added", "cargo-deny", "# Typst for the report template tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func equal(a, b any) bool {
	switch x := a.(type) {
	case []string:
		return slices.Equal(x, b.([]string))
	case map[string]string:
		y := b.(map[string]string)
		if len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if y[k] != v {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// norm makes shell scripts comparable: whitespace and line continuations
// collapse, rm -f equals rm.
func norm(s string) string {
	s = strings.ReplaceAll(s, "\\\n", " ")
	s = strings.ReplaceAll(s, "rm -f ", "rm ")
	f := strings.Fields(s)
	// The reference removes the archive after the version check is done.
	return strings.Join(f, " ")
}

func TestDockerfileFallback(t *testing.T) {
	d, repo, _ := generate(t, "docker")
	if d.Workspace != "" || repo.Environment.Toolchains.Rust != "rust-toolchain" {
		t.Errorf("workspace %q toolchain %q", d.Workspace, repo.Environment.Toolchains.Rust)
	}
	if repo.Environment.Image != "rust:1-slim-bookworm" {
		t.Errorf("image %q", repo.Environment.Image)
	}
	if !slices.Equal(sorted(repo.Environment.Packages.Apt), []string{"libpq-dev", "protobuf-compiler"}) {
		t.Errorf("apt %v", repo.Environment.Packages.Apt)
	}
	if repo.SlotPaths()[0] != "target" || len(repo.Secrets) != 0 {
		t.Errorf("cache %v secrets %v", repo.SlotPaths(), repo.Secrets)
	}
}

func TestWorkflowServices(t *testing.T) {
	_, repo, out := generate(t, "services")
	pg := repo.Services["postgres"]
	if pg.Image != "postgres:16" || pg.Port != 5432 || pg.Env["POSTGRES_PASSWORD"] != "secret" || pg.Env["POSTGRES_USER"] != "" {
		t.Errorf("postgres %+v", pg)
	}
	if r := repo.Services["redis"]; r.Image != "redis:7" || r.Port != 6379 {
		t.Errorf("redis %+v", r)
	}
	if repo.Resources.CPU != 3 { // 4 cores * 0.625, rounded
		t.Errorf("cpu %v (4 cores)", repo.Resources.CPU)
	}
	if !slices.Equal(repo.Environment.Packages.Apt, []string{"libsqlite3-dev", "curl", "ca-certificates"}) {
		t.Errorf("apt %v", repo.Environment.Packages.Apt)
	}
	if repo.Environment.Env["RUSTFLAGS"] != "-Dwarnings" || repo.Environment.Env["CARGO_TERM_COLOR"] != "" {
		t.Errorf("env %v", repo.Environment.Env)
	}
	if len(repo.Environment.Setup) != 1 || !strings.Contains(repo.Environment.Setup[0].Run, "nexte.st") {
		t.Errorf("setup %v", repo.Environment.Setup)
	}
	if env := repo.Tools["cargo"].Env; !slices.Contains(env, "DATABASE_URL") || !slices.Contains(env, "NEXTEST_*") {
		t.Errorf("cargo env %v", env)
	}
	if strings.Contains(out, "npm") {
		t.Errorf("non-Rust job leaked into the configuration:\n%s", out)
	}
}

func TestNonRustRepository(t *testing.T) {
	d, repo, _ := generate(t, "plain")
	if d.Rust || repo.Environment.Image != "debian:trixie-slim" || len(repo.Tools) != 0 {
		t.Errorf("rust %v image %q tools %v", d.Rust, repo.Environment.Image, repo.Tools)
	}
}

func TestAptPackages(t *testing.T) {
	got := aptPackages(`apt-get update -qq && apt-get install -y -qq --no-install-recommends cmake libheif-dev/trixie-security "${extra[@]}" foo=1.2 >/dev/null`)
	if !slices.Equal(got, []string{"cmake", "libheif-dev", "foo"}) {
		t.Errorf("%v", got)
	}
}

func TestBaseImage(t *testing.T) {
	for in, want := range map[string]string{
		"rust:1.98.1-trixie":      "rust:1-slim-trixie",
		"rust:1.85-slim-bookworm": "rust:1-slim-bookworm",
		"rust:1.85":               "rust:1-slim",
		"rust:latest":             "rust:latest",
		"debian:trixie":           "debian:trixie",
	} {
		if got := baseImage(in, true); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if baseImage("rust:1.98.1-trixie", false) != "rust:1.98.1-trixie" {
		t.Error("without a toolchain file the pinned image stays")
	}
}

func TestEnvPatterns(t *testing.T) {
	got := envPatterns([]string{"APP_TEST_PROFILE", "APP_TEST_PG_BIN", "DATABASE_URL", "FOO_BAR_BAZ"})
	if !slices.Equal(got, []string{"APP_TEST_*", "DATABASE_URL", "FOO_BAR_BAZ"}) {
		t.Errorf("%v", got)
	}
}

func TestGoAndBunRepository(t *testing.T) {
	d, repo, out := generate(t, "gobun")
	if strings.Join(d.Adapters, ",") != "go,node" {
		t.Fatalf("adapters %v", d.Adapters)
	}
	if repo.Environment.Image != "golang:1.26-trixie" {
		t.Errorf("image %q", repo.Environment.Image)
	}
	byName := map[string]config.Cache{}
	for _, c := range repo.Caches {
		byName[c.Name] = c
	}
	if c := byName["go-build"]; c.Scope != config.ScopeGlobal || c.Tool != config.CacheToolGo {
		t.Errorf("go-build %+v", c)
	}
	if c := byName["go-mod"]; c.Scope != config.ScopeMachine {
		t.Errorf("go-mod %+v", c)
	}
	if c := byName["node-modules"]; c.Scope != config.ScopeMachine || c.Install != "bun install --frozen-lockfile" || len(c.Digest) != 2 {
		t.Errorf("node-modules %+v", c)
	}
	if _, ok := repo.Tools["go"]; !ok {
		t.Errorf("no [tool.go]\n%s", out)
	}
	if _, ok := repo.Tools["bun"]; !ok {
		t.Errorf("no [tool.bun]\n%s", out)
	}
}

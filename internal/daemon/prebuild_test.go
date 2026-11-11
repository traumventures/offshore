package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/envbuild"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/pool"
)

// prebuildPool records prebuilds and unreachable reports.
type prebuildPool struct {
	pool.Pool
	mu          sync.Mutex
	built       []string
	fail        error
	unreachable []string
	done        chan string
}

func (p *prebuildPool) Prebuild(_ context.Context, env pool.Env, _ io.Writer) error {
	p.mu.Lock()
	p.built = append(p.built, env.Hash)
	err := p.fail
	p.mu.Unlock()
	if p.done != nil {
		p.done <- env.Hash
	}
	return err
}

func (p *prebuildPool) EnvState(context.Context, string, string) (pool.EnvState, error) {
	return pool.EnvState{Known: true, Missing: []string{"aws (linux/amd64)"}}, nil
}

func (p *prebuildPool) Unreachable(_ context.Context, lease, machine string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreachable = append(p.unreachable, lease+"/"+machine)
	return nil
}

func (p *prebuildPool) builds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.built...)
}

func testDaemon(p pool.Pool) *Daemon {
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), pool: p, roots: map[string]*root{}, conns: map[string]*machineConn{}}
	d.metrics = newDaemonMetrics(d)
	d.env = newEnvWatch(d)
	return d
}

func writeRepo(t *testing.T, dir, apt string) {
	t.Helper()
	cfg := "version = 1\n[environment]\nimage = \"debian:trixie\"\n[environment.toolchains]\nrust = \"rust-toolchain.toml\"\n[environment.packages]\napt = [\"" + apt + "\"]\n"
	if err := os.WriteFile(filepath.Join(dir, config.RepoFileName), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resolve(t *testing.T, dir string) (*config.Repo, *envbuild.Plan) {
	t.Helper()
	r, err := config.LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := envbuild.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, p
}

func waitBuild(t *testing.T, p *prebuildPool) string {
	t.Helper()
	select {
	case h := <-p.done:
		return h
	case <-time.After(10 * time.Second):
		t.Fatal("no prebuild")
	}
	return ""
}

func TestPrebuildOnEnvironmentChange(t *testing.T) {
	old := prebuildDebounce
	prebuildDebounce = 10 * time.Millisecond
	defer func() { prebuildDebounce = old }()
	dir := t.TempDir()
	writeRepo(t, dir, "cmake")
	os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"1.90\"\n"), 0o644)
	p := &prebuildPool{done: make(chan string, 10)}
	d := testDaemon(p)
	repo, plan := resolve(t, dir)

	// The first request records the environment; it builds the image itself.
	d.env.observe(repo, plan, "run")
	if len(p.builds()) != 0 {
		t.Fatal("first sight of an environment must not prebuild")
	}
	// Unrelated files do not trigger anything.
	d.env.changed(repo.Root, []string{"src/main.rs"})
	time.Sleep(50 * time.Millisecond)
	if len(p.builds()) != 0 {
		t.Fatal("source change triggered a prebuild")
	}
	// A configuration change builds the new environment in the background.
	writeRepo(t, dir, "clang")
	_, plan2 := resolve(t, dir)
	d.env.changed(repo.Root, []string{config.RepoFileName})
	if h := waitBuild(t, p); h != plan2.Hash {
		t.Fatalf("built %s, want %s", h, plan2.Hash)
	}
	// The toolchain file is an input too.
	os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"1.91\"\n"), 0o644)
	_, plan3 := resolve(t, dir)
	d.env.changed(repo.Root, []string{"rust-toolchain.toml"})
	if h := waitBuild(t, p); h != plan3.Hash {
		t.Fatalf("built %s, want %s", h, plan3.Hash)
	}
	// The same environment at the next request: no second build.
	r3, p3 := resolve(t, dir)
	d.env.observe(r3, p3, "run")
	// Back to an earlier, already built environment: not rebuilt either.
	writeRepo(t, dir, "clang")
	os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("[toolchain]\nchannel = \"1.90\"\n"), 0o644)
	r2, pl2 := resolve(t, dir)
	d.env.observe(r2, pl2, "run")
	d.env.running.Wait()
	if n := len(p.builds()); n != 2 {
		t.Fatalf("%d builds, want 2: %v", n, p.builds())
	}
	st, err := d.env.status(context.Background(), dir)
	if err != nil || st.Hash != plan2.Hash || st.Prebuild == nil || st.Prebuild.Running || st.Pool == nil || len(st.Pool.Missing) != 1 {
		t.Fatalf("status %+v %v", st, err)
	}
}

// A change detected at the next request (no file watcher event) also
// starts the build; a failed build is retried at the next change.
func TestPrebuildAtRequestAndRetryAfterFailure(t *testing.T) {
	dir := t.TempDir()
	writeRepo(t, dir, "cmake")
	os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("stable\n"), 0o644)
	p := &prebuildPool{done: make(chan string, 10), fail: errors.New("docker build failed")}
	d := testDaemon(p)
	r1, pl1 := resolve(t, dir)
	d.env.observe(r1, pl1, "run")
	writeRepo(t, dir, "clang")
	r2, pl2 := resolve(t, dir)
	d.env.observe(r2, pl2, "run of cargo")
	waitBuild(t, p)
	d.env.running.Wait()
	st, _ := d.env.status(context.Background(), dir)
	if st.Prebuild == nil || st.Prebuild.Error == "" || st.Prebuild.Trigger != "run of cargo" {
		t.Fatalf("status %+v", st.Prebuild)
	}
	// `offshore env build` retries a failed build.
	p.mu.Lock()
	p.fail = nil
	p.mu.Unlock()
	if _, err := d.dispatch(context.Background(), Request{Op: "env-build", Root: dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if h := waitBuild(t, p); h != pl2.Hash {
		t.Fatal("wrong hash")
	}
	d.env.running.Wait()
	// --wait runs synchronously.
	res, err := d.dispatch(context.Background(), Request{Op: "env-build", Root: dir, Wait: true}, io.Discard)
	<-p.done
	if err != nil || res.(*Prebuild).Running || res.(*Prebuild).Error != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

// The file watcher of a tracked root feeds environment changes.
func TestPrebuildFromFileWatcher(t *testing.T) {
	old := prebuildDebounce
	prebuildDebounce = 10 * time.Millisecond
	defer func() { prebuildDebounce = old }()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	writeRepo(t, dir, "cmake")
	os.WriteFile(filepath.Join(dir, "rust-toolchain.toml"), []byte("stable\n"), 0o644)
	p := &prebuildPool{done: make(chan string, 10)}
	d := testDaemon(p)
	repo, plan := resolve(t, dir)
	d.env.observe(repo, plan, "run")
	r, err := d.rootFor(dir, manifest.Rules{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	// A different size: mtimes are coarse, a same-size rewrite within
	// a few milliseconds would look unchanged to the scanner.
	writeRepo(t, dir, "clang-19")
	_, plan2 := resolve(t, dir)
	if h := waitBuild(t, p); h != plan2.Hash {
		t.Fatalf("built %s, want %s", h, plan2.Hash)
	}
}

func TestUnreachableOp(t *testing.T) {
	p := &prebuildPool{}
	d := testDaemon(p)
	if _, err := d.dispatch(context.Background(), Request{Op: "unreachable", Lease: "l1", Machine: "m1"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(p.unreachable) != 1 || p.unreachable[0] != "l1/m1" {
		t.Fatalf("%v", p.unreachable)
	}
}

func TestEnvInputs(t *testing.T) {
	r := &config.Repo{Environment: config.Environment{Dockerfile: "docker/build.Dockerfile",
		Toolchains: config.Toolchains{Rust: "app/rust-toolchain.toml"},
		Setup:      []config.SetupStep{{Run: "x", Inputs: []string{"scripts/setup.sh", "tools"}}}}}
	in := envInputs(r)
	for path, want := range map[string]bool{
		".offshore.toml": true, ".offshore.local.toml": true, "app/rust-toolchain.toml": true,
		"docker/other": true, "scripts/setup.sh": true, "tools/a/b": true, "toolsx": false, "app/src/lib.rs": false,
	} {
		if relevant(in, path) != want {
			t.Errorf("%s: %v", path, !want)
		}
	}
}

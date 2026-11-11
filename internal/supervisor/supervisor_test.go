package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/traumventures/offshore/internal/clock"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/machine"
	"github.com/traumventures/offshore/internal/provider"
)

type fakePlatform struct {
	mu       sync.Mutex
	secrets  map[string]string
	fails    int
	fetches  int
	deleted  bool
	prepared int
	stops    int
	cacheDir string
}

func (p *fakePlatform) FetchSecrets(context.Context) (map[string]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fetches++
	if p.fails > 0 {
		p.fails--
		return nil, errors.New("not yet")
	}
	if p.deleted {
		return nil, errors.New("parameter not found")
	}
	return p.secrets, nil
}
func (p *fakePlatform) DeleteSecrets(context.Context) error { p.deleted = true; return nil }
func (p *fakePlatform) Prepare(context.Context, Runner) error {
	p.prepared++
	return nil
}
func (p *fakePlatform) StopSelf(context.Context) error { p.stops++; return nil }

// fakeDocker records commands and keeps containers, images and volumes.
type fakeDocker struct {
	mu         sync.Mutex
	cmds       []string
	envs       [][]string
	containers map[string]bool
	images     map[string]bool
	gateway    string
}

func (d *fakeDocker) run(_ context.Context, env []string, _ []byte, args ...string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cmds = append(d.cmds, strings.Join(args, " "))
	d.envs = append(d.envs, env)
	switch args[0] {
	case "container":
		if !d.containers[args[2]] {
			return nil, errors.New("no such container")
		}
	case "image":
		if !d.images[args[2]] {
			return nil, errors.New("no such image")
		}
	case "pull":
		d.images[args[len(args)-1]] = true
	case "network":
		return []byte(d.gateway + " "), nil
	case "create":
		d.containers[args[2]] = true
	}
	return nil, nil
}

func testAgent(t *testing.T) (*Supervisor, *fakePlatform, *fakeDocker) {
	dir := t.TempDir()
	p := &fakePlatform{secrets: map[string]string{"OFFSHORE_TOKEN": "tok"}, fails: 2}
	d := &fakeDocker{containers: map[string]bool{}, images: map[string]bool{}}
	a := &Supervisor{
		Spec: Spec{Provider: "fake", ExpiresAt: time.Now().Add(time.Hour).Unix(), IdleStop: 120, Layout: machine.Layout{
			ID: "m1", Image: "img", Binary: "/bin/cs", Publish: "7443:7443/udp", CacheVolume: "cs-cache", DepsVolume: "cs-deps",
			StatusDir: filepath.Join(dir, "status"), SecretKeys: []string{"OFFSHORE_TOKEN"},
			Services: []provider.Service{{Name: "db", Image: "pg"}},
		}},
		Platform: p, StateDir: filepath.Join(dir, "state"), Docker: d.run,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep: func(time.Duration) {},
	}
	return a, p, d
}

func TestFirstBootCreatesAndStartsInOrder(t *testing.T) {
	a, p, d := testAgent(t)
	ctx := context.Background()
	if err := a.EnsureContainers(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.deleted || p.fetches != 3 || p.prepared != 1 {
		t.Fatalf("platform %+v", p)
	}
	var creates, starts []string
	for i, c := range d.cmds {
		f := strings.Fields(c)
		switch f[0] {
		case "create":
			creates = append(creates, f[2])
			if f[2] == "cs-m1" && !slices.Equal(d.envs[i], []string{"OFFSHORE_TOKEN=tok"}) {
				t.Fatalf("machine created without secret env: %v", d.envs[i])
			}
			if strings.Contains(c, "tok") {
				t.Fatalf("secret in args: %s", c)
			}
		case "start":
			starts = append(starts, f[1])
		}
	}
	if !slices.Equal(creates, []string{"cs-m1", "cs-m1-svc-db"}) || !slices.Equal(starts, []string{"cs-m1", "cs-m1-svc-db"}) {
		t.Fatalf("creates %v starts %v", creates, starts)
	}
	b, err := os.ReadFile(a.secretPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(a.secretPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode %v", fi.Mode())
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil || m["OFFSHORE_TOKEN"] != "tok" {
		t.Fatalf("persisted %s", b)
	}
}

func TestRebootStartsWithoutRecreating(t *testing.T) {
	a, p, d := testAgent(t)
	ctx := context.Background()
	if err := a.EnsureContainers(ctx); err != nil {
		t.Fatal(err)
	}
	d.cmds = nil
	if err := a.EnsureContainers(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range d.cmds {
		if strings.HasPrefix(c, "create") || strings.HasPrefix(c, "pull") {
			t.Fatalf("reboot ran %q", c)
		}
	}
	// Lost containers are recreated from the persisted secrets even though
	// the provider's channel is gone.
	d.containers = map[string]bool{}
	if err := a.EnsureContainers(ctx); err != nil || p.fetches != 3 {
		t.Fatalf("recreate: err=%v fetches=%d", err, p.fetches)
	}
}

func TestCheckActs(t *testing.T) {
	a, p, dk := testAgent(t)
	clk := clock.NewFake(time.Now())
	a.Clock = clk
	now := clk.Now()
	shutdowns := 0
	a.Shutdown = func() error { shutdowns++; return nil }
	os.MkdirAll(a.Spec.Layout.StatusDir, 0o755)
	status := filepath.Join(a.Spec.Layout.StatusDir, "status.json")
	ctx := context.Background()

	if d := a.Check(ctx); d != machine.Keep {
		t.Fatalf("no status: %v", d)
	}
	machine.WriteHostStatus(status, machine.HostStatus{UpdatedAt: now.Unix(), Ready: true,
		IdleSince: now.Add(-time.Hour).Unix(), LastProbe: now.Add(-11 * time.Minute).Unix()})
	if d := a.Check(ctx); d != machine.IdleStop || p.stops != 1 {
		t.Fatalf("idle: %v stops=%d", d, p.stops)
	}
	// The worker container is stopped first (it saves its slots on SIGTERM).
	if last := dk.cmds[len(dk.cmds)-1]; last != "stop -t 120 cs-m1" {
		t.Fatalf("container not stopped before the instance: %q", last)
	}
	a.Spec.ExpiresAt = now.Unix()
	if d := a.Check(ctx); d != machine.Terminate || shutdowns != 1 {
		t.Fatalf("expired: %v shutdowns=%d", d, shutdowns)
	}
}

func TestSpecRoundTrip(t *testing.T) {
	a, _, _ := testAgent(t)
	p := filepath.Join(t.TempDir(), "machine.json")
	b, _ := json.Marshal(a.Spec)
	os.WriteFile(p, b, 0o600)
	s, err := LoadSpec(p)
	if err != nil || s.Layout.ID != "m1" || s.IdleStopAfter() != 2*time.Minute || len(s.Layout.Services) != 1 {
		t.Fatalf("spec %+v %v", s, err)
	}
}

type terminatingPlatform struct {
	*fakePlatform
	terminations int
	err          error
}

func (p *terminatingPlatform) TerminateSelf(context.Context) error { p.terminations++; return p.err }

func TestCheckTerminatesSelfAtExpiry(t *testing.T) {
	a, p, _ := testAgent(t)
	tp := &terminatingPlatform{fakePlatform: p}
	a.Platform = tp
	shutdowns := 0
	a.Shutdown = func() error { shutdowns++; return nil }
	a.Spec.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	if d := a.Check(context.Background()); d != machine.Terminate || tp.terminations != 1 || shutdowns != 0 {
		t.Fatalf("expired: %v terminations=%d shutdowns=%d", d, tp.terminations, shutdowns)
	}
	tp.err = errors.New("denied")
	if d := a.Check(context.Background()); d != machine.Terminate || tp.terminations != 2 || shutdowns != 1 {
		t.Fatalf("fallback: %v terminations=%d shutdowns=%d", d, tp.terminations, shutdowns)
	}
}

package static

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/supervisor"
)

// fakeHost runs the host side in-process with fake docker and systemctl.
type fakeHost struct {
	h        *supervisor.StaticHost
	uploaded bool
	active   bool
	running  bool
	cmds     []string
}

type fakeTransport struct {
	hosts map[string]*fakeHost
	down  map[string]bool
}

func newFakeHost(t *testing.T) *fakeHost {
	dir := t.TempDir()
	f := &fakeHost{}
	f.h = &supervisor.StaticHost{
		SpecPath: filepath.Join(dir, "etc", "machine.json"), StateDir: filepath.Join(dir, "state"), UnitDir: dir, Binary: "/bin/cs",
		Docker: func(_ context.Context, _ []string, _ []byte, args ...string) ([]byte, error) {
			f.cmds = append(f.cmds, "docker "+strings.Join(args, " "))
			switch args[0] {
			case "inspect":
				if f.running {
					return []byte("true\n"), nil
				}
				return nil, errors.New("no such container")
			case "stop", "rm":
				f.running = false
			}
			return nil, nil
		},
		Systemctl: func(_ context.Context, args ...string) error {
			f.cmds = append(f.cmds, "systemctl "+strings.Join(args, " "))
			switch args[0] {
			case "enable", "start":
				f.active, f.running = true, true
			case "stop", "disable":
				f.active = false
			case "is-active":
				if !f.active {
					return errors.New("inactive")
				}
			}
			return nil
		},
	}
	return f
}

func (t *fakeTransport) Run(ctx context.Context, h Host, cmd string, stdin []byte) ([]byte, error) {
	if t.down[h.Name] {
		return nil, errors.New("connection refused")
	}
	f := t.hosts[h.Name]
	switch {
	case strings.HasPrefix(cmd, "test -x "):
		if !f.uploaded {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	case strings.HasPrefix(cmd, "mkdir -p /var/lib/cloudsourced/bin && cat > "):
		f.uploaded = len(stdin) > 0
		return nil, nil
	}
	_, op, ok := strings.Cut(cmd, " supervisor static ")
	if !ok || !f.uploaded {
		return nil, errors.New("unexpected command " + cmd)
	}
	switch op {
	case "apply":
		var p supervisor.StaticPayload
		if err := json.Unmarshal(stdin, &p); err != nil {
			return nil, err
		}
		return nil, f.h.Apply(ctx, p)
	case "start":
		return nil, f.h.Start(ctx)
	case "stop":
		return nil, f.h.Stop(ctx)
	case "destroy":
		return nil, f.h.Destroy(ctx)
	case "status":
		return json.Marshal(f.h.Status(ctx))
	}
	return nil, errors.New("unknown op " + op)
}

func testProvider(t *testing.T) (*Static, *fakeTransport) {
	bin := filepath.Join(t.TempDir(), "cloudsourced")
	os.WriteFile(bin, []byte("binary"), 0o755)
	ft := &fakeTransport{hosts: map[string]*fakeHost{"small": newFakeHost(t), "big": newFakeHost(t)}, down: map[string]bool{}}
	s := &Static{Hosts: []Host{
		{Name: "small", SSH: "root@10.0.0.1", Addr: "10.0.0.1:7443", CPU: 4, Memory: 8 << 30, Arch: "amd64"},
		{Name: "big", SSH: "root@10.0.0.2", Addr: "10.0.0.2:7443", CPU: 32, Memory: 128 << 30, Arch: "amd64"},
	}, Transport: ft, Binary: bin, Registry: "registry.local/envs", hasBin: map[string]bool{}}
	return s, ft
}

func TestStaticLifecycle(t *testing.T) {
	ctx := context.Background()
	s, ft := testProvider(t)
	spec := provider.MachineSpec{ID: "m1", Owner: "pool", Image: "registry.local/envs:abc-amd64", EnvHash: "abc", Size: "big",
		CPU: 16, Memory: 64 << 30, ExpiresAt: time.Now().Add(time.Hour), Finger: "fp",
		Env: map[string]string{"CLOUDSOURCED_MACHINE_ID": "m1"}, SecretEnv: map[string]string{"CLOUDSOURCED_TLS_SEED": "secret"}}
	m, err := s.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != provider.Starting || m.Labels["cloudsourced.host"] != "big" {
		t.Fatalf("created %+v (needs the big host)", m)
	}
	big := ft.hosts["big"]
	if !big.uploaded || !big.active {
		t.Fatal("binary not uploaded or supervisor not started")
	}
	// The public spec has no secret values; the secret channel has them.
	b, _ := os.ReadFile(big.h.SpecPath)
	if bytes.Contains(b, []byte(`"secret"`)) || !bytes.Contains(b, []byte(`"provider":"static"`)) {
		t.Fatalf("spec %s", b)
	}
	sec, _ := os.ReadFile(filepath.Join(big.h.StateDir, "static-secrets.json"))
	if !bytes.Contains(sec, []byte("secret")) {
		t.Fatal("secrets not delivered")
	}
	ms, err := s.List(ctx, "pool")
	if err != nil || len(ms) != 1 || ms[0].State != provider.Running || ms[0].Addr != "10.0.0.2:7443" || ms[0].CPU != 32 {
		t.Fatalf("list %+v %v", ms, err)
	}
	if ms, _ := s.List(ctx, "other"); len(ms) != 0 {
		t.Fatal("other pool sees the machine")
	}
	// The host is taken: a second machine of that size does not fit.
	spec2 := spec
	spec2.ID = "m2"
	if _, err := s.Create(ctx, spec2); err == nil {
		t.Fatal("second machine placed on a busy or too small host")
	}
	if err := s.Stop(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.List(ctx, "pool"); ms[0].State != provider.Stopped || ms[0].Addr != "" {
		t.Fatalf("stopped %+v", ms)
	}
	if m, err := s.Start(ctx, "m1"); err != nil || m.State != provider.Running {
		t.Fatalf("start %+v %v", m, err)
	}
	if err := s.Destroy(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.List(ctx, "pool"); len(ms) != 0 {
		t.Fatalf("destroyed machine listed %+v", ms)
	}
	if _, err := os.Stat(big.h.SpecPath); err == nil {
		t.Fatal("spec left on the host")
	}
	if !strings.Contains(strings.Join(big.cmds, "\n"), "docker volume rm -f cs-cache-m1") {
		t.Fatalf("cache volume not removed: %v", big.cmds)
	}
	// Destroying twice is fine; a free host takes a new machine.
	if err := s.Destroy(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	// An unreachable host is skipped, not fatal.
	ft.down["small"] = true
	if ms, err := s.List(ctx, "pool"); err != nil || len(ms) != 1 {
		t.Fatalf("list with a host down: %v %v", ms, err)
	}
}

func TestParseHosts(t *testing.T) {
	hs, err := parseHosts([]any{map[string]any{"ssh": "admin@build1:2222", "cpu": int64(8), "memory": "32G"}})
	if err != nil || hs[0].Addr != "build1:7443" || hs[0].Name != "admin@build1:2222" || hs[0].Memory != 32<<30 || hs[0].Arch != "amd64" {
		t.Fatalf("%+v %v", hs, err)
	}
	for _, bad := range []any{nil, []any{map[string]any{"cpu": int64(1), "memory": "1G"}}, []any{map[string]any{"ssh": "x"}},
		[]any{map[string]any{"ssh": "a", "name": "n", "cpu": int64(1), "memory": "1G"}, map[string]any{"ssh": "b", "name": "n", "cpu": int64(1), "memory": "1G"}}} {
		if _, err := parseHosts(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := open(map[string]any{"hosts": []any{}}); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("registry not required: %v", err)
	}
}

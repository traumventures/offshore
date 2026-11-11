package machine

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/provider"
)

func testLayout() Layout {
	return Layout{
		ID: "m1", Image: "img:1", Binary: "/host/offshore", Publish: "127.0.0.1::7443/udp",
		CacheVolume: "cache-m1", DepsVolume: "deps-x", CPU: 2, Memory: 1 << 30, Swap: true,
		Labels:     map[string]string{"b": "2", "a": "1"},
		SvcLabels:  map[string]string{"kind": "service"},
		Env:        map[string]string{"OFFSHORE_MACHINE_ID": "m1", "OFFSHORE_CPU": "2"},
		SecretKeys: []string{"OFFSHORE_TOKEN", "OFFSHORE_KEY"},
		Services:   []provider.Service{{Name: "db", Image: "postgres:16", Env: map[string]string{"Z": "1", "A": "2"}, Command: []string{"postgres", "-c", "x=1"}}},
	}
}

func TestMachineContainerArgs(t *testing.T) {
	l := testLayout()
	secret := map[string]string{"OFFSHORE_TOKEN": "tok", "OFFSHORE_KEY": "pem\nkey"}
	cs := l.Containers(secret, SecretsInline)
	if len(cs) != 2 || cs[0].Name != "cs-m1" || cs[1].Name != "cs-m1-svc-db" {
		t.Fatalf("containers %+v", cs)
	}
	want := []string{"create", "--name", "cs-m1", "--hostname", "cs-m1",
		"--label", "a=1", "--label", "b=2",
		"--device", "/dev/fuse", "--cap-add", "SYS_ADMIN", "--security-opt", "apparmor=unconfined",
		"--cgroupns", "private", "-p", "127.0.0.1::7443/udp",
		"-v", "/host/offshore:/opt/offshore/bin/offshore:ro",
		"-v", "cache-m1:/cache", "-v", "deps-x:/deps",
		"--entrypoint", "/opt/offshore/bin/offshore",
		"-e", "OFFSHORE_CPU=2", "-e", "OFFSHORE_MACHINE_ID=m1",
		"-e", "OFFSHORE_KEY=pem\nkey", "-e", "OFFSHORE_TOKEN=tok",
		"--cpus", "2", "--memory", "1073741824", "--memory-swap", "-1",
		"img:1", "worker"}
	if !reflect.DeepEqual(cs[0].Create, want) {
		t.Fatalf("machine args\n got %q\nwant %q", cs[0].Create, want)
	}
	wantSvc := []string{"create", "--name", "cs-m1-svc-db", "--network", "container:cs-m1",
		"--label", "kind=service", "-e", "A=2", "-e", "Z=1", "postgres:16", "postgres", "-c", "x=1"}
	if !reflect.DeepEqual(cs[1].Create, wantSvc) {
		t.Fatalf("service args\n got %q\nwant %q", cs[1].Create, wantSvc)
	}
	if got := l.StartOrder(); !reflect.DeepEqual(got, []string{"cs-m1", "cs-m1-svc-db"}) {
		t.Fatalf("start order %v", got)
	}
}

func TestSecretsFromProcessEnvKeepValuesOutOfArgs(t *testing.T) {
	l := testLayout()
	l.CPU, l.Memory = 0, 0
	l.StatusDir = "/run/cs"
	secret := map[string]string{"OFFSHORE_TOKEN": "tok", "OFFSHORE_KEY": "pem"}
	args := l.Containers(secret, SecretsFromProcessEnv)[0].Create
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "tok") || strings.Contains(joined, "pem") {
		t.Fatalf("secret value in args: %q", args)
	}
	for _, want := range []string{"-e OFFSHORE_TOKEN ", "-e OFFSHORE_KEY ", "-v /run/cs:" + HostStatusDir,
		"-e " + HostStatusEnv + "=" + HostStatusFile} {
		if !strings.Contains(joined+" ", want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	// No limits on a whole-VM machine; swap without a memory limit is moot.
	if slices.Contains(args, "--cpus") || slices.Contains(args, "--memory") || slices.Contains(args, "--memory-swap") {
		t.Fatalf("unexpected limits: %q", args)
	}
	if env := l.SecretEnv(secret); !reflect.DeepEqual(env, []string{"OFFSHORE_KEY=pem", "OFFSHORE_TOKEN=tok"}) {
		t.Fatalf("secret env %v", env)
	}
}

func TestDecide(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	idle := func(idleFor, probeAgo time.Duration) *HostStatus {
		return &HostStatus{UpdatedAt: now.Unix(), Ready: true, IdleSince: ago(idleFor), LastProbe: ago(probeAgo)}
	}
	future := now.Add(time.Hour)
	cases := []struct {
		name     string
		expires  time.Time
		idleStop time.Duration
		st       *HostStatus
		want     Decision
	}{
		{"no status", future, 2 * time.Minute, nil, Keep},
		{"expired without status", now, 2 * time.Minute, nil, Terminate},
		{"expired idle", now.Add(-time.Second), 0, idle(time.Hour, 0), Terminate},
		{"expired busy within grace", now.Add(-time.Minute), 0, &HostStatus{UpdatedAt: now.Unix(), Sessions: 1}, Keep},
		{"expired busy after grace", now.Add(-ExpiryGrace), 0, &HostStatus{UpdatedAt: now.Unix(), Sessions: 1}, Terminate},
		{"idle stop disabled", future, 0, idle(time.Hour, time.Hour), Keep},
		{"idle and unobserved", future, 2 * time.Minute, idle(11*time.Minute, 11*time.Minute), IdleStop},
		{"idle but probed recently", future, 2 * time.Minute, idle(time.Hour, 9*time.Minute), Keep},
		{"unobserved window is idle stop when longer", future, 30 * time.Minute, idle(31*time.Minute, 20*time.Minute), Keep},
		{"unobserved longer than idle stop", future, 30 * time.Minute, idle(31*time.Minute, 31*time.Minute), IdleStop},
		{"not idle long enough", future, 20 * time.Minute, idle(10*time.Minute, time.Hour), Keep},
		{"busy", future, 2 * time.Minute, &HostStatus{UpdatedAt: now.Unix(), Ready: true, Sessions: 2, LastProbe: ago(time.Hour)}, Keep},
		{"not ready (provisioning)", future, 2 * time.Minute, &HostStatus{UpdatedAt: now.Unix(), IdleSince: ago(time.Hour), LastProbe: ago(time.Hour)}, Keep},
		{"stale status", future, 2 * time.Minute, &HostStatus{UpdatedAt: ago(StatusStale + time.Second), Ready: true, IdleSince: ago(time.Hour), LastProbe: ago(time.Hour)}, Keep},
		{"no expiry", time.Time{}, 2 * time.Minute, idle(time.Hour, time.Hour), IdleStop},
	}
	for _, c := range cases {
		if got := Decide(now, c.expires, c.idleStop, c.st); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHostStatusFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	in := HostStatus{UpdatedAt: 5, IdleSince: 4, LastProbe: 3, Sessions: 1, Ready: true}
	if err := WriteHostStatus(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadHostStatus(p)
	if err != nil || out != in {
		t.Fatalf("read %+v %v", out, err)
	}
}

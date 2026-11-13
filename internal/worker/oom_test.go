package worker

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

func TestParseMemoryEvents(t *testing.T) {
	ev := parseMemoryEvents("low 0\nhigh 12\nmax 3\noom 2\noom_kill 1\noom_group_kill 0\n")
	if ev.High != 12 || ev.Max != 3 || ev.OOM != 2 || ev.OOMKill != 1 {
		t.Fatalf("%+v", ev)
	}
}

func TestExitCause(t *testing.T) {
	kill := int(syscall.SIGKILL)
	for _, c := range []struct {
		name       string
		code, sig  int
		ev         memEvents
		delta      int64
		offshore   string
		client     int
		wantSig    int
		wantOOM    bool
		wantKiller string
	}{
		{"clean exit", 0, 0, memEvents{}, 0, "", 0, 0, false, ""},
		{"cgroup oom", 137, kill, memEvents{OOMKill: 1}, 0, "", 0, kill, true, killerCgroupOOM},
		// prod: a shell wrapper exited 137 for its SIGKILLed child, no
		// signal on the command itself, the kernel's global OOM killer.
		{"wrapper 137 machine oom", 137, 0, memEvents{}, 1, "", 0, kill, true, killerMachineOOM},
		{"oom from cgroup counters beats wrapper", 137, 0, memEvents{OOMKill: 2}, -1, "", 0, kill, true, killerCgroupOOM},
		{"client gone", 137, kill, memEvents{}, 0, killerClient, 0, kill, false, killerClient},
		{"ctrl-c", 130, int(syscall.SIGINT), memEvents{}, 0, "", int(syscall.SIGINT), int(syscall.SIGINT), false, killerClientSignal},
		{"sigkill unknown", 137, kill, memEvents{}, 0, "", 0, kill, false, killerUnknownSIGKILL},
		{"sigterm", 143, int(syscall.SIGTERM), memEvents{}, 0, "", 0, int(syscall.SIGTERM), false, killerSignal},
		{"exit 1", 1, 0, memEvents{Max: 4}, 0, "", 0, 0, false, ""},
	} {
		sig, oom, killer := exitCause(c.code, c.sig, c.ev, false, c.delta, c.offshore, c.client)
		if sig != c.wantSig || oom != c.wantOOM || killer != c.wantKiller {
			t.Errorf("%s: got %d %v %q", c.name, sig, oom, killer)
		}
	}
}

func TestMachineOOMKills(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vmstat")
	os.WriteFile(p, []byte("pgfault 1\noom_kill 7\n"), 0o644)
	old := vmstatPath
	vmstatPath = p
	defer func() { vmstatPath = old }()
	if n := machineOOMKills(); n != 7 {
		t.Fatal(n)
	}
	vmstatPath = p + ".missing"
	if n := machineOOMKills(); n != -1 {
		t.Fatal(n)
	}
}

func TestSamplerReadsCgroup(t *testing.T) {
	dir := t.TempDir()
	w := func(f, s string) { os.WriteFile(filepath.Join(dir, f), []byte(s), 0o644) }
	w("memory.current", "1048576\n")
	w("cpu.stat", "usage_usec 1000000\nuser_usec 1\n")
	w("io.stat", "8:0 rbytes=100 wbytes=200 rios=1\n8:16 rbytes=1 wbytes=2\n")
	w("cpu.pressure", "some avg10=12.50 avg60=0 avg300=0 total=1\nfull avg10=0 avg60=0 avg300=0 total=0\n")
	w("memory.pressure", "some avg10=0.00 avg60=0 avg300=0 total=0\n")
	w("memory.events", "high 3\nmax 0\noom 0\noom_kill 0\n")
	sm := &sampler{dir: dir}
	t0 := time.Unix(1_800_000_000, 0)
	sm.sample(t0)
	w("cpu.stat", "usage_usec 11000000\n")
	s := sm.sample(t0.Add(5 * time.Second))
	if s.CPU != 2 || s.Memory != 1<<20 || s.IORead != 101 || s.IOWrite != 202 || s.PSICPU != 0.125 || s.MemoryHigh != 3 {
		t.Fatalf("%+v", s)
	}
	se := &session{}
	for i := 0; i < maxSessionSamples+3; i++ {
		se.addSample(s)
	}
	if len(se.recentSamples()) != maxSessionSamples {
		t.Fatal(len(se.recentSamples()))
	}
}

func TestSessionMemoryLimit(t *testing.T) {
	const g = int64(1) << 30
	// x8s c8a.2xlarge: 15G, 1.5G headroom.
	if got := sessionMemoryLimit(15 * g); got != 15*g-SessionHeadroom {
		t.Fatalf("15G: %d", got)
	}
	if got := sessionMemoryLimit(2 * g); got != g {
		t.Fatalf("small machine keeps half: %d", got)
	}
	if got := sessionMemoryLimit(0); got != 0 {
		t.Fatalf("unknown: %d", got)
	}
}

func TestMachineMemory(t *testing.T) {
	root := t.TempDir()
	meminfo := filepath.Join(root, "meminfo")
	os.WriteFile(meminfo, []byte("MemTotal:       15728640 kB\nMemFree: 1 kB\n"), 0o644)
	if got := machineMemory(root, meminfo); got != 15728640<<10 {
		t.Fatalf("meminfo: %d", got)
	}
	os.WriteFile(filepath.Join(root, "memory.max"), []byte("max\n"), 0o644)
	if got := machineMemory(root, meminfo); got != 15728640<<10 {
		t.Fatalf("unlimited container: %d", got)
	}
	os.WriteFile(filepath.Join(root, "memory.max"), []byte("8589934592\n"), 0o644)
	if got := machineMemory(root, meminfo); got != 8<<30 {
		t.Fatalf("container limit: %d", got)
	}
}

// Session cgroups are created inside the capped sessions group, so the
// kernel's OOM killer picks a session process, not the worker.
func TestSessionsGroup(t *testing.T) {
	root := t.TempDir()
	dir, err := setupSessionsGroup(root, []string{"+cpu", "+memory"}, 13<<30)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "memory.max")); string(b) != "13958643712" {
		t.Fatalf("memory.max %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control")); string(b) != "+cpu +memory" {
		t.Fatalf("subtree_control %q", b)
	}
	if _, err := setupSessionsGroup(root, nil, 0); err != nil {
		t.Fatalf("reuse: %v", err)
	}
	cg := &cgroups{enabled: true, parent: dir}
	s, err := cg.create("abc", scheduler.Grant{})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(s) != dir {
		t.Fatalf("session cgroup %s outside %s", s, dir)
	}
}

// Production 2026-11-13: cargo clippy exited 101 after a rustc was killed
// by the sessions group's limit; the session cgroup showed no oom_kill.
func TestExitCauseSessionsOOM(t *testing.T) {
	sig, oom, killer := exitCause(101, 0, memEvents{}, true, 0, "", 0)
	if !oom || killer != killerSessionsOOM || sig != 0 {
		t.Fatalf("got %d %v %q", sig, oom, killer)
	}
	if _, oom, _ := exitCause(0, 0, memEvents{}, true, 0, "", 0); oom {
		t.Fatal("a successful session is not OOM killed")
	}
	if _, oom, k := exitCause(137, int(syscall.SIGKILL), memEvents{}, true, 0, killerClient, 0); oom || k != killerClient {
		t.Fatal("offshore's own kill wins")
	}
	if _, oom, k := exitCause(101, 0, memEvents{OOMKill: 1}, true, 0, "", 0); !oom || k != killerCgroupOOM {
		t.Fatal("its own cgroup's counter wins")
	}
}

func TestSessionsOOMDelta(t *testing.T) {
	root := t.TempDir()
	dir, err := setupSessionsGroup(root, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(dir, "memory.events")
	os.WriteFile(ev, []byte("low 0\nhigh 0\nmax 10\noom 1\noom_kill 1\noom_group_kill 0\n"), 0o644)
	cg := &cgroups{enabled: true, parent: dir}
	before := cg.sessionsEvents()
	if sessionsOOM(before, cg.sessionsEvents()) {
		t.Fatal("no change: no OOM")
	}
	os.WriteFile(ev, []byte("low 0\nhigh 0\nmax 30\noom 2\noom_kill 2\noom_group_kill 0\n"), 0o644)
	if !sessionsOOM(before, cg.sessionsEvents()) {
		t.Fatal("oom_kill delta not detected")
	}
	if (&cgroups{enabled: true, parent: cgroupRoot}).sessionsEvents() != (memEvents{}) {
		t.Fatal("no sessions group: no events")
	}
}

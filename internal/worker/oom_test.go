package worker

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
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
		sig, oom, killer := exitCause(c.code, c.sig, c.ev, c.delta, c.offshore, c.client)
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

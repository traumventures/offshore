package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/traumventures/offshore/internal/machine"
)

// BootScript is where a provider's first boot script records its phases
// ("<name> <unix ms>" lines, machine.ParseBootScript). It lives on tmpfs:
// a later boot (a started machine) starts without them.
const BootScript = "/run/offshore/boot-phases"

// BootScriptLine is the shell command a boot script runs to record phase.
func BootScriptLine(phase string) string {
	return "echo " + phase + " $(date +%s%3N) >> " + BootScript
}

func (a *Supervisor) bootScript() string {
	if a.BootScriptPath != "" {
		return a.BootScriptPath
	}
	return BootScript
}

// hostUptime is the time since the kernel started.
func hostUptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	s, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

// startBoot begins the boot phase record of this boot: the kernel start,
// the boot script's phases and the supervisor's start.
func (a *Supervisor) startBoot() {
	up := hostUptime
	if a.Uptime != nil {
		up = a.Uptime
	}
	a.boot = nil
	if d := up(); d > 0 {
		a.boot = append(a.boot, machine.BootPhase{Name: machine.PhaseKernel, At: a.now().Add(-d).UnixMilli()})
	}
	if b, err := os.ReadFile(a.bootScript()); err == nil {
		a.boot = append(a.boot, machine.ParseBootScript(string(b))...)
	}
	a.phase(machine.PhaseSupervisor, 0)
}

// phase records that the boot reached a phase and publishes the record
// to the status directory, where the worker reads it.
func (a *Supervisor) phase(name string, bytes int64) {
	a.boot = append(a.boot, machine.BootPhase{Name: name, At: a.now().UnixMilli(), Bytes: bytes})
	a.Log.Info("boot phase", "phase", name, "bytes", bytes)
	dir := a.Spec.Layout.StatusDir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if err := machine.WriteBoot(filepath.Join(dir, filepath.Base(machine.BootFile)), a.boot); err != nil {
		a.Log.Warn("publishing boot phases", "err", err)
	}
}

// updateBinary installs a pending server binary and executes it
// (reports true when the new binary should take over and it did not
// return an error).
func (a *Supervisor) updateBinary(ctx context.Context) bool {
	u, ok := a.Platform.(BinaryUpdater)
	path := a.Spec.Layout.Binary
	if !ok || path == "" {
		return false
	}
	updated, err := u.UpdateBinary(ctx, path)
	if err != nil {
		a.Log.Warn("updating the server binary, keeping the installed one", "err", err)
	}
	if !updated {
		return false
	}
	a.Log.Info("server binary updated, restarting the supervisor", "path", path)
	exec := a.Exec
	if exec == nil {
		exec = func(p string) error { return syscall.Exec(p, append([]string{p}, os.Args[1:]...), os.Environ()) }
	}
	if err := exec(path); err != nil {
		a.Log.Error("executing the updated binary", "err", err)
		return false
	}
	return true
}

package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProtectedOOMScoreAdj is the oom_score_adj of the supervisor and the
// worker: the kernel never OOM-kills them, so a machine whose sessions
// outgrow its memory loses a session process (a rustc), not its worker
// (incident 2026-11-12: a global OOM left the machine unreachable).
// Sessions raise their own score again (sandbox.SessionOOMScoreAdj).
const ProtectedOOMScoreAdj = -1000

// procDir is /proc (a var for tests).
var procDir = "/proc"

// setOOMScoreAdj sets the oom_score_adj of pid ("self": this process)
// unless it has that value already.
func setOOMScoreAdj(pid string, v int) error {
	p := filepath.Join(procDir, pid, "oom_score_adj")
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(v) {
		return nil
	}
	return os.WriteFile(p, []byte(strconv.Itoa(v)), 0o644)
}

// protectSelf keeps the kernel's OOM killer away from the supervisor.
func (a *Supervisor) protectSelf() {
	if err := setOOMScoreAdj("self", ProtectedOOMScoreAdj); err != nil {
		a.Log.Warn("cannot protect the supervisor from the OOM killer", "err", err)
	}
}

// protectWorker sets the worker container's main process (the worker)
// to ProtectedOOMScoreAdj. It runs after every container start (its pid
// changes on restart) and covers containers created before the layout
// set --oom-score-adj.
func (a *Supervisor) protectWorker(ctx context.Context) error {
	out, err := a.Docker(ctx, nil, nil, "inspect", "--format", "{{.State.Pid}}", a.Spec.Layout.Name())
	if err != nil {
		return err
	}
	pid := strings.TrimSpace(string(out))
	if n, err := strconv.Atoi(pid); err != nil || n <= 0 {
		return fmt.Errorf("worker container not running (pid %q)", pid)
	}
	return setOOMScoreAdj(pid, ProtectedOOMScoreAdj)
}

// Package sandbox starts session processes in their own mount namespace with
// per-session bind mounts (worker-local build directories, dependency
// caches) and an optional cgroup.
//
// The server re-executes its own binary as "offshore sandbox-init" with
// CLONE_NEWNS; the init step performs the mounts and then execs the command.
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// SpecEnv is the environment variable carrying the JSON spec.
const SpecEnv = "OFFSHORE_SANDBOX_SPEC"

// Bind is a bind mount from Src onto Dst.
type Bind struct {
	Src      string `json:"src"`
	Dst      string `json:"dst"`
	ReadOnly bool   `json:"ro,omitempty"`
}

// Spec describes the process to start.
type Spec struct {
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	Binds  []Bind   `json:"binds"`
	Cgroup string   `json:"cgroup,omitempty"` // cgroup directory to join
	// OOMScoreAdj is the session's oom_score_adj (0: inherited). The
	// worker and supervisor run at -1000 (never OOM killed); sessions
	// raise theirs so that the kernel picks a session process.
	OOMScoreAdj int `json:"oom_score_adj,omitempty"`
}

// SessionOOMScoreAdj is the oom_score_adj of session processes: they are
// preferred victims of any OOM kill on the machine.
const SessionOOMScoreAdj = 500

// oomScoreAdjPath is the file setting the process's OOM score (a var for
// tests).
var oomScoreAdjPath = "/proc/self/oom_score_adj"

// setOOMScoreAdj sets the calling process's oom_score_adj. Raising it
// needs no privilege, so a session leaves the worker's -1000 behind.
func setOOMScoreAdj(v int) error {
	return os.WriteFile(oomScoreAdjPath, []byte(strconv.Itoa(v)), 0o644)
}

// Command builds the exec.Cmd that starts spec in a new mount namespace.
func Command(self string, spec Spec) (*exec.Cmd, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "sandbox-init")
	cmd.Env = []string{SpecEnv + "=" + string(b)}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	return cmd, nil
}

// Init runs inside the new namespace. It never returns on success.
func Init() error {
	var spec Spec
	if err := json.Unmarshal([]byte(os.Getenv(SpecEnv)), &spec); err != nil {
		return fmt.Errorf("sandbox: bad spec: %w", err)
	}
	if len(spec.Argv) == 0 {
		return fmt.Errorf("sandbox: empty argv")
	}
	// Keep our mounts private to this namespace.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("sandbox: make mounts private: %w", err)
	}
	for _, b := range spec.Binds {
		if err := os.MkdirAll(b.Src, 0o755); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		if _, err := os.Stat(b.Dst); err != nil {
			if err := os.MkdirAll(b.Dst, 0o755); err != nil {
				return fmt.Errorf("sandbox: mount point %s: %w", b.Dst, err)
			}
		}
		if err := syscall.Mount(b.Src, b.Dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("sandbox: bind %s -> %s: %w", b.Src, b.Dst, err)
		}
		if b.ReadOnly {
			if err := syscall.Mount("", b.Dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_REC, ""); err != nil {
				return fmt.Errorf("sandbox: read-only %s: %w", b.Dst, err)
			}
		}
	}
	if spec.Cgroup != "" {
		if err := os.WriteFile(filepath.Join(spec.Cgroup, "cgroup.procs"), []byte("0"), 0o644); err != nil {
			return fmt.Errorf("sandbox: join cgroup: %w", err)
		}
	}
	if spec.OOMScoreAdj != 0 {
		if err := setOOMScoreAdj(spec.OOMScoreAdj); err != nil {
			return fmt.Errorf("sandbox: oom_score_adj: %w", err)
		}
	}
	if err := os.Chdir(spec.Cwd); err != nil {
		return fmt.Errorf("sandbox: chdir %s: %w", spec.Cwd, err)
	}
	prog := spec.Argv[0]
	if !strings.Contains(prog, "/") {
		for _, kv := range spec.Env {
			if strings.HasPrefix(kv, "PATH=") {
				os.Setenv("PATH", kv[5:])
			}
		}
		p, err := exec.LookPath(prog)
		if err != nil {
			return fmt.Errorf("%s: command not found", prog)
		}
		prog = p
	}
	return syscall.Exec(prog, spec.Argv, spec.Env)
}

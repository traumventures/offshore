package server

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const cgroupRoot = "/sys/fs/cgroup"

// cgroups manages per-session cgroups (cgroup v2). It is best effort: when
// cgroups are not writable sessions run without limits.
type cgroups struct {
	enabled bool
}

func setupCgroups(log *slog.Logger) *cgroups {
	cg := &cgroups{}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		log.Warn("cgroup v2 not available, sessions run without resource limits")
		return cg
	}
	if !writable(cgroupRoot) {
		// Docker mounts the cgroup namespace read-only; with CAP_SYS_ADMIN we
		// can remount it read-write.
		if err := syscall.Mount("", cgroupRoot, "", syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC|syscall.MS_RELATIME, ""); err != nil {
			log.Warn("cgroup filesystem is read-only, sessions run without resource limits", "err", err)
			return cg
		}
	}
	// cgroup v2 forbids processes in inner nodes: move everything in the
	// root group into a leaf before enabling controllers.
	leaf := filepath.Join(cgroupRoot, "cloudsourced-server")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		log.Warn("cannot create cgroup", "err", err)
		return cg
	}
	procs, _ := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.procs"))
	for _, pid := range strings.Fields(string(procs)) {
		os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(pid), 0o644)
	}
	ctrls, _ := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	var enable []string
	for _, c := range strings.Fields(string(ctrls)) {
		if c == "cpu" || c == "memory" || c == "pids" {
			enable = append(enable, "+"+c)
		}
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0o644); err != nil {
		log.Warn("cannot enable cgroup controllers", "err", err)
		return cg
	}
	cg.enabled = true
	log.Info("cgroups enabled", "controllers", enable)
	return cg
}

func writable(dir string) bool {
	return syscall.Access(dir, 2) == nil && func() bool {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return false
		}
		return st.Flags&1 == 0 // ST_RDONLY
	}()
}

// create makes a session cgroup with limits and returns its path.
func (c *cgroups) create(id string, cpu float64, memory int64) (string, error) {
	if !c.enabled {
		return "", nil
	}
	dir := filepath.Join(cgroupRoot, "session-"+id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	if memory > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(memory, 10)), 0o644); err != nil {
			return dir, fmt.Errorf("memory.max: %w", err)
		}
		os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("0"), 0o644)
	}
	if cpu > 0 {
		// Proportional sharing: idle CPU stays usable by every session.
		w := int(cpu * 100)
		if w < 1 {
			w = 1
		}
		if w > 10000 {
			w = 10000
		}
		os.WriteFile(filepath.Join(dir, "cpu.weight"), []byte(strconv.Itoa(w)), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "pids.max"), []byte("32768"), 0o644)
	return dir, nil
}

// kill terminates all processes in a session cgroup.
func (c *cgroups) kill(dir string) {
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		// Older kernels: kill each process.
		procs, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
		for _, p := range strings.Fields(string(procs)) {
			if pid, err := strconv.Atoi(p); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}

// remove kills remaining processes and removes the cgroup.
func (c *cgroups) remove(dir string) {
	if dir == "" {
		return
	}
	c.kill(dir)
	for i := 0; i < 50; i++ {
		if err := os.Remove(dir); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// oomKilled reports whether the memory limit killed a process.
func (c *cgroups) oomKilled(dir string) bool {
	if dir == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "oom_kill" && f[1] != "0" {
			return true
		}
	}
	return false
}

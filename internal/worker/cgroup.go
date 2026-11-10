package worker

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/traumventures/cloudsourced/internal/scheduler"
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
	leaf := filepath.Join(cgroupRoot, "cloudsourced-worker")
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

// create makes a session cgroup with the limits of a grant and returns
// its path.
func (c *cgroups) create(id string, g scheduler.Grant) (string, error) {
	if !c.enabled {
		return "", nil
	}
	dir := filepath.Join(cgroupRoot, "session-"+id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	write := func(file, v string) error {
		return os.WriteFile(filepath.Join(dir, file), []byte(v), 0o644)
	}
	if g.MemoryMax > 0 {
		if err := write("memory.max", strconv.FormatInt(g.MemoryMax, 10)); err != nil {
			return dir, fmt.Errorf("memory.max: %w", err)
		}
	}
	if g.MemoryHigh > 0 {
		if err := write("memory.high", strconv.FormatInt(g.MemoryHigh, 10)); err != nil {
			return dir, fmt.Errorf("memory.high: %w", err)
		}
	}
	switch {
	case g.Swap < 0:
		write("memory.swap.max", "max")
	default:
		write("memory.swap.max", strconv.FormatInt(g.Swap, 10))
	}
	if g.CPUWeight > 0 {
		// Proportional sharing: idle CPU stays usable by every session.
		w := min(max(int(g.CPUWeight*100), 1), 10000)
		write("cpu.weight", strconv.Itoa(w))
	}
	write("pids.max", "32768")
	return dir, nil
}

// usage reads what a session cgroup consumed.
func (c *cgroups) usage(dir string) (peak int64, cpuSeconds float64) {
	if dir == "" {
		return 0, 0
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.peak")); err == nil {
		peak, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.stat")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "usage_usec" {
				us, _ := strconv.ParseFloat(f[1], 64)
				cpuSeconds = us / 1e6
			}
		}
	}
	return peak, cpuSeconds
}

// current returns the memory currently used by a session cgroup.
func (c *cgroups) current(dir string) int64 {
	if dir == "" {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// stall reads the PSI "some total" of a session cgroup: seconds at least
// one of its tasks waited for CPU or memory.
func (c *cgroups) stall(dir string) (cpu, memory float64) {
	if dir == "" {
		return 0, 0
	}
	return psiTotal(filepath.Join(dir, "cpu.pressure")), psiTotal(filepath.Join(dir, "memory.pressure"))
}

func psiTotal(file string) float64 {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, kv := range strings.Fields(line)[1:] {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				us, _ := strconv.ParseFloat(v, 64)
				return us / 1e6
			}
		}
	}
	return 0
}

// cpuSeconds returns the CPU time of everything on the machine (the
// worker's cgroup, sessions included; /proc/stat without cgroups).
func cpuSeconds() (float64, bool) {
	if b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.stat")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "usage_usec" {
				us, _ := strconv.ParseFloat(f[1], 64)
				return us / 1e6, true
			}
		}
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 4 || f[0] != "cpu" {
		return 0, false
	}
	var ticks float64
	for i, v := range f[1:] {
		if i == 3 || i == 4 { // idle, iowait
			continue
		}
		n, _ := strconv.ParseFloat(v, 64)
		ticks += n
	}
	return ticks / 100, true // USER_HZ
}

// pressure reads PSI "some avg10" of the machine for cpu and memory.
func pressure() scheduler.Pressure {
	read := func(res string) float64 {
		b, err := os.ReadFile(filepath.Join(cgroupRoot, res+".pressure"))
		if err != nil {
			b, err = os.ReadFile(filepath.Join("/proc/pressure", res))
			if err != nil {
				return 0
			}
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "some ") {
				continue
			}
			for _, kv := range strings.Fields(line)[1:] {
				if v, ok := strings.CutPrefix(kv, "avg10="); ok {
					f, _ := strconv.ParseFloat(v, 64)
					return f / 100
				}
			}
		}
		return 0
	}
	return scheduler.Pressure{CPU: read("cpu"), Memory: read("memory")}
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

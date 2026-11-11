package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/hostfs"
)

// localDockerCommand returns the Docker CLI of the embedded pool's first
// local-docker provider (nil: none).
func localDockerCommand(fc *config.PoolConfig) []string {
	for _, name := range fc.ProviderNames() {
		if fc.ProviderType(name) != "local-docker" {
			continue
		}
		cmd := []string{"docker"}
		if raw, ok := fc.Providers[name]["command"].([]any); ok && len(raw) > 0 {
			cmd = cmd[:0]
			for _, a := range raw {
				cmd = append(cmd, fmt.Sprint(a))
			}
		}
		return cmd
	}
	return nil
}

// dockerRootDir asks the Docker daemon where it keeps its volumes (the
// local-docker machines' caches); default /var/lib/docker.
func dockerRootDir(ctx context.Context, cmd []string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := append(append([]string{}, cmd[1:]...), "info", "--format", "{{.DockerRootDir}}")
	if out, err := exec.CommandContext(ctx, cmd[0], args...).Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			return dir
		}
	}
	return "/var/lib/docker"
}

// runCacheFSCheck checks the file system of the local-docker machines'
// cache volumes (in embedded mode with a local-docker provider).
func runCacheFSCheck(ctx context.Context, client *config.Client) check {
	c := check{Name: "cache medium", Status: checkSkip}
	if client == nil || !client.Embedded() {
		c.Detail = "the control plane's machines report their cache medium (offshore_worker_cache_fs_ok)"
		return c
	}
	fc, err := config.LoadPool(config.PoolConfigPath())
	if err != nil {
		c.Detail = "pool config unreadable"
		return c
	}
	cmd := localDockerCommand(fc)
	if cmd == nil {
		c.Detail = "no local-docker provider"
		return c
	}
	if h := os.Getenv("DOCKER_HOST"); h != "" && !strings.HasPrefix(h, "unix://") {
		c.Detail = "Docker runs on " + h + ": its disk is not inspectable from here"
		return c
	}
	dir := dockerRootDir(ctx, cmd)
	// The reflink probe writes two temporary files: only where we may.
	probe := unix.Access(dir, unix.W_OK) == nil
	info, err := hostfs.Inspect(dir, probe)
	return cacheFSCheck(info, err, probe)
}

// cacheFSCheck judges the local-docker cache medium. local-docker
// accepts any file system; xfs with reflink is what cloud machines use
// and what makes session snapshots cheap.
func cacheFSCheck(info hostfs.Info, err error, reflinkProbed bool) check {
	c := check{Name: "cache medium"}
	if err != nil {
		c.Status, c.Detail = checkSkip, err.Error()
		return c
	}
	reflink := "reflink"
	switch {
	case !reflinkProbed && (info.Type == "xfs" || info.Type == "btrfs"):
		reflink = "reflink not probed (no write access; xfs formatted since xfsprogs 5.1 has it)"
		info.Reflink = true // judged by the type alone
	case !info.Reflink:
		reflink = "no reflink"
	}
	c.Detail = fmt.Sprintf("Docker volumes in %s: %s, %s", info.Path, info.Type, reflink)
	var warns []string
	if !info.Reflink {
		warns = append(warns, "session snapshots copy changed files instead of cloning them (above 256 MiB the idle commit takes the session): put Docker's data root on xfs (mkfs.xfs -m reflink=1) for cloud-like behaviour")
	}
	if info.NoAutoDaAlloc() {
		warns = append(warns, "ext4 is mounted with noauto_da_alloc: files replaced by rename are not flushed first, so cache files written without fsync can be empty after a crash (offshore fsyncs its commit metadata and drops torn cache files at start); remove noauto_da_alloc from the mount options")
	}
	if len(warns) == 0 {
		c.Status = checkOK
		return c
	}
	c.Status, c.Hint = checkWarn, strings.Join(warns, "; ")
	return c
}

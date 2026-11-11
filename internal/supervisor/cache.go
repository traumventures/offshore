package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/hostfs"
)

// CacheSpec says where the machine's cache directory lives on the host
// (Spec.Cache; nil: a Docker volume on the root disk).
type CacheSpec struct {
	// Medium is "local" or "volume".
	Medium string `json:"medium"`
	// Device is the stable device path of the cache volume (medium volume).
	Device string `json:"device,omitempty"`
	// LocalDevices is a glob of instance-store NVMe devices (medium local);
	// when one exists the cache lives on it instead of the root disk.
	LocalDevices string `json:"local_devices,omitempty"`
	// TTLSeconds: content of a volume unused for longer is wiped when it
	// is attached (0: never).
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// Cache media.
const (
	CacheLocal  = "local"
	CacheVolume = "volume"
)

// cacheMarker records when a cache volume was last used (on the volume).
const cacheMarker = ".offshore-cache.json"

type cacheUse struct {
	Used int64 `json:"used"` // unix seconds
}

// HostRunner runs a command on the host.
type HostRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execHost(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (a *Supervisor) host() HostRunner {
	if a.Host != nil {
		return a.Host
	}
	return execHost
}

// cacheDir is where a cache device is mounted.
func (a *Supervisor) cacheDir() string { return filepath.Join(a.StateDir, "cache") }

func (a *Supervisor) bootID() string {
	if a.BootID != nil {
		return a.BootID()
	}
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// cacheDevice is the device the cache lives on ("" for the root disk)
// and whether it is wiped on every boot (instance store).
func (a *Supervisor) cacheDevice(ctx context.Context) (dev string, ephemeral bool, err error) {
	c := a.Spec.Cache
	switch {
	case c == nil:
		return "", false, nil
	case c.Medium == CacheVolume:
		// The volume is attached right after the instance starts: wait.
		for i := 0; ; i++ {
			if _, err := os.Stat(c.Device); err == nil {
				return c.Device, false, nil
			}
			if i >= 150 || ctx.Err() != nil {
				return "", false, fmt.Errorf("cache volume %s not attached", c.Device)
			}
			a.sleep(ctx, 2*time.Second)
		}
	case c.LocalDevices != "":
		devs, _ := filepath.Glob(c.LocalDevices)
		sort.Strings(devs)
		if len(devs) > 0 {
			return devs[0], true, nil
		}
	}
	return "", false, nil
}

// PrepareCache mounts the cache medium at the cache directory and points
// the layout's cache volume at it. An instance-store disk is wiped and
// formatted (xfs with reflink) once per boot, a volume formatted when it
// is blank; a volume unused for longer than its TTL is emptied. Without
// a cache device the layout keeps its Docker volume.
func (a *Supervisor) PrepareCache(ctx context.Context) error {
	dev, ephemeral, err := a.cacheDevice(ctx)
	if err != nil || dev == "" {
		return err
	}
	run := a.host()
	dir := a.cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mounted := false
	if out, err := run(ctx, "findmnt", "-n", "-o", "SOURCE", dir); err == nil && strings.TrimSpace(string(out)) != "" {
		mounted = true
	}
	bootFile := filepath.Join(a.StateDir, "cache-boot")
	boot := a.bootID()
	if ephemeral {
		last, _ := os.ReadFile(bootFile)
		if strings.TrimSpace(string(last)) != boot || !mounted {
			if mounted {
				if out, err := run(ctx, "umount", dir); err != nil {
					return fmt.Errorf("umount %s: %v: %s", dir, err, out)
				}
				mounted = false
			}
			a.Log.Info("formatting the instance-store disk for the cache", "device", dev)
			if err := a.mkfs(ctx, dev, true); err != nil {
				return err
			}
			if err := writeFileSync(bootFile, []byte(boot+"\n"), 0o600); err != nil {
				return err
			}
		}
	} else if !mounted {
		out, _ := run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
		switch typ := strings.TrimSpace(string(out)); typ {
		case "":
			a.Log.Info("formatting the new cache volume", "device", dev)
			if err := a.mkfs(ctx, dev, false); err != nil {
				return err
			}
		case "xfs":
		default:
			// The volume is a cache: a foreign file system (ext4 from a
			// manual format) would break snapshot capture, so it is
			// replaced.
			a.Log.Warn("cache volume is not xfs, reformatting it (its content is lost)", "device", dev, "type", typ)
			if err := a.mkfs(ctx, dev, true); err != nil {
				return err
			}
		}
	}
	if !mounted {
		if out, err := run(ctx, "mount", "-o", "noatime", dev, dir); err != nil {
			return fmt.Errorf("mount %s at %s: %v: %s", dev, dir, err, out)
		}
	}
	if !ephemeral {
		if err := a.expireCache(dir); err != nil {
			return err
		}
		a.GrowCache(ctx)
	}
	a.checkCacheFS(dir)
	a.Spec.Layout.CacheVolume = dir
	return nil
}

// checkCacheFS logs when the mounted cache medium is not xfs with
// reflink (an old xfs formatted without it, a mount that failed over to
// another device); the worker reports the same in its status.
func (a *Supervisor) checkCacheFS(dir string) {
	info, err := a.FSProbes.Inspect(dir, true)
	if err != nil {
		a.Log.Warn("cache medium not inspectable", "dir", dir, "err", err)
		return
	}
	for _, p := range hostfs.Check(info, a.Spec.CacheFSPolicy()).Problems {
		a.Log.Error("cache medium breaks the machine policy", "dir", dir, "problem", p)
	}
}

func (a *Supervisor) mkfs(ctx context.Context, dev string, force bool) error {
	args := []string{"-m", "reflink=1"}
	if force {
		args = append(args, "-f")
	}
	if out, err := a.host()(ctx, "mkfs.xfs", append(args, dev)...); err != nil {
		return fmt.Errorf("mkfs.xfs %s: %v: %s", dev, err, out)
	}
	return nil
}

// expireCache empties a cache volume last used longer than its TTL ago
// and marks it used now.
func (a *Supervisor) expireCache(dir string) error {
	ttl := time.Duration(a.Spec.Cache.TTLSeconds) * time.Second
	marker := filepath.Join(dir, cacheMarker)
	var u cacheUse
	if b, err := os.ReadFile(marker); err == nil {
		json.Unmarshal(b, &u)
	}
	if ttl > 0 && u.Used > 0 && a.now().Sub(time.Unix(u.Used, 0)) > ttl {
		a.Log.Info("cache volume unused for longer than its ttl, wiping it", "last_used", time.Unix(u.Used, 0), "ttl", ttl)
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		var errs []error
		for _, e := range ents {
			if e.Name() != cacheMarker && e.Name() != "lost+found" {
				errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
			}
		}
		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("wiping the expired cache: %w", err)
		}
	}
	a.touched = time.Time{}
	return a.touchCache()
}

// touchCache marks the cache volume used now (at most every 10 minutes).
func (a *Supervisor) touchCache() error {
	if a.Spec.Cache == nil || a.Spec.Cache.Medium != CacheVolume || a.now().Sub(a.touched) < 10*time.Minute {
		return nil
	}
	a.touched = a.now()
	b, _ := json.Marshal(cacheUse{Used: a.now().Unix()})
	return writeFileSync(filepath.Join(a.cacheDir(), cacheMarker), b, 0o644)
}

// GrowCache grows the cache volume's file system to its device after a
// resize: the device is rescanned (paravirtualized and SCSI attachments
// do not notice by themselves) and xfs_growfs grows online (a no-op when
// the size is unchanged).
func (a *Supervisor) GrowCache(ctx context.Context) error {
	c := a.Spec.Cache
	if c == nil || c.Medium != CacheVolume {
		return nil
	}
	if real, err := filepath.EvalSymlinks(c.Device); err == nil {
		rescan := filepath.Join(a.sysBlock(), filepath.Base(real), "device", "rescan")
		if _, err := os.Stat(rescan); err == nil {
			os.WriteFile(rescan, []byte("1"), 0o200)
		}
	}
	if out, err := a.host()(ctx, "xfs_growfs", a.cacheDir()); err != nil {
		return fmt.Errorf("xfs_growfs: %v: %s", err, out)
	}
	return nil
}

func (a *Supervisor) sysBlock() string {
	if a.SysBlock != "" {
		return a.SysBlock
	}
	return "/sys/class/block"
}

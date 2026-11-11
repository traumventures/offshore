package anyfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// KernelOverlayOptions are the overlayfs options of slot mounts:
// redirect_dir lets renamed lower directories work (rustc renames
// incremental session directories), metacopy makes copy-up of metadata
// changes (chmod, utimes, hardlinks of uplifted artifacts) metadata only
// instead of copying whole files (the restore killer without it).
const KernelOverlayOptions = "redirect_dir=on,metacopy=on"

// MountKernelOverlay mounts an overlayfs at target: upper and work (on
// the same local file system, not itself an overlay) over the lower
// directories (top first). It needs CAP_SYS_ADMIN.
func MountKernelOverlay(target, upper, work string, lowers ...string) error {
	return MountKernelOverlayData(target, upper, work, lowers, nil)
}

// MountKernelOverlayData is MountKernelOverlay with data-only lower
// layers below lowers (searched in order for the content of metacopy
// files with an absolute redirect; never listed or looked up by path).
func MountKernelOverlayData(target, upper, work string, lowers, data []string) error {
	if len(lowers) == 0 {
		return errors.New("anyfs: overlay without lower directory")
	}
	for _, d := range []string{target, upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for _, l := range append(append([]string(nil), lowers...), data...) {
		if strings.ContainsAny(l, ",:") {
			return fmt.Errorf("anyfs: overlay lower %q contains ',' or ':'", l)
		}
	}
	ld := strings.Join(lowers, ":")
	for _, d := range data {
		ld += "::" + d
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,%s", ld, upper, work, KernelOverlayOptions)
	if err := unix.Mount("overlay", target, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay at %s: %w", target, err)
	}
	return nil
}

// Unmount unmounts a mount point, lazily (detached) if it is busy.
func Unmount(target string) error {
	err := unix.Unmount(target, 0)
	if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return unix.Unmount(target, unix.MNT_DETACH)
}

var (
	probeMu sync.Mutex
	probed  = map[string]error{}
)

// KernelOverlaySupported reports whether this process can mount a kernel
// overlayfs with KernelOverlayOptions whose upper directory lives below
// base (probed once per base by mounting a tiny overlay).
func KernelOverlaySupported(base string) error {
	probeMu.Lock()
	defer probeMu.Unlock()
	if err, ok := probed[base]; ok {
		return err
	}
	err := probeOverlay(base)
	probed[base] = err
	return err
}

func probeOverlay(base string) error {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(base, ".overlay-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	t := filepath.Join(dir, "m")
	if err := MountKernelOverlay(t, filepath.Join(dir, "u"), filepath.Join(dir, "w"), mkdir(filepath.Join(dir, "l"))); err != nil {
		return err
	}
	return Unmount(t)
}

func mkdir(d string) string {
	os.MkdirAll(d, 0o755)
	return d
}

// IsMountpoint reports whether p is a mount point (its device differs
// from its parent's).
func IsMountpoint(p string) bool {
	var a, b unix.Stat_t
	if unix.Lstat(p, &a) != nil || unix.Lstat(filepath.Dir(p), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}
